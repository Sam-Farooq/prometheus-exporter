package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Sam-Farooq/prometheus-exporter/internal/collector"
	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"-version"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out.String(); got != "pgexporter "+version+"\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestSeriesBudgetFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"-series-budget"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()

	// The figure the README quotes. It is this command's output, so a
	// change to the limits or to the metric set moves both or fails here.
	for _, want := range []string{
		"pg_table_dead_rows",
		"worst case",
		"3137",
		"at 500 tables, 16 standbys, 32 slots, 16 databases",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("-series-budget output is missing %q:\n%s", want, got)
		}
	}
}

func TestSeriesBudgetFollowsTheFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"-series-budget", "-collector.bloat.max-tables", "25"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 3137 minus six metrics for each of the 475 tables no longer budgeted.
	if !strings.Contains(out.String(), "287") {
		t.Errorf("budget did not follow -collector.bloat.max-tables:\n%s", out.String())
	}
}

func TestFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a telemetry path without a slash", []string{"-web.telemetry-path", "metrics"}, "must start with /"},
		{"a zero scrape timeout", []string{"-scrape-timeout", "0s"}, "must be positive"},
		{"a negative table cap", []string{"-collector.bloat.max-tables", "-1"}, "must not be negative"},
		{"a negative size filter", []string{"-collector.bloat.min-table-bytes", "-4096"}, "must not be negative"},
		{"a stray argument", []string{"postgres://localhost/app"}, "unexpected argument"},
		{"an unknown log level", []string{"-log.level", "verbose"}, "unknown level"},
		{"an unknown flag", []string{"-collector.everything"}, "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			_, err := parseFlags(tc.args, &errOut)
			if err == nil {
				t.Fatalf("parseFlags(%v) returned no error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDefaultsMatchTheDocumentedOnes(t *testing.T) {
	opts, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.listen != ":9187" {
		t.Errorf("listen = %q", opts.listen)
	}
	if opts.path != "/metrics" {
		t.Errorf("path = %q", opts.path)
	}
	if opts.timeout != collector.DefaultTimeout {
		t.Errorf("timeout = %v", opts.timeout)
	}
	if opts.limits != postgres.DefaultLimits {
		t.Errorf("limits = %+v, want %+v", opts.limits, postgres.DefaultLimits)
	}
}

func TestMissingDSNSaysWhyItIsNotAFlag(t *testing.T) {
	t.Setenv(dsnEnv, "")
	var out, errOut bytes.Buffer
	err := run(nil, &out, &errOut)
	if err == nil {
		t.Fatal("run with no DSN returned no error")
	}
	// A reviewer's first reaction to a missing -dsn flag is that it was
	// forgotten, so the error says it was not.
	for _, want := range []string{dsnEnv, "ps"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func newTestMux(t *testing.T, src postgres.Source) *http.ServeMux {
	t.Helper()
	e := collector.New(collector.Config{Source: src, Limits: postgres.DefaultLimits, Version: version})
	reg := prometheus.NewRegistry()
	if err := reg.Register(e); err != nil {
		t.Fatal(err)
	}
	return newMux(reg, "/metrics", time.Second)
}

func get(t *testing.T, mux *http.ServeMux, path string) (status int, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return rec.Code, rec.Body.String()
}

func TestHealthzDoesNotDependOnTheDatabase(t *testing.T) {
	// A health endpoint that fails during a database outage gets the
	// exporter restarted or dropped from service discovery, which removes
	// the pg_up 0 that was reporting the outage.
	mux := newTestMux(t, &postgres.Fake{PingErr: errors.New("connection refused")})

	code, body := get(t, mux, "/healthz")
	if code != http.StatusOK {
		t.Errorf("/healthz = %d with the database down, want 200", code)
	}
	if strings.TrimSpace(body) != "ok" {
		t.Errorf("/healthz body = %q", body)
	}
}

func TestMetricsServesPgUpZeroWithATwoHundred(t *testing.T) {
	mux := newTestMux(t, &postgres.Fake{PingErr: errors.New("connection refused")})

	code, body := get(t, mux, "/metrics")
	// pg_up carries the database's state. The status code carries the
	// exporter's, and the exporter is fine.
	if code != http.StatusOK {
		t.Errorf("/metrics = %d, want 200", code)
	}
	for _, want := range []string{"pg_up 0", `pg_exporter_scrape_errors_total{collector="connect"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

func TestMetricsServesAHealthyScrape(t *testing.T) {
	f := &postgres.Fake{Data: postgres.Fixture{
		Replication: postgres.Replication{
			Standbys: []postgres.Standby{{ApplicationName: "replica-a", State: "streaming", LagBytes: 4096}},
		},
		Wraparound: postgres.Wraparound{FreezeMaxAge: 200000000},
	}}
	mux := newTestMux(t, f)

	code, body := get(t, mux, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d", code)
	}
	for _, want := range []string{
		"pg_up 1",
		`pg_replication_standby_lag_bytes{application_name="replica-a"} 4096`,
		`pg_exporter_build_info{version="` + version + `"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q:\n%s", want, body)
		}
	}
}

func TestIndexListsTheEndpointsAndNothingElseIs404(t *testing.T) {
	mux := newTestMux(t, &postgres.Fake{})

	code, body := get(t, mux, "/")
	if code != http.StatusOK {
		t.Errorf("/ = %d", code)
	}
	for _, want := range []string{"pgexporter " + version, "/metrics", "/healthz"} {
		if !strings.Contains(body, want) {
			t.Errorf("/ is missing %q:\n%s", want, body)
		}
	}

	// A mux pattern of "/" matches everything, so a typo in a scrape
	// config would otherwise return the index with a 200 and Prometheus
	// would report the target as up with no metrics.
	if code, _ := get(t, mux, "/metric"); code != http.StatusNotFound {
		t.Errorf("/metric = %d, want 404", code)
	}
}

func TestTelemetryPathIsConfigurable(t *testing.T) {
	e := collector.New(collector.Config{Source: &postgres.Fake{}, Version: version})
	reg := prometheus.NewRegistry()
	if err := reg.Register(e); err != nil {
		t.Fatal(err)
	}
	mux := newMux(reg, "/probe", time.Second)

	if code, body := get(t, mux, "/probe"); code != http.StatusOK || !strings.Contains(body, "pg_up") {
		t.Errorf("/probe = %d, body has pg_up: %v", code, strings.Contains(body, "pg_up"))
	}
	if code, body := get(t, mux, "/"); code != http.StatusOK || !strings.Contains(body, "/probe") {
		t.Errorf("the index does not name the configured path: %q", body)
	}
}

func TestLogLevelFlag(t *testing.T) {
	cases := map[string]string{"debug": "debug", "info": "info", "warn": "warn", "WARNING": "warn", "error": "error"}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			opts, err := parseFlags([]string{"-log.level", in}, io.Discard)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if got := strings.ToLower(opts.logLevel.String()); got != want {
				t.Errorf("level = %q, want %q", got, want)
			}
		})
	}
}
