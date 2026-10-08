package collector

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

// render returns one scrape as exposition text, which is what every assertion
// below reads. Going through the registry rather than through Collect directly
// is deliberate: the registry is where a duplicate label set becomes an error,
// and a test that reads the channel sees the duplicate as two metrics and
// passes.
func render(t *testing.T, e *Exporter) string {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(e); err != nil {
		t.Fatalf("register: %v", err)
	}
	var buf bytes.Buffer
	if err := gatherInto(&buf, reg, describedNames(e)); err != nil {
		t.Fatalf("gather: %v", err)
	}
	return buf.String()
}

func hasLine(out, line string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

func mustHave(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !hasLine(out, l) {
			t.Errorf("missing line:\n  %s", l)
		}
	}
}

func mustNotHave(t *testing.T, out string, substrings ...string) {
	t.Helper()
	for _, s := range substrings {
		if strings.Contains(out, s) {
			t.Errorf("output still contains %q", s)
		}
	}
}

func TestFailedScrapeSetsUpToZeroAndKeepsItsSeries(t *testing.T) {
	fake := &postgres.Fake{
		Data:    load(t, "primary-healthy.json"),
		PingErr: errors.New("connection refused"),
	}
	e := newExporter(t, fake, postgres.DefaultLimits)
	out := render(t, e)

	// The whole point. A vanished series reads as a healthy quiet period to
	// anything written as an absence check, and nothing alerts.
	mustHave(t, out,
		"pg_up 0",
		`pg_exporter_collector_success{collector="connect"} 0`,
		`pg_exporter_collector_success{collector="replication"} 0`,
		`pg_exporter_collector_success{collector="bloat"} 0`,
		`pg_exporter_collector_success{collector="wraparound"} 0`,
		`pg_exporter_scrape_errors_total{collector="connect"} 1`,
		`pg_exporter_scrape_errors_total{collector="bloat"} 0`,
		"pg_exporter_scrapes_total 1",
	)

	// No data series, and no duration for a scrape that never happened.
	mustNotHave(t, out, "pg_table_dead_rows", "pg_replication_standby_lag_bytes")
	if strings.Contains(out, `pg_exporter_scrape_duration_seconds{collector="bloat"}`) {
		t.Error("a duration was reported for a collector that never ran")
	}
	if !strings.Contains(out, `pg_exporter_scrape_duration_seconds{collector="connect"}`) {
		t.Error("the connect attempt happened and should carry a duration")
	}

	// Queries are not attempted once the ping has failed. On a server that
	// is down this is the difference between one timeout and four.
	if c := fake.Counts(); c.Replication != 0 || c.Bloat != 0 || c.Wraparound != 0 {
		t.Errorf("queries ran after the ping failed: %+v", c)
	}
}

func TestOneDeniedViewLeavesTheRestOfTheScrape(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "primary-healthy.json"), ReplicationErr: errDenied}
	e := newExporter(t, fake, postgres.DefaultLimits)
	out := render(t, e)

	// A role without pg_monitor is a configuration mistake, not an outage.
	// Reporting pg_up 0 for it sends somebody to look at the database.
	mustHave(t, out,
		"pg_up 1",
		`pg_exporter_collector_success{collector="replication"} 0`,
		`pg_exporter_collector_success{collector="bloat"} 1`,
		`pg_exporter_scrape_errors_total{collector="replication"} 1`,
		`pg_table_dead_rows{schema="public",table="orders"} 12000`,
		`pg_database_xid_age_transactions{datname="app"} 4.2e+07`,
	)
	mustNotHave(t, out, "pg_replication_standby_lag_bytes", "pg_in_recovery")
}

func TestScrapeStopsAtTheTimeout(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "primary-healthy.json"), Hang: true}
	e := New(Config{Source: fake, Limits: postgres.DefaultLimits, Timeout: 20 * time.Millisecond, Version: testVersion})

	start := time.Now()
	out := render(t, e)
	elapsed := time.Since(start)

	// The failure this prevents is a scrape that blocks until the kernel
	// gives up on the socket. Prometheus would time out first, the target
	// would go stale, and the reason would be nowhere in the metrics.
	if elapsed > 2*time.Second {
		t.Errorf("scrape took %s against a server that never answers", elapsed)
	}
	mustHave(t, out, "pg_up 0", `pg_exporter_scrape_errors_total{collector="connect"} 1`)
	if n := fake.Counts().Cancelled; n != 1 {
		t.Errorf("Cancelled = %d, want 1: the context should have ended the call", n)
	}
}

