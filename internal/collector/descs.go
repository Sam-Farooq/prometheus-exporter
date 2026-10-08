package collector

import "github.com/prometheus/client_golang/prometheus"

// Namespace prefixes every metric this exporter emits.
const Namespace = "pg"

// The four scrape units. Every one of them has its own success gauge, its own
// duration gauge and its own entry in pg_exporter_scrape_errors_total, because
// "the exporter failed" is not an answer anybody can act on.
const (
	CollectorConnect     = "connect"
	CollectorReplication = "replication"
	CollectorBloat       = "bloat"
	CollectorWraparound  = "wraparound"
)

// CollectorNames is the complete, closed set of values the collector label can
// take. It is closed on purpose: the label budget in the README is only a
// budget if nothing can add a value to it at runtime.
var CollectorNames = []string{
	CollectorConnect,
	CollectorReplication,
	CollectorBloat,
	CollectorWraparound,
}

// LabelNames is every label this exporter is allowed to attach to anything.
// TestLabelsStayInsideTheBudget holds every descriptor to it. A label not on
// this list is not rejected by Prometheus, it is accepted and multiplies the
// series count by its cardinality, which is why the list is a test and not a
// comment.
var LabelNames = []string{
	"application_name",
	"collector",
	"datname",
	"schema",
	"slot_name",
	"slot_type",
	"state",
	"table",
	"version",
}

// descs holds every descriptor the exporter will ever send. Describe walks
// this struct and Collect reads from it, so the two cannot disagree about
// which metrics exist: there is one list.
type descs struct {
	up        *prometheus.Desc
	buildInfo *prometheus.Desc

	collectorSuccess  *prometheus.Desc
	collectorDuration *prometheus.Desc

	inRecovery      *prometheus.Desc
	standbys        *prometheus.Desc
	standbyLagBytes *prometheus.Desc
	standbyLagSecs  *prometheus.Desc
	standbysFolded  *prometheus.Desc
	slotActive      *prometheus.Desc
	slotRetained    *prometheus.Desc

	tableDeadRows      *prometheus.Desc
	tableLiveRows      *prometheus.Desc
	tableSizeBytes     *prometheus.Desc
	tableVacThreshold  *prometheus.Desc
	tableLastAutovac   *prometheus.Desc
	tableAutovacsTotal *prometheus.Desc
	tableOmitted       *prometheus.Desc

	databaseXIDAge *prometheus.Desc
	freezeMaxAge   *prometheus.Desc
}

func name(parts ...string) string {
	out := Namespace
	for _, p := range parts {
		out += "_" + p
	}
	return out
}

