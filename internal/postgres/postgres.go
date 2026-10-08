// Package postgres holds the read side of the exporter: the row shapes, the
// queries that produce them, one implementation over database/sql, and a
// hand-written fake so the collectors can be tested with no server running.
//
// Nothing in this package formats a metric. The collectors own that, so the
// only thing that has to be true here is that a row means what its field
// names say.
package postgres

import (
	"context"
	"sort"
	"time"
)

// Limits bound how many table series one scrape can produce. They are applied
// by the query, not after it, so a server with 40,000 tables does not send
// 40,000 rows over the wire to have most of them discarded.
type Limits struct {
	// MinTableBytes drops tables smaller than this. A 200-row lookup table
	// has dead rows too and nobody is ever going to vacuum it by hand.
	MinTableBytes int64
	// MaxTables caps the result after the size ordering. Reaching it means
	// the table metrics are a top-N rather than a complete picture, which
	// the exporter reports rather than hides.
	MaxTables int
}

// DefaultLimits keep a scrape of a large server inside a few thousand series.
// The reasoning for both numbers is in the README.
var DefaultLimits = Limits{MinTableBytes: 8 << 20, MaxTables: 500}

// Standby is one walsender, from pg_stat_replication.
type Standby struct {
	// ApplicationName is set by the standby's own connection string and is
	// not unique: two standbys that never set it both arrive as
	// "walreceiver". Folding duplicates is the collector's problem.
	ApplicationName string `json:"application_name"`
	// State is one of startup, catchup, streaming, backup, stopping.
	State string `json:"state"`
	// LagBytes is WAL written on this server that the standby has not
	// replayed, from LSN arithmetic. Always available.
	LagBytes float64 `json:"lag_bytes"`
	// ReplayLagSeconds is what the standby last reported. It is nil when
	// the standby has not reported since the last WAL write, which is why
	// LagBytes is the metric worth alerting on.
	ReplayLagSeconds *float64 `json:"replay_lag_seconds"`
}

// Slot is one row of pg_replication_slots.
type Slot struct {
	Name string `json:"slot_name"`
	// Type is physical or logical.
	Type string `json:"slot_type"`
	// Active is false when no consumer is attached. An inactive slot still
	// pins WAL, so RetainedWALBytes only grows while this is false.
	Active bool `json:"active"`
	// RetainedWALBytes is WAL the server is keeping for this slot.
	RetainedWALBytes float64 `json:"retained_wal_bytes"`
}

// Replication is one scrape of the replication state.
type Replication struct {
	// InRecovery is true on a standby. It changes which LSN the lag is
	// measured against, so the query reports it rather than guessing.
	InRecovery bool      `json:"in_recovery"`
	Standbys   []Standby `json:"standbys"`
	Slots      []Slot    `json:"slots"`
}

// Table is one row of pg_stat_user_tables plus its on-disk size.
type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"table"`
	// DeadRows and LiveRows are the statistics collector's estimates. They
	// are not counts, and after a stats reset they are both zero on a table
	// holding a billion rows.
	DeadRows  float64 `json:"dead_rows"`
	LiveRows  float64 `json:"live_rows"`
	SizeBytes float64 `json:"size_bytes"`
	// AutovacuumThresholdRows is autovacuum_vacuum_threshold plus
	// autovacuum_vacuum_scale_factor times LiveRows, computed from the
	// server-wide settings. A table carrying its own reloptions is wrong
	// here, which the README names.
	AutovacuumThresholdRows float64 `json:"autovacuum_threshold_rows"`
	// LastAutovacuum is nil for a table autovacuum has never processed.
	LastAutovacuum *time.Time `json:"last_autovacuum"`
	Autovacuums    float64    `json:"autovacuums"`
}

// Bloat is one scrape of the table statistics.
type Bloat struct {
	Tables []Table `json:"tables"`
	// Matching is how many tables passed MinTableBytes before MaxTables was
	// applied. Tables beyond the cap are counted, not silently gone.
	Matching int `json:"matching"`
}

// Omitted is the number of tables that matched the size filter and fell
// outside the row cap.
func (b Bloat) Omitted() float64 {
	n := b.Matching - len(b.Tables)
	if n < 0 {
		return 0
	}
	return float64(n)
}

// Database is one row of pg_database.
type Database struct {
	Name string `json:"datname"`
	// XIDAge is transactions since this database's oldest unfrozen XID.
	XIDAge float64 `json:"xid_age"`
}

// Wraparound is one scrape of the transaction id horizon.
type Wraparound struct {
	Databases []Database `json:"databases"`
	// FreezeMaxAge is autovacuum_freeze_max_age, the age at which the
	// server starts forcing anti-wraparound vacuums.
	FreezeMaxAge float64 `json:"freeze_max_age"`
}

// Source is everything the collectors need from a server. It exists so the
// collectors can be tested against Fake with no network, no credentials and
// no container.
type Source interface {
	// Ping decides pg_up. It is separate from the three queries because a
	// server that answers but refuses one view is a different failure from
	// a server that is gone, and the two should not look the same.
	Ping(ctx context.Context) error
	Replication(ctx context.Context) (Replication, error)
	Bloat(ctx context.Context, lim Limits) (Bloat, error)
	Wraparound(ctx context.Context) (Wraparound, error)
	Close() error
}

// ApplyLimits is the row-selection rule: drop anything under MinTableBytes,
// order by size descending, keep MaxTables. The real source expresses the same
// rule in SQL. Two statements of one rule can drift, so the duplication is
// named in the README and TestQueriesStillFilter checks the SQL has not lost
// its half of it.
//
// MaxTables of zero or less means no cap.
func ApplyLimits(tables []Table, lim Limits) (kept []Table, matching int) {
	matched := make([]Table, 0, len(tables))
	for _, t := range tables {
		if t.SizeBytes >= float64(lim.MinTableBytes) {
			matched = append(matched, t)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].SizeBytes != matched[j].SizeBytes {
			return matched[i].SizeBytes > matched[j].SizeBytes
		}
		if matched[i].Schema != matched[j].Schema {
			return matched[i].Schema < matched[j].Schema
		}
		return matched[i].Name < matched[j].Name
	})
	matching = len(matched)
	if lim.MaxTables > 0 && len(matched) > lim.MaxTables {
		matched = matched[:lim.MaxTables]
	}
	return matched, matching
}
