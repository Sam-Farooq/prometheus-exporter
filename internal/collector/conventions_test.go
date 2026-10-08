package collector

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

var metricName = regexp.MustCompile(`^[a-z][a-z0-9_]*[a-z0-9]$`)

// renderedTypes gathers every fixture once and returns the type the exposition
// declares for each metric name. Reading the type off the rendered output
// rather than off the Go call site is the point: the exposition is what
// Prometheus parses.
func renderedTypes(t *testing.T) map[string]dto.MetricType {
	t.Helper()
	out := map[string]dto.MetricType{}
	for _, fixture := range []string{"primary-healthy.json", "primary-degraded.json", "table-cap.json", "empty.json"} {
		fake := &postgres.Fake{Data: load(t, fixture)}
		e := newExporter(t, fake, postgres.DefaultLimits)
		reg := prometheus.NewPedanticRegistry()
		if err := reg.Register(e); err != nil {
			t.Fatal(err)
		}
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range families {
			out[mf.GetName()] = mf.GetType()
		}
	}
	return out
}

func TestMetricNamesFollowTheConventions(t *testing.T) {
	types := renderedTypes(t)
	e := newExporter(t, &postgres.Fake{}, postgres.DefaultLimits)

	// Abbreviated units are the commonest naming fault and the one that
	// cannot be fixed later: a dashboard and six alerts already read the
	// old name.
	bannedSuffixes := []string{"_ms", "_us", "_ns", "_kb", "_mb", "_gb", "_pct", "_percent", "_secs", "_byte"}

	for _, n := range describedNames(e) {
		if !strings.HasPrefix(n, Namespace+"_") {
			t.Errorf("%s does not start with the %s_ namespace", n, Namespace)
		}
		if !metricName.MatchString(n) {
			t.Errorf("%s is not a legal snake_case metric name", n)
		}

		// _count, _sum and _bucket are the suffixes a histogram or a
		// summary generates. A plain gauge called pg_something_count
		// collides with them in anything that parses by suffix.
		for _, reserved := range []string{"_count", "_sum", "_bucket"} {
			if strings.HasSuffix(n, reserved) {
				t.Errorf("%s ends in %s, which a histogram generates", n, reserved)
			}
		}

		for _, bad := range bannedSuffixes {
			if strings.HasSuffix(n, bad) {
				t.Errorf("%s uses an abbreviated or non-base unit", n)
			}
		}

		// A metric whose name mentions a base unit has to end with it, so
		// pg_table_size_bytes is legal and pg_bytes_table_size is not.
		for _, unit := range []string{"bytes", "seconds", "transactions"} {
			if strings.Contains(n, "_"+unit) && !strings.HasSuffix(n, "_"+unit) {
				t.Errorf("%s names the unit %s somewhere other than the end", n, unit)
			}
		}

		typ, rendered := types[n]
		if !rendered {
			t.Errorf("%s is described and never rendered by any fixture", n)
			continue
		}
		isCounter := typ == dto.MetricType_COUNTER
		hasTotal := strings.HasSuffix(n, "_total")
		if isCounter != hasTotal {
			t.Errorf("%s renders as %s and %s a _total suffix", n, typ, map[bool]string{true: "has", false: "does not have"}[hasTotal])
		}
	}
}

func TestLabelsStayInsideTheBudget(t *testing.T) {
	allowed := map[string]bool{}
	for _, l := range LabelNames {
		allowed[l] = true
	}

	labelPattern := regexp.MustCompile(`variableLabels: \{([^}]*)\}`)
	descriptors := newDescs()
	for _, d := range descriptors.all() {
		m := labelPattern.FindStringSubmatch(d.String())
		if len(m) != 2 || strings.TrimSpace(m[1]) == "" {
			continue
		}
		for _, raw := range strings.Split(m[1], ",") {
			label := strings.TrimSpace(raw)
			if label == "" {
				continue
			}
			// A label not on the list is not rejected by Prometheus. It is
			// accepted, and it multiplies the series count by its own
			// cardinality. queryid from pg_stat_statements is the example:
			// unbounded by construction, so no ceiling can be stated for
			// it at all.
			if !allowed[label] {
				t.Errorf("%s carries the label %q, which is not in LabelNames", fqNameOf(d), label)
			}
		}
	}
}

func TestHelpStringsSayWhatToConclude(t *testing.T) {
	descriptors := newDescs()
	for _, d := range descriptors.all() {
		n := fqNameOf(d)
		help := helpOf(d)
		switch {
		case help == "":
			t.Errorf("%s has no help string", n)
		case !strings.HasSuffix(help, "."):
			t.Errorf("%s: help is not a sentence: %q", n, help)
		case len(help) < 80:
			// A help string that restates the metric name tells a reader
			// looking at an unfamiliar graph nothing they did not have.
			t.Errorf("%s: help is %d characters and says nothing a reader could act on: %q", n, len(help), help)
		}
	}
}

