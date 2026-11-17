// Package cli implements the whatchanged command line.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/simplyzee/whatchanged/internal/api"
	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/tracker"
)

// Build info, set by main.
var (
	Version = "dev"
	Commit  = "none"
)

const usage = `whatchanged — what changed in my cluster, and what probably broke it?

Usage:
  whatchanged [list] [flags]     Show recent changes (default command)
  whatchanged suspects [flags]   Rank the most likely culprits for a problem
  whatchanged push [flags]       Record a change from CI, Terraform, a script…
  whatchanged server [flags]     Run the watcher + API (usually in-cluster)
  whatchanged trackers           List the built-in change trackers
  whatchanged version

Examples:
  whatchanged --since 30m -n payments
  whatchanged suspects -n payments --at 10m
  whatchanged push --source github-actions --name api --summary "deploy v1.4.2" --url "$RUN_URL"

Environment:
  WHATCHANGED_URL     API address for list/suspects/push (default http://localhost:8080)
  WHATCHANGED_TOKEN   Bearer token for push

Run 'whatchanged <command> -h' for command flags.
`

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	cmd := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "list", "ls", "changes":
		err = runList(args, stdout)
	case "suspects", "why":
		err = runSuspects(args, stdout)
	case "push":
		err = runPush(args, stdout)
	case "server", "serve":
		err = runServer(args, stderr)
	case "trackers":
		for _, r := range tracker.Registry {
			def := "on"
			if !r.Default {
				def = "off (opt-in)"
			}
			fmt.Fprintf(stdout, "%-36s %s\n", r.Name, def)
		}
	case "version", "--version":
		fmt.Fprintf(stdout, "whatchanged %s (%s)\n", Version, Commit)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// common flags for client commands
type clientFlags struct {
	server    string
	namespace string
	output    string
}

func (c *clientFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.server, "server", envOr("WHATCHANGED_URL", "http://localhost:8080"), "whatchanged API address")
	fs.StringVar(&c.namespace, "namespace", "", "namespace(s), comma separated")
	fs.StringVar(&c.namespace, "n", "", "shorthand for --namespace")
	fs.StringVar(&c.output, "o", "table", "output: table, wide or json")
}

func runList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var cf clientFlags
	cf.register(fs)
	since := fs.String("since", "1h", "how far back: 30m, 2h, 1d or RFC 3339")
	until := fs.String("until", "", "end of range (default now)")
	kind := fs.String("kind", "", "kind(s), e.g. Deployment,ConfigMap")
	source := fs.String("source", "", "source(s), e.g. kubernetes,helm,argocd,github-actions")
	action := fs.String("action", "", "action(s), e.g. image,config,release")
	impact := fs.String("impact", "", "minimum impact: low, medium, high")
	text := fs.String("q", "", "free-text search")
	noWarn := fs.Bool("no-warnings", false, "hide Warning events")
	limit := fs.Int("limit", 200, "maximum results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{"since": {*since}, "limit": {fmt.Sprint(*limit)}}
	for k, v := range map[string]string{"until": *until, "namespace": cf.namespace, "kind": *kind, "source": *source, "action": *action, "impact": *impact, "q": *text} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if *noWarn {
		q.Set("signals", "false")
	}
	var res api.ChangesResponse
	if err := newClient(cf.server, "").do("GET", "/api/v1/changes", q, nil, &res); err != nil {
		return err
	}
	if cf.output == "json" {
		return printJSON(out, res)
	}
	if len(res.Changes) == 0 {
		fmt.Fprintf(out, "Nothing changed since %s.\n", res.Since.Local().Format("15:04 Mon 2 Jan"))
		return nil
	}
	printChanges(out, res.Changes, nil, cf.output == "wide")
	return nil
}

func runSuspects(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("suspects", flag.ContinueOnError)
	var cf clientFlags
	cf.register(fs)
	at := fs.String("at", "now", "when the problem started: now, 10m (ago) or RFC 3339")
	window := fs.String("window", "1h", "how far before --at to look")
	limit := fs.Int("limit", 10, "maximum suspects")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{"at": {*at}, "window": {*window}, "limit": {fmt.Sprint(*limit)}}
	if cf.namespace != "" {
		q.Set("namespace", cf.namespace)
	}
	var res api.SuspectsResponse
	if err := newClient(cf.server, "").do("GET", "/api/v1/suspects", q, nil, &res); err != nil {
		return err
	}
	if cf.output == "json" {
		return printJSON(out, res)
	}
	if len(res.Suspects) == 0 {
		fmt.Fprintf(out, "No changes in the %s before %s. Probably not a change this time.\n", res.Window, res.At.Local().Format("15:04"))
		return nil
	}
	fmt.Fprintf(out, "Most likely culprits for a problem at %s (%s window, %d warning event(s)):\n\n",
		res.At.Local().Format("15:04 Mon 2 Jan"), res.Window, res.Warnings)
	cs := make([]change.Change, len(res.Suspects))
	scores := make([]string, len(res.Suspects))
	for i, s := range res.Suspects {
		cs[i] = s.Change
		scores[i] = fmt.Sprint(s.Score)
	}
	printChanges(out, cs, scores, cf.output == "wide")
	if cf.output == "wide" {
		return nil
	}
	fmt.Fprintln(out)
	for i, s := range res.Suspects {
		if i == 3 {
			break
		}
		fmt.Fprintf(out, "#%d %s: %s\n", i+1, s.Object(), strings.Join(s.Reasons, ", "))
	}
	return nil
}

