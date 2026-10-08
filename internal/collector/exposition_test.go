package collector

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

// The golden files hold the exposition text, because that text is the
// contract. A test that asserts on a Go struct passes while the rendered
// output carries the wrong metric name, the wrong label or a counter typed as
// a gauge, and the rendered output is the only thing Prometheus reads.
var update = flag.Bool("update", false, "rewrite the golden exposition files in testdata")

// errDenied stands in for the failure a reviewer will actually hit: the
// monitoring role was created without pg_monitor, so the server is up and one
// view is not readable.
var errDenied = errors.New("permission denied for view pg_stat_replication")

const testVersion = "0.16.4-test"

// goldenNames is every metric the golden files compare, which is everything
// except the per-collector duration. Wall time is not reproducible and a
// golden file that contains it fails on a slow runner for no reason;
// TestDurationIsRecorded covers it separately.
func goldenNames(t *testing.T, e *Exporter) []string {
	t.Helper()
	var out []string
	for _, n := range describedNames(e) {
		if n == "pg_exporter_scrape_duration_seconds" {
			continue
		}
		out = append(out, n)
	}
	return out
}

func describedNames(e *Exporter) []string {
	ch := make(chan *prometheus.Desc, 64)
	go func() {
		e.Describe(ch)
		close(ch)
	}()
	seen := map[string]bool{}
	for d := range ch {
		seen[fqNameOf(d)] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func load(t *testing.T, fixture string) postgres.Fixture {
	t.Helper()
	f, err := postgres.LoadFixture(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("load %s: %v", fixture, err)
	}
	return f
}

func newExporter(t *testing.T, f *postgres.Fake, lim postgres.Limits) *Exporter {
	t.Helper()
	return New(Config{Source: f, Limits: lim, Version: testVersion})
}

func TestExposition(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		golden  string
		limits  postgres.Limits
		mutate  func(*postgres.Fake)
	}{
		{
			name:    "a healthy primary",
			fixture: "primary-healthy.json",
			golden:  "primary-healthy.prom",
			limits:  postgres.DefaultLimits,
		},
		{
			name:    "duplicate standby names, inactive slots, a table never vacuumed",
			fixture: "primary-degraded.json",
			golden:  "primary-degraded.prom",
			limits:  postgres.DefaultLimits,
		},
		{
			name:    "a standby with nothing replicating from it",
			fixture: "standby.json",
			golden:  "standby.prom",
			limits:  postgres.DefaultLimits,
		},
		{
			name:    "every query returns no rows",
			fixture: "empty.json",
			golden:  "empty.prom",
			limits:  postgres.DefaultLimits,
		},
		{
			name:    "the row cap turns the table metrics into a top-N",
			fixture: "table-cap.json",
			golden:  "table-cap.prom",
			limits:  postgres.Limits{MinTableBytes: 8 << 20, MaxTables: 2},
		},
		{
			name:    "identifiers the exposition format has to escape",
			fixture: "odd-identifiers.json",
			golden:  "odd-identifiers.prom",
			limits:  postgres.DefaultLimits,
		},
		{
			name:    "the server does not answer",
			fixture: "primary-healthy.json",
			golden:  "down.prom",
			limits:  postgres.DefaultLimits,
			mutate:  func(f *postgres.Fake) { f.PingErr = errors.New("dial tcp 10.0.3.14:5432: connect: connection refused") },
		},
		{
			name:    "the role cannot read pg_stat_replication",
			fixture: "primary-healthy.json",
			golden:  "replication-denied.prom",
			limits:  postgres.DefaultLimits,
			mutate:  func(f *postgres.Fake) { f.ReplicationErr = errDenied },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &postgres.Fake{Data: load(t, tc.fixture)}
			if tc.mutate != nil {
				tc.mutate(fake)
			}
			e := newExporter(t, fake, tc.limits)

			names := goldenNames(t, e)
			path := filepath.Join("testdata", tc.golden)

			if *update {
				var buf bytes.Buffer
				if err := writeExposition(&buf, e, names); err != nil {
					t.Fatalf("render: %v", err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				t.Logf("rewrote %s", path)
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run go test ./... -update to create it): %v", err)
			}
			if err := testutil.CollectAndCompare(e, bytes.NewReader(want), names...); err != nil {
				t.Error(err)
			}
		})
	}
}

// writeExposition renders one scrape the way /metrics would.
func writeExposition(buf *bytes.Buffer, e *Exporter, names []string) error {
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(e); err != nil {
		return err
	}
	return gatherInto(buf, reg, names)
}

func TestGoldenFilesCoverEveryDescriptor(t *testing.T) {
	// A metric nobody renders is a metric nobody has read. The reverse
	// direction of TestDescribeAndCollectAgree: every descriptor has to show
	// up in at least one golden file, or the help string has never been
	// checked against a value.
	fake := &postgres.Fake{Data: load(t, "primary-healthy.json")}
	e := newExporter(t, fake, postgres.DefaultLimits)

	rendered := map[string]bool{}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".prom") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("testdata", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if after, ok := strings.CutPrefix(line, "# TYPE "); ok {
				rendered[strings.Fields(after)[0]] = true
			}
		}
	}

	for _, n := range goldenNames(t, e) {
		if !rendered[n] {
			t.Errorf("%s is described and appears in no golden file", n)
		}
	}
}
