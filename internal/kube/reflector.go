package kube

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Handler receives the state of a collection.
type Handler interface {
	// Replace is called with the full list after every (re)list. initial is
	// true only for the very first list.
	Replace(items []json.RawMessage, initial bool)
	// Apply is called for each ADDED, MODIFIED or DELETED event.
	Apply(ev Event)
}

// ReflectOptions tunes Reflect.
type ReflectOptions struct {
	Logger *slog.Logger
	// OnSynced is called once, after the first successful list.
	OnSynced func()
	// OnRestart is called each time the watch has to be re-established.
	OnRestart func()
	// MinWatch/MaxWatch bound the randomised server-side watch timeout.
	MinWatch, MaxWatch time.Duration
}

// Reflect lists and then watches r, feeding h, until ctx is cancelled. If
// the very first list fails with 404 (resource type not installed) or 403
// (no RBAC), that error is returned so the caller can decide what to do.
func Reflect(ctx context.Context, c *Client, r Resource, h Handler, o ReflectOptions) error {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	if o.MinWatch <= 0 {
		o.MinWatch = 5 * time.Minute
	}
	if o.MaxWatch < o.MinWatch {
		o.MaxWatch = 2 * o.MinWatch
	}
	restart := func() {
		if o.OnRestart != nil {
			o.OnRestart()
		}
	}
	initial := true
	backoff := newBackoff()
	for ctx.Err() == nil {
		list, err := c.List(ctx, r)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if initial && (IsNotFound(err) || IsForbidden(err)) {
				return err
			}
			log.Warn("list failed", "path", r.Path, "err", err)
			backoff.wait(ctx)
			continue
		}
		backoff.reset()
		h.Replace(list.Items, initial)
		if initial {
			initial = false
			if o.OnSynced != nil {
				o.OnSynced()
			}
		}
		rv := list.ResourceVersion

		// Watch until the server tells us our resourceVersion is too old.
		for ctx.Err() == nil {
			timeout := o.MinWatch + time.Duration(rand.Int64N(int64(o.MaxWatch-o.MinWatch)+1))
			err := c.Watch(ctx, r, rv, timeout, func(ev Event) error {
				var m struct {
					Metadata struct {
						ResourceVersion string `json:"resourceVersion"`
					} `json:"metadata"`
				}
				if json.Unmarshal(ev.Object, &m) == nil && m.Metadata.ResourceVersion != "" {
					rv = m.Metadata.ResourceVersion
				}
				if ev.Type == "BOOKMARK" {
					return nil
				}
				h.Apply(ev)
				backoff.reset()
				return nil
			})
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				continue // normal server-side timeout; resume from rv
			}
			restart()
			if IsGone(err) {
				log.Debug("watch expired, relisting", "path", r.Path)
				break
			}
			log.Warn("watch failed", "path", r.Path, "err", err)
			backoff.wait(ctx)
			if backoff.attempts > 3 {
				break // something is off; a relist is cheap insurance
			}
		}
	}
	return nil
}

type backoffState struct {
	d        time.Duration
	attempts int
}

func newBackoff() *backoffState { return &backoffState{d: time.Second} }

func (b *backoffState) reset() { b.d, b.attempts = time.Second, 0 }

func (b *backoffState) wait(ctx context.Context) {
	b.attempts++
	jitter := time.Duration(rand.Int64N(int64(b.d / 2)))
	t := time.NewTimer(b.d + jitter)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	if b.d < 30*time.Second {
		b.d *= 2
	}
}