type labelsFlag map[string]string

func (l labelsFlag) String() string { return "" }
func (l labelsFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("label must be key=value, got %q", v)
	}
	l[k] = val
	return nil
}

func runPush(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	server := fs.String("server", envOr("WHATCHANGED_URL", "http://localhost:8080"), "whatchanged API address")
	token := fs.String("token", "", "bearer token (default $WHATCHANGED_TOKEN)")
	var c change.Change
	labels := labelsFlag{}
	fs.StringVar(&c.Source, "source", "", "where the change came from, e.g. github-actions, terraform (required)")
	fs.StringVar(&c.Name, "name", "", "what changed, e.g. a service or stack name")
	fs.StringVar(&c.Summary, "summary", "", "one line describing the change")
	fs.StringVar(&c.Namespace, "namespace", "", "namespace it affects, if any")
	fs.StringVar(&c.Namespace, "n", "", "shorthand for --namespace")
	fs.StringVar(&c.Kind, "kind", "", "kind of object, e.g. Service, Stack, FeatureFlag")
	fs.StringVar(&c.Action, "action", change.ActionDeploy, "deploy, config, release, flag, migration…")
	impact := fs.String("impact", "medium", "low, medium or high")
	fs.StringVar(&c.Actor, "actor", envOr("GITHUB_ACTOR", ""), "who made the change")
	fs.StringVar(&c.URL, "url", "", "link to the pipeline run, PR or plan")
	at := fs.String("at", "", "when it happened (default now)")
	fs.Var(labels, "label", "key=value label (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	imp, err := change.ParseImpact(*impact)
	if err != nil {
		return err
	}
	c.Impact = imp
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("--at must be RFC 3339: %w", err)
		}
		c.Time = t
	}
	if len(labels) > 0 {
		c.Labels = labels
	}
	if c.Source == "" {
		return errors.New("--source is required")
	}
	tok := *token
	if tok == "" {
		tok = os.Getenv("WHATCHANGED_TOKEN")
	}
	if tok == "" {
		return errors.New("a token is required: set WHATCHANGED_TOKEN or --token")
	}
	var res api.IngestResponse
	if err := newClient(*server, tok).do("POST", "/api/v1/changes", nil, c, &res); err != nil {
		return err
	}
	if res.Accepted == 0 {
		fmt.Fprintln(out, "already recorded:", strings.Join(res.IDs, ", "))
		return nil
	}
	fmt.Fprintln(out, "recorded:", strings.Join(res.IDs, ", "))
	return nil
}

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func colorOK(out io.Writer) bool {
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

func paintImpact(c change.Change, color bool) string {
	label := string(c.Impact)
	if c.IsSignal() {
		label = "warn"
	}
	if !color {
		return label
	}
	code := map[string]string{"high": "31", "medium": "33", "low": "34", "warn": "90"}[label]
	return "\x1b[" + code + "m" + label + "\x1b[0m"
}

func shortAge(t time.Time) string {
	d := time.Since(t)
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

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func printChanges(out io.Writer, cs []change.Change, scores []string, wide bool) {
	color := colorOK(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "AGE\tIMPACT\tSOURCE\tNAMESPACE\tOBJECT\tCHANGE"
	if scores != nil {
		header = "SCORE\t" + header
	}
	if wide {
		header += "\tACTOR\tTIME"
	}
	fmt.Fprintln(tw, header)
	for i, c := range cs {
		ns := c.Namespace
		if ns == "" {
			ns = "-"
		}
		summary := c.Summary
		if !wide {
			summary = clip(summary, 70)
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", shortAge(c.Time), paintImpact(c, color), c.Source, ns, clip(c.Object(), 48), summary)
		if scores != nil {
			row = scores[i] + "\t" + row
		}
		if wide {
			actor := c.Actor
			if actor == "" {
				actor = "-"
			}
			row += "\t" + actor + "\t" + c.Time.Local().Format(time.RFC3339)
		}
		fmt.Fprintln(tw, row)
	}
	tw.Flush()
}
