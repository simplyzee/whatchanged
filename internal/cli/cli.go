// Package cli implements the whatchanged command line (also installed as
// the kubectl plugin `kubectl whatchanged`).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/mcp"
	"github.com/simplyzee/whatchanged/internal/render"
	"github.com/simplyzee/whatchanged/internal/tracker"
)

// Build info, set by main.
var (
	Version = "dev"
	Commit  = "none"
	// Plugin is true when invoked as kubectl-whatchanged.
	Plugin = false
	// Stdin is read by `whatchanged mcp`.
	Stdin io.Reader = os.Stdin
)

func prog() string {
	if Plugin {
		return "kubectl whatchanged"
	}
	return "whatchanged"
}

func usage() string {
	return strings.ReplaceAll(`PROG: what changed in my cluster, and what probably broke it?

Usage:
  PROG [list] [flags]      Show recent changes (default command)
  PROG suspects [flags]    Rank the most likely culprits for a problem
  PROG show <id>           Show one change, with its diff
  PROG push [flags]        Record a change from CI, Terraform, a script…
  PROG mcp                 Run an MCP server on stdio for AI agents
  PROG server [flags]      Run the watcher + API (usually in-cluster)
  PROG trackers            List the built-in change trackers
  PROG version

Examples:
  PROG --since 30m -n payments
  PROG suspects -n payments --at 10m
  PROG --flag manual,freeze --since 7d
  PROG push --source github-actions --name api --summary "deploy v1.4.2" --url "$RUN_URL"

Connecting:
  By default PROG talks to WHATCHANGED_URL (or http://localhost:8080).
  With --port-forward (the default for kubectl whatchanged) it runs
  "kubectl port-forward" to svc/whatchanged in the whatchanged namespace
  for you. Use --install-namespace, --service, --context and --kubeconfig
  to point it elsewhere.

Environment:
  WHATCHANGED_URL        API address
  WHATCHANGED_TOKEN      Bearer token for push and for mcp's record_change
  WHATCHANGED_NAMESPACE  Namespace whatchanged is installed in (port-forward)
  WHATCHANGED_SERVICE    Service name (port-forward)

Run 'PROG <command> -h' for command flags.
`, "PROG", prog())
}

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	cmd := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "list", "ls", "changes":
		err = runList(args, stdout, stderr)
	case "suspects", "why":
		err = runSuspects(args, stdout, stderr)
	case "show", "get", "diff":
		err = runShow(args, stdout, stderr)
	case "push":
		err = runPush(args, stdout, stderr)
	case "mcp":
		err = runMCP(args, stdout, stderr)
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
		fmt.Fprint(stdout, usage())
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage())
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

type outFlags struct {
	namespace string
	output    string
}

func (o *outFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.namespace, "namespace", "", "namespace(s), comma separated")
	fs.StringVar(&o.namespace, "n", "", "shorthand for --namespace")
	fs.StringVar(&o.output, "o", "table", "output: table, wide or json")
}

