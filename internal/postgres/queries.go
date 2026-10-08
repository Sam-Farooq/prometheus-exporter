package postgres

// The four statements the exporter runs. They are constants in one file so a
// reviewer can read every query the exporter will ever send without opening
// the connection code, and so a test can assert the filtering is still in the
// SQL rather than having quietly moved into Go.
//
// All four are read-only, all four cast to float8 or text in the query, and
// none of them takes a value from a label or a flag by string concatenation.
// Casting in SQL rather than in Go is deliberate: pg_lsn, interval and xid
// have driver-specific Go representations, and a float8 does not.
const (
	// QueryInRecovery decides which LSN the lag is measured against. On a
	// standby, pg_current_wal_lsn() raises an error rather than returning a
	// value, which would fail the whole replication scrape.
	QueryInRecovery = `SELECT pg_is_in_recovery()`

	// QueryStandbys reads one row per walsender.
	//
	// The reference LSN is pg_last_wal_receive_lsn() on a standby and
	// pg_current_wal_lsn() on a primary, so a cascading standby reports a
	// number instead of an error.
	//
	// replay_lag is an interval and is NULL until the standby reports, so
	// the epoch cast can be NULL and is scanned into a pointer.
	QueryStandbys = `
SELECT
    coalesce(nullif(application_name, ''), 'unknown')                  AS application_name,
    coalesce(state, 'unknown')                                         AS state,
    coalesce(
        pg_wal_lsn_diff(
            CASE WHEN pg_is_in_recovery()
                 THEN pg_last_wal_receive_lsn()
                 ELSE pg_current_wal_lsn()
            END,
            replay_lsn
        ), 0
    )::float8                                                          AS lag_bytes,
    extract(epoch FROM replay_lag)::float8                             AS replay_lag_seconds
FROM pg_stat_replication`

	// QuerySlots reads one row per replication slot.
	//
	// restart_lsn is NULL for a slot that has never been reserved, and the
	// retained figure is then zero rather than missing: a brand new slot is
	// holding nothing, and that is a fact rather than an absence.
	QuerySlots = `
SELECT
    slot_name,
    slot_type,
    active,
    coalesce(
        pg_wal_lsn_diff(
            CASE WHEN pg_is_in_recovery()
                 THEN pg_last_wal_receive_lsn()
                 ELSE pg_current_wal_lsn()
            END,
            restart_lsn
        ), 0
    )::float8 AS retained_wal_bytes
FROM pg_replication_slots`

	// QueryTables reads the vacuum-relevant statistics for the largest
	// tables, and reports how many tables matched before the cap.
	//
	// count(*) OVER () is what makes the cap visible: without it a top-N
	// looks exactly like a complete list. The size filter and the ordering
	// are here, in SQL, and ApplyLimits states the same rule for the fake.
	//
	// $1 is the minimum size in bytes, $2 the row cap.
	QueryTables = `
SELECT schema, "table", dead_rows, live_rows, size_bytes,
       autovacuum_threshold_rows, last_autovacuum, autovacuums, matching
FROM (
    SELECT
        s.schemaname::text                                     AS schema,
        s.relname::text                                        AS "table",
        s.n_dead_tup::float8                                   AS dead_rows,
        s.n_live_tup::float8                                   AS live_rows,
        pg_total_relation_size(s.relid)::float8                AS size_bytes,
        (
            current_setting('autovacuum_vacuum_threshold')::float8
            + current_setting('autovacuum_vacuum_scale_factor')::float8
              * s.n_live_tup::float8
        )                                                      AS autovacuum_threshold_rows,
        s.last_autovacuum                                      AS last_autovacuum,
        s.autovacuum_count::float8                             AS autovacuums,
        count(*) OVER ()                                       AS matching
    FROM pg_stat_user_tables s
    WHERE pg_total_relation_size(s.relid) >= $1
) t
ORDER BY size_bytes DESC, schema ASC, "table" ASC
LIMIT $2`

	// QueryWraparound reads the transaction id horizon per database and the
	// age at which the server starts forcing vacuums.
	//
	// Databases with datallowconn false are excluded: template0 is frozen
	// and its age is not a thing anybody can act on.
	QueryWraparound = `
SELECT
    d.datname::text                                            AS datname,
    age(d.datfrozenxid)::float8                                AS xid_age,
    current_setting('autovacuum_freeze_max_age')::float8        AS freeze_max_age
FROM pg_database d
WHERE d.datallowconn
ORDER BY d.datname`
)