func TestCountersSurviveScrapesAndGaugesDoNot(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "primary-healthy.json")}
	e := newExporter(t, fake, postgres.DefaultLimits)

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(e); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := reg.Gather(); err != nil {
			t.Fatalf("gather %d: %v", i, err)
		}
	}

	// The opposite failure to the stale series one: having moved the data to
	// const metrics, a counter rebuilt per scrape would read 1 here forever
	// and rate() over it would be a flat zero.
	if got := testutil.ToFloat64(e.scrapes); got != 3 {
		t.Errorf("pg_exporter_scrapes_total = %v after three scrapes, want 3", got)
	}
}

func TestErrorCountersExistBeforeTheFirstError(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "empty.json")}
	e := newExporter(t, fake, postgres.DefaultLimits)
	out := render(t, e)

	// increase() over a window that starts before a counter existed has no
	// baseline, so the first failure of a collector would be invisible for
	// one window if the series appeared with it.
	for _, n := range CollectorNames {
		mustHave(t, out, `pg_exporter_scrape_errors_total{collector="`+n+`"} 0`)
	}
}

func TestDuplicateApplicationNameFoldsInsteadOfBreakingTheEndpoint(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "primary-degraded.json")}
	e := newExporter(t, fake, postgres.DefaultLimits)

	// Two standbys that never set application_name both arrive as
	// "walreceiver". Emitting both sends the registry two metrics with the
	// same name and labels, Gather returns an error, and /metrics answers
	// 500: the exporter looks down because two replicas share a connection
	// string.
	out := render(t, e)

	mustHave(t, out,
		`pg_replication_standby_lag_bytes{application_name="walreceiver"} 2.147483648e+09`,
		`pg_replication_standby_lag_bytes{application_name="replica-c"} 5.36870912e+08`,
		`pg_replication_standby_replay_lag_seconds{application_name="walreceiver"} 11.5`,
		"pg_replication_standbys_folded 1",
	)

	// The larger of the two lags survives the fold. A standby 2 GiB behind
	// and one 1 GiB behind are not reported as the better of the pair.
	if strings.Contains(out, `pg_replication_standby_lag_bytes{application_name="walreceiver"} 1.073741824e+09`) {
		t.Error("folding kept the smaller lag")
	}

	// replica-c has never reported a replay time, so it has no seconds
	// series. A zero there would say it is caught up.
	if strings.Contains(out, `pg_replication_standby_replay_lag_seconds{application_name="replica-c"}`) {
		t.Error("a standby that has not reported got a replay lag anyway")
	}
}

func TestEveryReplicationStateIsPresentAtZero(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "primary-degraded.json")}
	e := newExporter(t, fake, postgres.DefaultLimits)
	out := render(t, e)

	want := map[string]string{
		"streaming": "2",
		"catchup":   "1",
		"startup":   "0",
		"backup":    "0",
		"stopping":  "0",
		"unknown":   "0",
	}
	for state, v := range want {
		mustHave(t, out, `pg_replication_standbys{state="`+state+`"} `+v)
	}
}

func TestAnUnknownStateFoldsIntoUnknown(t *testing.T) {
	fake := &postgres.Fake{Data: postgres.Fixture{
		Replication: postgres.Replication{
			Standbys: []postgres.Standby{{ApplicationName: "replica-x", State: "quantum", LagBytes: 1}},
		},
	}}
	e := newExporter(t, fake, postgres.DefaultLimits)
	out := render(t, e)

	// A state this exporter has not heard of must not become a new label
	// value. A future PostgreSQL release adding one would otherwise add a
	// series to every instance of this exporter at once.
	mustHave(t, out, `pg_replication_standbys{state="unknown"} 1`)
	mustNotHave(t, out, `state="quantum"`)
}

