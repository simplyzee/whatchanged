package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/simplyzee/whatchanged/internal/api"
	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/grafana"
	"github.com/simplyzee/whatchanged/internal/kube"
	"github.com/simplyzee/whatchanged/internal/metrics"
	"github.com/simplyzee/whatchanged/internal/store"
	"github.com/simplyzee/whatchanged/internal/timeparse"
	"github.com/simplyzee/whatchanged/internal/tracker"
)

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// secret reads a value from a file (preferred) or an environment variable.
func secret(file, env string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv(env)), nil
}

func runServer(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "HTTP listen address")
	apiServer := fs.String("api-server", "", "Kubernetes API URL (default: in-cluster; for local dev run `kubectl proxy` and use http://127.0.0.1:8001)")
	tokenFile := fs.String("kube-token-file", "", "bearer token file for --api-server")
	caFile := fs.String("kube-ca-file", "", "CA bundle for --api-server")
	insecure := fs.Bool("kube-insecure-skip-tls-verify", false, "skip API server TLS verification (dev only)")
	noKube := fs.Bool("no-kubernetes", false, "don't watch a cluster; only accept pushed changes")
	dataDir := fs.String("data-dir", "", "directory to persist changes (empty = memory only)")
	retention := fs.String("retention", "7d", "how long to keep changes")
	maxChanges := fs.Int("max-changes", 250_000, "cap on stored changes")
	enable := fs.String("enable", "", "extra trackers to enable, comma separated (see `whatchanged trackers`)")
	disable := fs.String("disable", "", "trackers to disable, comma separated")
	excludeNS := fs.String("exclude-namespaces", "kube-node-lease", "namespaces to ignore, comma separated")
	ingestFile := fs.String("ingest-token-file", "", "file holding the bearer token for POST /api/v1/changes (or set WHATCHANGED_INGEST_TOKEN)")
	grafanaURL := fs.String("grafana-url", "", "push changes to Grafana as annotations (e.g. http://grafana.monitoring)")
	grafanaTokenFile := fs.String("grafana-token-file", "", "Grafana service-account token file (or set WHATCHANGED_GRAFANA_TOKEN)")
	grafanaMin := fs.String("grafana-min-impact", "medium", "minimum impact pushed to Grafana")
	cluster := fs.String("cluster-name", "", "cluster name, added as a tag on Grafana annotations")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "json", "json or text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level: %w", err)
	}
	hopts := &slog.HandlerOptions{Level: lvl}
	var logger *slog.Logger
	if *logFormat == "text" {
		logger = slog.New(slog.NewTextHandler(stderr, hopts))
	} else {
		logger = slog.New(slog.NewJSONHandler(stderr, hopts))
	}
	slog.SetDefault(logger)

	ret, err := timeparse.Duration(*retention)
	if err != nil {
		return fmt.Errorf("invalid --retention: %w", err)
	}
	ingestToken, err := secret(*ingestFile, "WHATCHANGED_INGEST_TOKEN")
	if err != nil {
		return fmt.Errorf("read ingest token: %w", err)
	}
	grafanaToken, err := secret(*grafanaTokenFile, "WHATCHANGED_GRAFANA_TOKEN")
	if err != nil {
		return fmt.Errorf("read grafana token: %w", err)
	}
	minImpact, err := change.ParseImpact(*grafanaMin)
	if err != nil {
		return err
	}

	st, err := store.Open(store.Options{Dir: *dataDir, Retention: ret, MaxItems: *maxChanges})
	if err != nil {
		return err
	}
	defer st.Close()

	reg := metrics.New()
	reg.Counter("whatchanged_changes_total", "Changes recorded, by source and action.", "source", "action")
	reg.Counter("whatchanged_ingested_total", "Changes received over the push API, by source.", "source")
	reg.Counter("whatchanged_grafana_annotations_total", "Grafana annotation pushes, by result.", "result")
	reg.Gauge("whatchanged_changes_stored", "Changes currently stored.", func() float64 { return float64(st.Len()) })

	st.OnAdd(func(c change.Change) { reg.Inc("whatchanged_changes_total", c.Source, c.Action) })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *grafanaURL != "" {
		var tags []string
		if *cluster != "" {
			tags = append(tags, "cluster:"+*cluster)
		}
		p := &grafana.Pusher{
			URL: *grafanaURL, Token: grafanaToken, MinImpact: minImpact, ExtraTags: tags, Logger: logger,
			OnResult: func(ok bool) {
				res := "ok"
				if !ok {
					res = "error"
				}
				reg.Inc("whatchanged_grafana_annotations_total", res)
			},
		}
		p.Start(ctx)
		st.OnAdd(p.Enqueue)
		logger.Info("pushing annotations to grafana", "url", *grafanaURL, "min_impact", minImpact)
	}

	excluded := map[string]bool{}
	for _, ns := range splitList(*excludeNS) {
		excluded[ns] = true
	}
	sink := func(c change.Change) {
		if c.Namespace != "" && excluded[c.Namespace] {
			return
		}
		if _, err := st.Add(c); err != nil {
			logger.Error("persist change", "err", err)
		}
	}

	srv := &api.Server{Store: st, IngestToken: ingestToken, Metrics: reg, Logger: logger}
	runner := &tracker.Runner{Logger: logger}
	if *noKube {
		srv.Ready = func() bool { return true }
	} else {
		kc, err := kube.New(kube.Config{
			Server: *apiServer, TokenFile: *tokenFile, CAFile: *caFile, Insecure: *insecure,
			UserAgent: "whatchanged/" + Version,
		})
		if err != nil {
			return err
		}
		trackers, err := tracker.Select(splitList(*enable), splitList(*disable), sink, nil)
		if err != nil {
			return err
		}
		runner.Client = kc
		srv.Ready = runner.Ready
		srv.Status = func() any { return runner.Statuses() }
		reg.Gauge("whatchanged_trackers_synced", "Trackers that completed their initial sync.", func() float64 {
			n := 0
			for _, s := range runner.Statuses() {
				if s.State == "synced" {
					n++
				}
			}
			return float64(n)
		})
		reg.Gauge("whatchanged_watch_restarts", "Watch restarts across all trackers since start.", func() float64 {
			n := 0
			for _, s := range runner.Statuses() {
				n += s.Restarts
			}
			return float64(n)
		})
		names := make([]string, len(trackers))
		for i, t := range trackers {
			names[i] = t.Name()
		}
		logger.Info("starting trackers", "trackers", strings.Join(names, ","))
		go runner.Run(ctx, trackers)
	}

	// Retention housekeeping.
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := st.Prune(); err != nil {
					logger.Error("prune", "err", err)
				} else if n > 0 {
					logger.Info("pruned old changes", "count", n)
				}
			}
		}
	}()

	hs := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", *listen, "version", Version, "ingest", ingestToken != "", "data_dir", *dataDir)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}
	return nil
}