func runList(args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf connFlags
	cf.register(fs)
	var of outFlags
	of.register(fs)
	since := fs.String("since", "1h", "how far back: 30m, 2h, 1d or RFC 3339")
	until := fs.String("until", "", "end of range (default now)")
	kind := fs.String("kind", "", "kind(s), e.g. Deployment,ConfigMap")
	source := fs.String("source", "", "source(s), e.g. kubernetes,helm,argocd,github-actions")
	action := fs.String("action", "", "action(s), e.g. image,config,release")
	impact := fs.String("impact", "", "minimum impact: low, medium, high")
	flags := fs.String("flag", "", "only flagged changes: manual, off-hours, freeze")
	text := fs.String("q", "", "free-text search")
	noWarn := fs.Bool("no-warnings", false, "hide Warning events")
	limit := fs.Int("limit", 200, "maximum results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, done, err := cf.connect(stderr)
	if err != nil {
		return err
	}
	defer done()
	q := url.Values{"since": {*since}, "limit": {fmt.Sprint(*limit)}}
	for k, v := range map[string]string{"until": *until, "namespace": of.namespace, "kind": *kind, "source": *source,
		"action": *action, "impact": *impact, "flag": *flags, "q": *text} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if *noWarn {
		q.Set("signals", "false")
	}
	res, err := c.Changes(q)
	if err != nil {
		return err
	}
	if of.output == "json" {
		return printJSON(out, res)
	}
	if len(res.Changes) == 0 {
		fmt.Fprintf(out, "Nothing changed since %s.\n", res.Since.Local().Format("15:04 Mon 2 Jan"))
		return nil
	}
	render.Changes(out, res.Changes, render.Options{Wide: of.output == "wide", Color: render.ColorOK(out)})
	return nil
}

func runSuspects(args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("suspects", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf connFlags
	cf.register(fs)
	var of outFlags
	of.register(fs)
	at := fs.String("at", "now", "when the problem started: now, 10m (ago) or RFC 3339")
	window := fs.String("window", "1h", "how far before --at to look")
	limit := fs.Int("limit", 10, "maximum suspects")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, done, err := cf.connect(stderr)
	if err != nil {
		return err
	}
	defer done()
	q := url.Values{"at": {*at}, "window": {*window}, "limit": {fmt.Sprint(*limit)}}
	if of.namespace != "" {
		q.Set("namespace", of.namespace)
	}
	res, err := c.Suspects(q)
	if err != nil {
		return err
	}
	if of.output == "json" {
		return printJSON(out, res)
	}
	if len(res.Suspects) == 0 {
		fmt.Fprintf(out, "No changes in the %s before %s. Probably not a change this time.\n", res.Window, res.At.Local().Format("15:04"))
		return nil
	}
	fmt.Fprintf(out, "Most likely culprits for a problem at %s (%s window, %d warning event(s)):\n\n",
		res.At.Local().Format("15:04 Mon 2 Jan"), res.Window, res.Warnings)
	cs := make([]change.Change, len(res.Suspects))
	scores := make([]int, len(res.Suspects))
	for i, s := range res.Suspects {
		cs[i], scores[i] = s.Change, s.Score
	}
	render.Changes(out, cs, render.Options{Wide: of.output == "wide", Color: render.ColorOK(out), Scores: scores})
	fmt.Fprintln(out)
	for i, s := range res.Suspects {
		if i == 3 {
			break
		}
		fmt.Fprintf(out, "#%d %s: %s\n", i+1, s.Object(), strings.Join(s.Reasons, ", "))
	}
	fmt.Fprintf(out, "\nSee the diff: %s show %s\n", prog(), render.ShortID(res.Suspects[0].ID))
	return nil
}

func runShow(args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf connFlags
	cf.register(fs)
	output := fs.String("o", "text", "output: text or json")
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if id == "" {
		id = fs.Arg(0)
	}
	if id == "" {
		return errors.New("usage: " + prog() + " show <id> (use -o wide on list to see IDs)")
	}
	c, done, err := cf.connect(stderr)
	if err != nil {
		return err
	}
	defer done()
	ch, err := c.Change(id)
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(out, ch)
	}
	render.Detail(out, ch, render.ColorOK(out))
	if len(ch.Diff) == 0 && !ch.IsSignal() {
		fmt.Fprintln(out, "\n(no diff recorded for this change)")
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

func runPush(args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf connFlags
	cf.register(fs)
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
	if cf.token() == "" {
		return errors.New("a token is required: set WHATCHANGED_TOKEN or --token")
	}
	cl, done, err := cf.connect(stderr)
	if err != nil {
		return err
	}
	defer done()
	res, err := cl.Push(c)
	if err != nil {
		return err
	}
	if res.Accepted == 0 {
		fmt.Fprintln(out, "already recorded:", strings.Join(res.IDs, ", "))
		return nil
	}
	fmt.Fprintln(out, "recorded:", strings.Join(res.IDs, ", "))
	return nil
}

func runMCP(args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf connFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, done, err := cf.connect(stderr)
	if err != nil {
		return err
	}
	defer done()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := &mcp.Server{Client: c, Version: Version}
	return s.ServeStdio(ctx, Stdin, out)
}

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