func TestBudgetCoversEveryMetric(t *testing.T) {
	e := newExporter(t, &postgres.Fake{}, postgres.DefaultLimits)

	budgeted := map[string]bool{}
	for _, l := range SeriesBudget(DefaultAssumptions) {
		if budgeted[l.Metric] {
			t.Errorf("%s is budgeted twice", l.Metric)
		}
		budgeted[l.Metric] = true
	}
	for _, n := range describedNames(e) {
		if !budgeted[n] {
			t.Errorf("%s has no line in the series budget, so the ceiling in the README is wrong", n)
		}
	}
	for n := range budgeted {
		found := false
		for _, d := range describedNames(e) {
			if d == n {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the budget has a line for %s, which no descriptor emits", n)
		}
	}
}

func TestSeriesBudgetArithmetic(t *testing.T) {
	cases := []struct {
		name  string
		a     Assumptions
		total int
	}{
		{
			// The figure the README quotes, and the figure
			// -series-budget prints.
			name:  "defaults",
			a:     DefaultAssumptions,
			total: 3137,
		},
		{
			// Six metrics per table is 96% of the ceiling, which is why
			// the cap is the only limit the query enforces.
			name:  "the row cap is the whole number",
			a:     Assumptions{Limits: postgres.Limits{MaxTables: 50}, Standbys: 16, Slots: 32, Databases: 16},
			total: 3137 - 6*450,
		},
		{
			// The fixed cost: six state counts, four per-collector
			// series three times over, and the handful with no labels.
			name:  "one table, no standbys, no slots, no databases",
			a:     Assumptions{Limits: postgres.Limits{MaxTables: 1}, Standbys: 0, Slots: 0, Databases: 0},
			total: 25 + 6,
		},
		{
			// 0 means no LIMIT in the query, so there is no ceiling to
			// print. This used to come out as 137, which is a ceiling
			// for the configuration that has none.
			name:  "no table cap has no ceiling",
			a:     Assumptions{Limits: postgres.Limits{MaxTables: 0}, Standbys: 16, Slots: 32, Databases: 16},
			total: Unbounded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TotalSeries(SeriesBudget(tc.a)); got != tc.total {
				t.Errorf("total = %d, want %d", got, tc.total)
			}
		})
	}
}

func TestFixturesDeclareTheirOrigin(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		seen++
		f, err := postgres.LoadFixture(filepath.Join("testdata", entry.Name()))
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		// Every number in these files was chosen by the author. A fixture
		// that stops saying so is a fixture somebody will read as a
		// recording of a real server.
		for _, phrase := range []string{"Hand-written", "not captured from a server"} {
			if !strings.Contains(f.Note, phrase) {
				t.Errorf("%s: note does not contain %q: %q", entry.Name(), phrase, f.Note)
			}
		}
	}
	if seen < 5 {
		t.Errorf("found %d fixtures, expected at least 5", seen)
	}
}

func TestFixtureThresholdsMatchTheDocumentedFormula(t *testing.T) {
	// autovacuum_vacuum_threshold 50 and autovacuum_vacuum_scale_factor 0.2
	// are the PostgreSQL defaults, and the fixtures are written as if the
	// server runs them. A fixture whose threshold does not follow from its
	// live row count is internally inconsistent, and an inconsistent
	// fixture is a test that passes against a server state that cannot
	// exist.
	const (
		base  = 50.0
		scale = 0.2
	)
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		f, err := postgres.LoadFixture(filepath.Join("testdata", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, tb := range f.Tables {
			want := base + scale*tb.LiveRows
			if math.Abs(tb.AutovacuumThresholdRows-want) > 0.5 {
				t.Errorf("%s: %s.%s threshold is %v, but %v live rows gives %v",
					entry.Name(), tb.Schema, tb.Name, tb.AutovacuumThresholdRows, tb.LiveRows, want)
			}
		}
	}
}

func TestFixturesRejectAnUnknownField(t *testing.T) {
	// A misspelled field name decodes to a zero value and the test that
	// depended on it passes against nothing.
	dir := t.TempDir()
	path := filepath.Join(dir, "typo.json")
	bad := map[string]any{"note": "Hand-written, not captured from a server.", "wraparround": map[string]any{}}
	raw, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.LoadFixture(path); err == nil {
		t.Fatal("a fixture with a misspelled field loaded without complaint")
	}
}

func TestHelpOfReadsBackWhatWentIn(t *testing.T) {
	cases := []struct {
		name string
		help string
	}{
		{"plain", "A sentence."},
		{"with a quoted phrase", `Mentions "pg_up" by name.`},
		{"with a brace", "Mentions {curly} braces."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := prometheus.NewDesc("pg_test_metric", tc.help, nil, nil)
			if got := helpOf(d); got != tc.help {
				t.Errorf("helpOf() = %q, want %q", got, tc.help)
			}
			if got := fqNameOf(d); got != "pg_test_metric" {
				t.Errorf("fqNameOf() = %q", got)
			}
		})
	}
}

func TestUnboundedBudgetSortsFirstAndSaysSo(t *testing.T) {
	lines := SeriesBudget(Assumptions{Limits: postgres.Limits{MaxTables: 0}, Standbys: 4, Slots: 4, Databases: 4})

	// An unbounded line at -1 would sort to the bottom of a descending
	// comparison, which is where a reader looks last.
	unbounded := 0
	for i, l := range lines {
		if l.Series != Unbounded {
			continue
		}
		unbounded++
		if i >= 6 {
			t.Errorf("%s is unbounded and sorted to position %d", l.Metric, i)
		}
		if l.Basis != "every table on the server" {
			t.Errorf("%s basis = %q", l.Metric, l.Basis)
		}
	}
	if unbounded != 6 {
		t.Errorf("%d unbounded lines, want the six table metrics", unbounded)
	}

	var buf strings.Builder
	if err := WriteBudget(&buf, Assumptions{Limits: postgres.Limits{MaxTables: 0}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"unbounded", "no table cap", "set -collector.bloat.max-tables to get a ceiling"} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteBudget output is missing %q:\n%s", want, out)
		}
	}
	// And it must not print a number that reads as a ceiling.
	if strings.Contains(out, "worst case     137") {
		t.Error("the budget printed a ceiling for a configuration that has none")
	}
}
