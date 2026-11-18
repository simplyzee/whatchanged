// Package store keeps changes in memory, ordered by time, with optional
// append-only JSONL persistence and time-based retention.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

const fileName = "changes.jsonl"

// Options configures a Store.
type Options struct {
	// Dir is where changes.jsonl lives. Empty means memory only.
	Dir string
	// Retention drops changes older than this. Zero means keep forever.
	Retention time.Duration
	// MaxItems caps memory use; the oldest changes are dropped first.
	MaxItems int
	// Now is overridable for tests.
	Now func() time.Time
}

// Store is safe for concurrent use.
type Store struct {
	opts  Options
	mu    sync.RWMutex
	items []change.Change // sorted by Time ascending
	ids   map[string]struct{}
	file  *os.File
	hooks []func(change.Change)
}

// Open creates a store, loading any persisted changes from opts.Dir.
func Open(opts Options) (*Store, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = 250_000
	}
	s := &Store{opts: opts, ids: map[string]struct{}{}}
	if opts.Dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(opts.Dir, fileName)
	dropped, err := s.load(path)
	if err != nil {
		return nil, err
	}
	if dropped {
		if err := s.rewrite(path); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s.file = f
	return s, nil
}

func (s *Store) load(path string) (dropped bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	cutoff := s.cutoff()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var c change.Change
			if jerr := json.Unmarshal(line, &c); jerr != nil || c.ID == "" {
				dropped = true // corrupt or truncated line
			} else if _, dup := s.ids[c.ID]; dup || (!cutoff.IsZero() && c.Time.Before(cutoff)) {
				dropped = true
			} else {
				s.insert(c)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("read %s: %w", path, err)
		}
	}
	if s.trimToMax() {
		dropped = true
	}
	return dropped, nil
}

// rewrite atomically replaces the file with the current in-memory contents.
func (s *Store) rewrite(path string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, c := range s.items {
		if err := enc.Encode(c); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) cutoff() time.Time {
	if s.opts.Retention <= 0 {
		return time.Time{}
	}
	return s.opts.Now().Add(-s.opts.Retention)
}

// insert keeps items sorted by time. Callers hold the lock.
func (s *Store) insert(c change.Change) {
	i := sort.Search(len(s.items), func(i int) bool { return s.items[i].Time.After(c.Time) })
	s.items = slices.Insert(s.items, i, c)
	s.ids[c.ID] = struct{}{}
}

func (s *Store) trimToMax() bool {
	over := len(s.items) - s.opts.MaxItems
	if over <= 0 {
		return false
	}
	for _, c := range s.items[:over] {
		delete(s.ids, c.ID)
	}
	s.items = slices.Clone(s.items[over:])
	return true
}

// OnAdd registers a hook called (outside the lock) for every new change.
func (s *Store) OnAdd(fn func(change.Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, fn)
}

// Add stores a change. It returns false if the change was a duplicate or
// already outside the retention window.
func (s *Store) Add(c change.Change) (bool, error) {
	if c.ID == "" {
		return false, errors.New("change has no id")
	}
	s.mu.Lock()
	if _, dup := s.ids[c.ID]; dup {
		s.mu.Unlock()
		return false, nil
	}
	if cut := s.cutoff(); !cut.IsZero() && c.Time.Before(cut) {
		s.mu.Unlock()
		return false, nil
	}
	s.insert(c)
	s.trimToMax()
	var werr error
	if s.file != nil {
		b, err := json.Marshal(c)
		if err == nil {
			_, err = s.file.Write(append(b, '\n'))
		}
		werr = err
	}
	hooks := slices.Clone(s.hooks)
	s.mu.Unlock()
	for _, h := range hooks {
		h(c)
	}
	return true, werr
}

// Prune drops changes outside the retention window and compacts the file.
func (s *Store) Prune() (int, error) {
	cut := s.cutoff()
	if cut.IsZero() {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := sort.Search(len(s.items), func(i int) bool { return !s.items[i].Time.Before(cut) })
	if n == 0 {
		return 0, nil
	}
	for _, c := range s.items[:n] {
		delete(s.ids, c.ID)
	}
	s.items = slices.Clone(s.items[n:])
	if s.file == nil {
		return n, nil
	}
	path := filepath.Join(s.opts.Dir, fileName)
	if err := s.file.Close(); err != nil {
		return n, err
	}
	if err := s.rewrite(path); err != nil {
		return n, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		s.file = nil
		return n, err
	}
	s.file = f
	return n, nil
}

// Len returns the number of stored changes.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// Close flushes and closes the backing file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Filter selects changes. Zero values mean "no constraint".
type Filter struct {
	Since      time.Time
	Until      time.Time
	Namespaces []string
	Kinds      []string
	Sources    []string
	Actions    []string
	MinImpact  change.Impact
	Flags      []string // any of
	Text       string
	NoSignals  bool
	Limit      int
}

func matchAny(v string, set []string) bool {
	if len(set) == 0 {
		return true
	}
	for _, s := range set {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// Match reports whether c passes the filter.
func (f Filter) Match(c change.Change) bool {
	if !f.Since.IsZero() && c.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && c.Time.After(f.Until) {
		return false
	}
	if !matchAny(c.Namespace, f.Namespaces) || !matchAny(c.Kind, f.Kinds) ||
		!matchAny(c.Source, f.Sources) || !matchAny(c.Action, f.Actions) {
		return false
	}
	if f.MinImpact != "" && !c.Impact.AtLeast(f.MinImpact) {
		return false
	}
	if f.NoSignals && c.IsSignal() {
		return false
	}
	if len(f.Flags) > 0 {
		hit := false
		for _, fl := range f.Flags {
			if c.HasFlag(fl) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if f.Text != "" {
		q := strings.ToLower(f.Text)
		hay := strings.ToLower(strings.Join([]string{c.Name, c.Summary, c.Actor, c.Kind, c.Namespace}, " "))
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// Query returns matching changes, newest first.
func (s *Store) Query(f Filter) []change.Change {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []change.Change{}
	for i := len(s.items) - 1; i >= 0; i-- {
		c := s.items[i]
		if !f.Until.IsZero() && c.Time.After(f.Until) {
			continue
		}
		if !f.Since.IsZero() && c.Time.Before(f.Since) {
			break
		}
		if !f.Match(c) {
			continue
		}
		out = append(out, c)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out
}

// ErrAmbiguous is returned by Get when an ID prefix matches several changes.
var ErrAmbiguous = errors.New("id prefix matches more than one change")

// Get returns the change with the given ID, or the single change whose ID
// starts with the given prefix (at least 6 characters).
func (s *Store) Get(id string) (change.Change, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.ids[id]; ok {
		for i := len(s.items) - 1; i >= 0; i-- {
			if s.items[i].ID == id {
				return s.items[i], true, nil
			}
		}
	}
	if len(id) < 6 {
		return change.Change{}, false, nil
	}
	var found *change.Change
	for i := range s.items {
		if strings.HasPrefix(s.items[i].ID, id) {
			if found != nil {
				return change.Change{}, false, ErrAmbiguous
			}
			found = &s.items[i]
		}
	}
	if found == nil {
		return change.Change{}, false, nil
	}
	return *found, true, nil
}
