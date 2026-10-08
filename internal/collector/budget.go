package collector

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"text/tabwriter"

	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

// Assumptions are the counts the worst case is computed from.
//
// Three of the four are not enforced by anything in this exporter: a server
// can have more slots than Slots says. They are the shape of a server this is
// expected to run against, and the number they produce is a ceiling under
// those assumptions rather than a limit the code imposes. MaxTables is the
// exception: that one the query enforces.
type Assumptions struct {
	Limits    postgres.Limits
	Standbys  int
	Slots     int
	Databases int
}

// DefaultAssumptions is the shape quoted in the README.
var DefaultAssumptions = Assumptions{
	Limits:    postgres.DefaultLimits,
	Standbys:  16,
	Slots:     32,
	Databases: 16,
}

// Unbounded is the Series value for a metric whose count nothing in this
// exporter limits.
//
// -collector.bloat.max-tables 0 means no LIMIT in the query, so the table
// series count is whatever the server happens to hold. Reporting that as zero
// was worse than reporting nothing: the budget printed a ceiling of 137 for
// the configuration with no ceiling at all.
const Unbounded = -1

// BudgetLine is one metric's worst-case series count, or Unbounded.
type BudgetLine struct {
	Metric string
	Series int
	Basis  string
}

// SeriesBudget is the worst case per metric, largest first.
//
// Cardinality is the thing that kills a Prometheus, and it is a product, not a
// sum: one label carrying a value per customer multiplies every other label by
// the number of customers. So the number is worth computing rather than
// guessing, and worth computing from the code rather than from a comment,
// which is why TestBudgetCoversEveryMetric refuses a descriptor that has no
// line here.
func SeriesBudget(a Assumptions) []BudgetLine {
	tables := a.Limits.MaxTables
	perTable := func(metric string) BudgetLine {
		if tables <= 0 {
			return BudgetLine{metric, Unbounded, "every table on the server"}
		}
		return BudgetLine{metric, tables, fmt.Sprintf("%d tables", tables)}
	}
	perStandby := func(metric string) BudgetLine {
		return BudgetLine{metric, a.Standbys, fmt.Sprintf("%d standbys", a.Standbys)}
	}
	perSlot := func(metric string) BudgetLine {
		return BudgetLine{metric, a.Slots, fmt.Sprintf("%d slots", a.Slots)}
	}
	collectors := len(CollectorNames)

	lines := []BudgetLine{
		perTable(name("table", "dead_rows")),
		perTable(name("table", "live_rows")),
		perTable(name("table", "size_bytes")),
		perTable(name("table", "autovacuum_threshold_rows")),
		perTable(name("table", "last_autovacuum_timestamp_seconds")),
		perTable(name("table", "autovacuums_total")),
		perSlot(name("replication", "slot_active")),
		perSlot(name("replication", "slot_retained_wal_bytes")),
		perStandby(name("replication", "standby_lag_bytes")),
		perStandby(name("replication", "standby_replay_lag_seconds")),
		{name("database", "xid_age_transactions"), a.Databases, fmt.Sprintf("%d databases", a.Databases)},
		{name("replication", "standbys"), len(ReplicationStates), "every state, always"},
		{name("exporter", "collector_success"), collectors, "one per collector"},
		{name("exporter", "scrape_duration_seconds"), collectors, "one per collector"},
		{name("exporter", "scrape_errors_total"), collectors, "one per collector"},
		{name("up"), 1, "no labels"},
		{name("in_recovery"), 1, "no labels"},
		{name("replication", "standbys_folded"), 1, "no labels"},
		{name("table", "series_omitted"), 1, "no labels"},
		{name("autovacuum", "freeze_max_age_transactions"), 1, "no labels"},
		{name("exporter", "build_info"), 1, "one build"},
		{name("exporter", "scrapes_total"), 1, "no labels"},
	}
	// Unbounded sorts first, not last, which is where a plain descending
	// comparison on -1 would put it.
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		if (a.Series == Unbounded) != (b.Series == Unbounded) {
			return a.Series == Unbounded
		}
		if a.Series != b.Series {
			return a.Series > b.Series
		}
		return a.Metric < b.Metric
	})
	return lines
}

// TotalSeries sums a budget, or returns Unbounded if any line is unbounded.
// A sum that silently drops the unbounded term is a smaller number than the
// truth, which is the direction that gets somebody hurt.
func TotalSeries(lines []BudgetLine) int {
	total := 0
	for _, l := range lines {
		if l.Series == Unbounded {
			return Unbounded
		}
		total += l.Series
	}
	return total
}

func seriesText(n int) string {
	if n == Unbounded {
		return "unbounded"
	}
	return strconv.Itoa(n)
}

// WriteBudget prints the budget as a table. This is what -series-budget shows,
// so the figure in the README is a command's output and not a claim.
func WriteBudget(w io.Writer, a Assumptions) error {
	lines := SeriesBudget(a)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "metric\tseries\tfrom\n"); err != nil {
		return err
	}
	for _, l := range lines {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", l.Metric, seriesText(l.Series), l.Basis); err != nil {
			return err
		}
	}

	total := TotalSeries(lines)
	tableCap := fmt.Sprintf("%d tables", a.Limits.MaxTables)
	if a.Limits.MaxTables <= 0 {
		tableCap = "no table cap"
	}
	if _, err := fmt.Fprintf(tw, "\nworst case\t%s\tat %s, %d standbys, %d slots, %d databases\n",
		seriesText(total), tableCap, a.Standbys, a.Slots, a.Databases); err != nil {
		return err
	}

	if total == Unbounded {
		_, err := fmt.Fprintf(tw, "table metrics\tunbounded\tset -collector.bloat.max-tables to get a ceiling\n")
		if err != nil {
			return err
		}
		return tw.Flush()
	}
	if _, err := fmt.Fprintf(tw, "table metrics\t%d\t%.0f%% of the total\n",
		6*a.Limits.MaxTables, 100*float64(6*a.Limits.MaxTables)/float64(total)); err != nil {
		return err
	}
	return tw.Flush()
}
