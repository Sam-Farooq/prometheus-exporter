package collector

import (
	"fmt"
	"io"
	"sort"
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

// BudgetLine is one metric's worst-case series count.
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
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].Series != lines[j].Series {
			return lines[i].Series > lines[j].Series
		}
		return lines[i].Metric < lines[j].Metric
	})
	return lines
}

// TotalSeries sums a budget.
func TotalSeries(lines []BudgetLine) int {
	total := 0
	for _, l := range lines {
		total += l.Series
	}
	return total
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
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\n", l.Metric, l.Series, l.Basis); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(tw, "\nworst case\t%d\tat %d tables, %d standbys, %d slots, %d databases\n",
		TotalSeries(lines), a.Limits.MaxTables, a.Standbys, a.Slots, a.Databases); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "table metrics\t%d\t%.0f%% of the total\n",
		6*a.Limits.MaxTables, 100*float64(6*a.Limits.MaxTables)/float64(TotalSeries(lines))); err != nil {
		return err
	}
	return tw.Flush()
}