func TestNeverVacuumedTableHasNoTimestamp(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "primary-degraded.json")}
	e := newExporter(t, fake, postgres.DefaultLimits)
	out := render(t, e)

	// public.events has never been autovacuumed. A zero here is 1970, and
	// time() - 0 > 86400 is a true statement about a table that has no
	// last vacuum rather than an old one.
	mustHave(t, out, `pg_table_last_autovacuum_timestamp_seconds{schema="public",table="sessions"} 1.7894256e+09`)
	if strings.Contains(out, `pg_table_last_autovacuum_timestamp_seconds{schema="public",table="events"}`) {
		t.Error("a table autovacuum has never touched got a timestamp")
	}
	// The dead row count and the threshold are both there, which is what
	// makes the absence readable.
	mustHave(t, out,
		`pg_table_dead_rows{schema="public",table="events"} 4.1e+07`,
		`pg_table_autovacuum_threshold_rows{schema="public",table="events"} 1.900005e+07`,
	)
}

func TestRowCapIsReported(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "table-cap.json")}
	e := newExporter(t, fake, postgres.Limits{MinTableBytes: 8 << 20, MaxTables: 2})
	out := render(t, e)

	// Five tables pass the size filter, two are kept. Without this gauge a
	// top-N and a complete list are the same picture.
	mustHave(t, out,
		"pg_table_series_omitted 3",
		`pg_table_size_bytes{schema="public",table="t_900m"} 9.437184e+08`,
		`pg_table_size_bytes{schema="public",table="t_800m"} 8.388608e+08`,
	)
	mustNotHave(t, out, `table="t_700m"`, `table="t_tiny"`)
}

func TestConcurrentScrapesDoNotSeeEachOther(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "primary-healthy.json")}
	e := newExporter(t, fake, postgres.DefaultLimits)

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(e); err != nil {
		t.Fatal(err)
	}

	// Two Prometheus servers scraping one exporter, or one server and a
	// person with curl. Run under -race, which CI does, this is where
	// per-scrape state kept on the struct shows up.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Gather(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent gather: %v", err)
	}

	if got := testutil.ToFloat64(e.scrapes); got != 16 {
		t.Errorf("pg_exporter_scrapes_total = %v, want 16", got)
	}
}

func TestDurationIsRecordedForCollectorsThatRan(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "primary-healthy.json")}
	e := New(Config{Source: fake, Limits: postgres.DefaultLimits, Timeout: time.Second, Version: testVersion})

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(e); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != "pg_exporter_scrape_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "collector" {
					seen[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	if len(seen) != len(CollectorNames) {
		t.Fatalf("durations for %v, want all of %v", seen, CollectorNames)
	}
	for name, v := range seen {
		if v < 0 || v > time.Second.Seconds() {
			t.Errorf("duration for %s = %v, outside [0, timeout]", name, v)
		}
	}
}

func TestBuildInfoCarriesTheVersion(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "empty.json")}
	e := newExporter(t, fake, postgres.DefaultLimits)
	mustHave(t, render(t, e), `pg_exporter_build_info{version="`+testVersion+`"} 1`)
}

func TestAnUnsetVersionIsNotAnEmptyLabel(t *testing.T) {
	fake := &postgres.Fake{Data: load(t, "empty.json")}
	e := New(Config{Source: fake, Limits: postgres.DefaultLimits})
	// An empty label value and an absent label are the same thing to
	// Prometheus, so a build with no version stamp would silently merge
	// with one that has none either.
	mustHave(t, render(t, e), `pg_exporter_build_info{version="unknown"} 1`)
}

func TestZeroTimeoutFallsBackToTheDefault(t *testing.T) {
	e := New(Config{Source: &postgres.Fake{}})
	if e.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", e.timeout, DefaultTimeout)
	}
}

func TestDescribeAndCollectAgree(t *testing.T) {
	// A pedantic registry rejects a collected metric whose descriptor was
	// not described. Gathering every fixture through one is the check.
	for _, fixture := range []string{"primary-healthy.json", "primary-degraded.json", "standby.json", "empty.json", "table-cap.json"} {
		t.Run(fixture, func(t *testing.T) {
			fake := &postgres.Fake{Data: load(t, fixture)}
			e := newExporter(t, fake, postgres.DefaultLimits)

			reg := prometheus.NewPedanticRegistry()
			if err := reg.Register(e); err != nil {
				t.Fatalf("register: %v", err)
			}
			families, err := reg.Gather()
			if err != nil {
				t.Fatalf("gather: %v", err)
			}

			described := map[string]bool{}
			for _, n := range describedNames(e) {
				described[n] = true
			}
			for _, mf := range families {
				if !described[mf.GetName()] {
					t.Errorf("collected %s, which Describe never sent", mf.GetName())
				}
			}
		})
	}
}
