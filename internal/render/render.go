// Package render formats changes for terminals and for MCP tool output.
package render

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
)

// Options controls table output.
type Options struct {
	Wide  bool
	Color bool
	// Scores, when set, adds a SCORE column (same length as the changes).
	Scores []int
	Now    time.Time
}

// ColorOK reports whether out is a terminal that wants colour.
func ColorOK(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func paint(s, code string, color bool) string {
	if !color || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func impactLabel(c change.Change, color bool) string {
	label := string(c.Impact)
	if c.IsSignal() {
		label = "warn"
	}
	code := map[string]string{"high": "31", "medium": "33", "low": "34", "warn": "90"}[label]
	return paint(label, code, color)
}

// Age renders a compact age such as "4m" or "2h".
func Age(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Clip shortens s to n runes.
func Clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ShortID is the prefix shown in tables; `whatchanged show` accepts it.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Changes writes a table.
func Changes(out io.Writer, cs []change.Change, o Options) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "AGE\tIMPACT\tSOURCE\tNAMESPACE\tOBJECT\tCHANGE"
	if o.Scores != nil {
		header = "SCORE\t" + header
	}
	if o.Wide {
		header = "ID\t" + header + "\tACTOR\tTIME"
	}
	fmt.Fprintln(tw, header)
	for i, c := range cs {
		ns := c.Namespace
		if ns == "" {
			ns = "-"
		}
		summary := c.Summary
		if !o.Wide {
			summary = Clip(summary, 70)
		}
		if len(c.Flags) > 0 {
			summary += " " + paint("["+strings.Join(c.Flags, ",")+"]", "35", o.Color)
		}
		obj := c.Object()
		if owner := c.Labels["owner"]; owner != "" && c.IsSignal() {
			obj += " (" + owner + ")"
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", Age(c.Time, o.Now), impactLabel(c, o.Color), c.Source, ns, Clip(obj, 56), summary)
		if o.Scores != nil {
			row = fmt.Sprint(o.Scores[i]) + "\t" + row
		}
		if o.Wide {
			actor := c.Actor
			if actor == "" {
				actor = "-"
			}
			row = ShortID(c.ID) + "\t" + row + "\t" + actor + "\t" + c.Time.Local().Format(time.RFC3339)
		}
		fmt.Fprintln(tw, row)
	}
	tw.Flush()
}

// Detail writes one change with its labels and diff.
func Detail(out io.Writer, c change.Change, color bool) {
	field := func(k, v string) {
		if v != "" {
			fmt.Fprintf(out, "%-10s %s\n", k+":", v)
		}
	}
	field("ID", c.ID)
	field("Time", c.Time.Local().Format(time.RFC3339))
	obj := c.Object()
	if c.Namespace != "" {
		obj = c.Namespace + "/" + obj
	}
	field("Object", obj)
	field("Source", c.Source)
	field("Action", c.Action)
	field("Impact", string(c.Impact))
	field("Actor", c.Actor)
	field("Summary", c.Summary)
	field("Flags", strings.Join(c.Flags, ", "))
	field("URL", c.URL)
	if len(c.Labels) > 0 {
		keys := make([]string, 0, len(c.Labels))
		for k := range c.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + c.Labels[k]
		}
		field("Labels", strings.Join(parts, ", "))
	}
	if len(c.Diff) == 0 {
		return
	}
	fmt.Fprintln(out, "\nDiff:")
	for _, l := range c.Diff {
		code := ""
		switch {
		case strings.HasPrefix(l, "+"):
			code = "32"
		case strings.HasPrefix(l, "-"):
			code = "31"
		}
		fmt.Fprintln(out, "  "+paint(l, code, color))
	}
}
