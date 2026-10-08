package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/Sam-Farooq/prometheus-exporter/internal/collector"
	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

// levelFlag parses a log level by name. The alternative is an integer, and
// -log.level=4 in a unit file is not something anybody reads correctly.
type levelFlag struct{ level *slog.Level }

func (f levelFlag) String() string {
	if f.level == nil {
		return "info"
	}
	return strings.ToLower(f.level.String())
}

func (f levelFlag) Set(v string) error {
	switch strings.ToLower(v) {
	case "debug":
		*f.level = slog.LevelDebug
	case "info":
		*f.level = slog.LevelInfo
	case "warn", "warning":
		*f.level = slog.LevelWarn
	case "error":
		*f.level = slog.LevelError
	default:
		return fmt.Errorf("unknown level %q, want debug, info, warn or error", v)
	}
	return nil
}

func newFlagSet(stderr io.Writer, opts *options) *flag.FlagSet {
	fs := flag.NewFlagSet("pgexporter", flag.ContinueOnError)
	fs.SetOutput(stderr)

	fs.StringVar(&opts.listen, "web.listen-address", ":9187", "address to serve on")
	fs.StringVar(&opts.path, "web.telemetry-path", "/metrics", "path to serve metrics on")
	fs.BoolVar(&opts.runtime, "web.runtime-metrics", true,
		"also export the exporter's own Go runtime and process metrics; these are not in -series-budget")

	fs.DurationVar(&opts.timeout, "scrape-timeout", collector.DefaultTimeout,
		"how long one scrape may take before it reports pg_up 0; keep it below the Prometheus scrape_timeout")

	fs.Int64Var(&opts.limits.MinTableBytes, "collector.bloat.min-table-bytes", postgres.DefaultLimits.MinTableBytes,
		"skip tables smaller than this; a lookup table has dead rows too and nobody will vacuum it by hand")
	fs.IntVar(&opts.limits.MaxTables, "collector.bloat.max-tables", postgres.DefaultLimits.MaxTables,
		"keep at most this many tables, largest first; 0 means every table, which on a sharded schema is tens of thousands of series")

	fs.Var(levelFlag{level: &opts.logLevel}, "log.level", "debug, info, warn or error")

	fs.BoolVar(&opts.showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&opts.seriesBudget, "series-budget", false,
		"print the worst-case series count under the configured limits and exit")

	return fs
}
