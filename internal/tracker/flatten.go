package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DiffMode controls whether and how object diffs are recorded.
type DiffMode string

const (
	// DiffsOff records no diffs.
	DiffsOff DiffMode = "off"
	// DiffsRedacted records diffs but hides values that commonly hold
	// secrets: env var values, ConfigMap data and anything whose key looks
	// like a password, token or key. Hidden values show as a short hash, so
	// you can still see that they changed.
	DiffsRedacted DiffMode = "redacted"
	// DiffsFull records diffs with values (Secrets are never diffed).
	DiffsFull DiffMode = "full"
)

// ParseDiffMode validates a diff mode.
func ParseDiffMode(s string) (DiffMode, error) {
	switch DiffMode(s) {
	case DiffsOff, DiffsRedacted, DiffsFull:
		return DiffMode(s), nil
	case "":
		return DiffsRedacted, nil
	}
	return "", fmt.Errorf("invalid diff mode %q (want off, redacted or full)", s)
}

const (
	maxDiffLines = 120
	maxFlatLines = 3000
	maxValueLen  = 160
)

var sensitiveKey = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[-_]?key|private[-_]?key|credential|auth|cert|dsn|conn(ection)?[-_]?str)`)

// skipped metadata: noisy, huge or meaningless in a diff.
var skipPaths = []string{
	"metadata.managedFields", "metadata.resourceVersion", "metadata.generation",
	"metadata.uid", "metadata.creationTimestamp", "metadata.selfLink",
	"metadata.ownerReferences", "metadata.finalizers",
	"metadata.annotations.kubectl.kubernetes.io/last-applied-configuration",
	"metadata.annotations.deployment.kubernetes.io/revision",
	"status",
}

func skipped(path string) bool {
	for _, p := range skipPaths {
		if path == p || strings.HasPrefix(path, p+".") || strings.HasPrefix(path, p+"[") {
			return true
		}
	}
	return false
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// redactedByDefault holds paths hidden in redacted mode for every kind.
func redactedByDefault(path string) bool {
	last := path
	if i := strings.LastIndexAny(path, ".["); i >= 0 {
		last = path[i+1:]
	}
	last = strings.TrimSuffix(last, "]")
	switch {
	case strings.HasSuffix(path, ".value") && strings.Contains(path, ".env["):
		return true // env var values
	case strings.HasPrefix(path, "data.") || strings.HasPrefix(path, "binaryData.") || strings.HasPrefix(path, "stringData."):
		return true
	case strings.Contains(path, ".caBundle") || strings.HasSuffix(path, "caBundle"):
		return true
	case sensitiveKey.MatchString(last):
		return true
	}
	return false
}

// flattenObject turns an object into "path: value" lines.
func flattenObject(raw json.RawMessage, mode DiffMode, redact func(string) bool) map[string]string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		delete(m, "apiVersion")
		delete(m, "kind")
	}
	out := map[string]string{}
	var walk func(path string, v any) bool
	walk = func(path string, v any) bool {
		if len(out) > maxFlatLines {
			return false
		}
		if path != "" && skipped(path) {
			return true
		}
		switch t := v.(type) {
		case map[string]any:
			if len(t) == 0 {
				out[path] = "{}"
				return true
			}
			for k, sub := range t {
				p := k
				if path != "" {
					p = path + "." + k
				}
				if !walk(p, sub) {
					return false
				}
			}
		case []any:
			if len(t) == 0 {
				out[path] = "[]"
				return true
			}
			for i, sub := range t {
				label := strconv.Itoa(i)
				if m, ok := sub.(map[string]any); ok {
					if n, ok := m["name"].(string); ok && n != "" {
						label = n
					}
				}
				if !walk(path+"["+label+"]", sub) {
					return false
				}
			}
		default:
			b, _ := json.Marshal(t)
			val := string(b)
			if mode == DiffsRedacted && (redactedByDefault(path) || (redact != nil && redact(path))) {
				val = "<redacted " + shortHash(val) + ">"
			} else if len(val) > maxValueLen {
				val = val[:maxValueLen] + "… <" + shortHash(val) + ">"
			}
			out[path] = val
		}
		return true
	}
	if !walk("", v) {
		return nil // too big to diff usefully
	}
	return out
}

// diffFlat renders the differences between two flattened objects.
func diffFlat(old, cur map[string]string, max int) []string {
	paths := map[string]bool{}
	for k := range old {
		paths[k] = true
	}
	for k := range cur {
		paths[k] = true
	}
	keys := make([]string, 0, len(paths))
	for k := range paths {
		if old[k] != cur[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []string
	for i, k := range keys {
		if len(out) >= max {
			out = append(out, fmt.Sprintf("… %d more changed paths", len(keys)-i))
			break
		}
		if o, ok := old[k]; ok {
			out = append(out, "- "+k+": "+o)
		}
		if c, ok := cur[k]; ok {
			out = append(out, "+ "+k+": "+c)
		}
	}
	return out
}
