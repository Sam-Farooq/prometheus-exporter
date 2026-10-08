// Command pgexporter serves PostgreSQL replication, bloat and transaction id
// metrics at /metrics.
//
// The DSN is read from DATA_SOURCE_NAME and is deliberately not a flag.
// Command lines are readable by every user on the host through ps, and a
// PostgreSQL DSN carries a password.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Sam-Farooq/prometheus-exporter/internal/collector"
	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

// version is stamped at build time with
// -ldflags "-X main.version=$(git describe --tags)". The default is the
// release this source is, so a go build with no flags still reports something
// true.
var version = "0.13.2"

// dsnEnv is the only place a credential enters this process.
const dsnEnv = "DATA_SOURCE_NAME"

// shutdownGrace is how long an in-flight scrape has to finish after a SIGTERM.
// It is longer than the default scrape timeout so a scrape that started just
// before the signal can complete rather than becoming a failed scrape in the
// middle of a rolling restart.
const shutdownGrace = 8 * time.Second

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "pgexporter: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	listen       string
	path         string
	timeout      time.Duration
	limits       postgres.Limits
	logLevel     slog.Level
	runtime      bool
	showVersion  bool
	seriesBudget bool
}

func run(args []string, stdout, stderr io.Writer) error {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	if opts.showVersion {
		_, err := fmt.Fprintf(stdout, "pgexporter %s\n", version)
		return err
	}

	if opts.seriesBudget {
		// The worst case under the configured limits, so the number in the
		// README is this command's output rather than a claim about it.
		return collector.WriteBudget(stdout, collector.Assumptions{
			Limits:    opts.limits,
			Standbys:  collector.DefaultAssumptions.Standbys,
			Slots:     collector.DefaultAssumptions.Slots,
			Databases: collector.DefaultAssumptions.Databases,
		})
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: opts.logLevel}))

	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		return fmt.Errorf("%s is not set; it holds the connection string, and a flag would put the password in ps", dsnEnv)
	}

	src, err := postgres.Open(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	exporter := collector.New(collector.Config{
		Source:  src,
		Limits:  opts.limits,
		Timeout: opts.timeout,
		Version: version,
		Logger:  logger,
	})

	reg := prometheus.NewRegistry()
	if err := reg.Register(exporter); err != nil {
		return fmt.Errorf("register exporter: %w", err)
	}
	if opts.runtime {
		// The exporter's own memory and file descriptors. Worth having,
		// and separable, because these series are not in the budget
		// -series-budget prints: client_golang owns them.
		if err := reg.Register(collectors.NewGoCollector()); err != nil {
			return fmt.Errorf("register go collector: %w", err)
		}
		if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
			return fmt.Errorf("register process collector: %w", err)
		}
	}

	srv := &http.Server{
		Addr:    opts.listen,
		Handler: newMux(reg, opts.path, opts.timeout),
		// Without this a half-open connection holds a goroutine until the
		// kernel times out the socket, which on a scraped endpoint is a
		// slow leak rather than an outage.
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", opts.listen, "path", opts.path, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		logger.Info("shutting down", "grace", shutdownGrace.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// newMux wires the three endpoints.
func newMux(reg *prometheus.Registry, path string, timeout time.Duration) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle(path, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		// A scrape that fails is an HTTP error, not a 200 with half the
		// metrics in it. pg_up carries the database's state; the status
		// code carries the exporter's.
		ErrorHandling: promhttp.HTTPErrorOnError,
		// Two Prometheus servers and a person with curl is three
		// concurrent scrapes of one database. The pool holds two
		// connections, so the fourth caller waits here rather than queuing
		// on the pool.
		MaxRequestsInFlight: 3,
		// Longer than the collector's own timeout, which is the thing that
		// is meant to end a slow scrape. This is the backstop.
		Timeout: timeout + 2*time.Second,
	}))

	// Liveness only, and it answers 200 while the database is unreachable.
	//
	// A health endpoint that fails when PostgreSQL is down gets the exporter
	// restarted or taken out of service discovery during a database outage,
	// which removes the pg_up 0 that says the database is down. The signal
	// disappears exactly when it is needed.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		width := len(path)
		if width < len("/healthz") {
			width = len("/healthz")
		}
		_, _ = fmt.Fprintf(w, "pgexporter %s\n\n%-*s  replication, bloat and transaction id metrics\n%-*s  liveness, independent of the database\n",
			version, width, path, width, "/healthz")
	})

	return mux
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := newFlagSet(stderr, &opts)
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if n := fs.NArg(); n > 0 {
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if !strings.HasPrefix(opts.path, "/") {
		return opts, fmt.Errorf("telemetry path %q must start with /", opts.path)
	}
	if opts.timeout <= 0 {
		return opts, fmt.Errorf("scrape timeout must be positive, got %s", opts.timeout)
	}
	if opts.limits.MaxTables < 0 {
		return opts, fmt.Errorf("max tables must not be negative, got %d", opts.limits.MaxTables)
	}
	if opts.limits.MinTableBytes < 0 {
		return opts, fmt.Errorf("min table bytes must not be negative, got %d", opts.limits.MinTableBytes)
	}
	return opts, nil
}