func newDescs() descs {
	return descs{
		up: prometheus.NewDesc(
			name("up"),
			"1 when the server answered a ping inside the scrape timeout. 0 means every value below it is missing for a reason, which is the difference between a down database and a quiet one.",
			nil, nil,
		),
		buildInfo: prometheus.NewDesc(
			name("exporter", "build_info"),
			"Always 1. The version label is the exporter build serving this endpoint, so a stale binary on one host is visible without logging in.",
			[]string{"version"}, nil,
		),

		collectorSuccess: prometheus.NewDesc(
			name("exporter", "collector_success"),
			"1 when this collector's queries all returned. A 0 here with pg_up at 1 is a permission or a timeout on one view, not an unreachable server.",
			[]string{"collector"}, nil,
		),
		collectorDuration: prometheus.NewDesc(
			name("exporter", "scrape_duration_seconds"),
			"Wall time the last scrape of this collector took. Compare it with the Prometheus scrape_timeout: a collector approaching it is about to start failing in a way that looks like the database went away.",
			[]string{"collector"}, nil,
		),

		inRecovery: prometheus.NewDesc(
			name("in_recovery"),
			"1 when this server is a standby. The replication metrics below describe outbound replication, so on a standby they describe cascading replicas rather than the server's own lag.",
			nil, nil,
		),
		standbys: prometheus.NewDesc(
			name("replication", "standbys"),
			"Walsenders in each replication state. Alert on the streaming count falling, not on lag rising: a standby that disconnects stops reporting lag entirely and its lag series goes away.",
			[]string{"state"}, nil,
		),
		standbyLagBytes: prometheus.NewDesc(
			name("replication", "standby_lag_bytes"),
			"WAL bytes written on this server that the standby has not replayed, from LSN arithmetic. Always available, unlike the seconds form, so this is the one to alert on.",
			[]string{"application_name"}, nil,
		),
		standbyLagSecs: prometheus.NewDesc(
			name("replication", "standby_replay_lag_seconds"),
			"Delay between a commit here and its replay on the standby, as the standby last reported it. Absent when the standby has not reported since the last WAL write, so a missing value is silence and not zero.",
			[]string{"application_name"}, nil,
		),
		standbysFolded: prometheus.NewDesc(
			name("replication", "standbys_folded"),
			"Walsenders that shared an application_name with another and were merged into one series, keeping the larger lag. Non-zero means two standbys are reporting as one, and the fix is a distinct application_name in their primary_conninfo.",
			nil, nil,
		),
		slotActive: prometheus.NewDesc(
			name("replication", "slot_active"),
			"1 when a consumer is attached to this slot. A 0 on a logical slot is the condition under which retained WAL grows without limit until the data directory fills.",
			[]string{"slot_name", "slot_type"}, nil,
		),
		slotRetained: prometheus.NewDesc(
			name("replication", "slot_retained_wal_bytes"),
			"WAL the server is keeping because this slot has not consumed it. This is the number that fills a disk, and it is retained whether or not anything is listening.",
			[]string{"slot_name", "slot_type"}, nil,
		),

		tableDeadRows: prometheus.NewDesc(
			name("table", "dead_rows"),
			"Dead rows the statistics collector believes this table holds. An estimate, not a count, and zero immediately after a statistics reset on a table that is full of them.",
			[]string{"schema", "table"}, nil,
		),
		tableLiveRows: prometheus.NewDesc(
			name("table", "live_rows"),
			"Live rows the statistics collector believes this table holds. Present because the autovacuum threshold is a function of it, so dead rows alone cannot be read.",
			[]string{"schema", "table"}, nil,
		),
		tableSizeBytes: prometheus.NewDesc(
			name("table", "size_bytes"),
			"Total relation size including indexes and TOAST. Growth with a flat live row count is bloat taking space that a vacuum will not give back to the filesystem.",
			[]string{"schema", "table"}, nil,
		),
		tableVacThreshold: prometheus.NewDesc(
			name("table", "autovacuum_threshold_rows"),
			"Dead rows at which autovacuum would pick this table up, from the server-wide threshold and scale factor. A table over its threshold and not recently vacuumed is the actionable case; dead rows on their own are not.",
			[]string{"schema", "table"}, nil,
		),
		tableLastAutovac: prometheus.NewDesc(
			name("table", "last_autovacuum_timestamp_seconds"),
			"Unix time of the last autovacuum of this table. Absent for a table autovacuum has never processed, which on a large table is itself the finding.",
			[]string{"schema", "table"}, nil,
		),
		tableAutovacsTotal: prometheus.NewDesc(
			name("table", "autovacuums_total"),
			"Autovacuums of this table since the last statistics reset. A rate of zero on a table above its threshold means autovacuum is not reaching it.",
			[]string{"schema", "table"}, nil,
		),
		tableOmitted: prometheus.NewDesc(
			name("table", "series_omitted"),
			"Tables that passed the size filter and fell outside the row cap. Non-zero means the table metrics above are a top-N by size, not every table on the server.",
			nil, nil,
		),

		databaseXIDAge: prometheus.NewDesc(
			name("database", "xid_age_transactions"),
			"Transactions since this database's oldest unfrozen transaction id. Divide by pg_autovacuum_freeze_max_age_transactions to see how close the server is to forcing vacuums; at two billion it stops accepting writes.",
			[]string{"datname"}, nil,
		),
		freezeMaxAge: prometheus.NewDesc(
			name("autovacuum", "freeze_max_age_transactions"),
			"The server's autovacuum_freeze_max_age. Exported rather than assumed because the default is not what a tuned server runs, and the ratio is meaningless against the wrong denominator.",
			nil, nil,
		),
	}
}

// all returns every descriptor, which is what Describe sends.
func (d descs) all() []*prometheus.Desc {
	return []*prometheus.Desc{
		d.up, d.buildInfo,
		d.collectorSuccess, d.collectorDuration,
		d.inRecovery, d.standbys, d.standbyLagBytes, d.standbyLagSecs,
		d.standbysFolded, d.slotActive, d.slotRetained,
		d.tableDeadRows, d.tableLiveRows, d.tableSizeBytes,
		d.tableVacThreshold, d.tableLastAutovac, d.tableAutovacsTotal,
		d.tableOmitted,
		d.databaseXIDAge, d.freezeMaxAge,
	}
}
