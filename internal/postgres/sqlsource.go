package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// pgx registers the "pgx" driver with database/sql. It is the only
	// reason this import exists; every query above goes through
	// database/sql, so swapping the driver changes this line and nothing
	// else.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// SQLSource reads a live server through database/sql.
//
// There is no test in this repo that exercises it, because a test that
// exercises it needs a PostgreSQL server and this suite runs with no network.
// What is tested is everything above it: the row shapes, the limit rule, and
// every decision the collectors make about the rows. The untested surface is
// four Query calls and a Scan per column, which is the smallest it can be
// made, and the README says so rather than implying coverage it does not have.
type SQLSource struct {
	db *sql.DB
}

// PoolSize bounds the connections the exporter will open. An exporter that
// opens a connection per scrape and never closes it is a slow way to run a
// server out of backends, and max_connections is shared with the application.
const PoolSize = 2

// Open returns a source over the given DSN. It does not connect: database/sql
// connects lazily, so a server that is down at start time does not stop the
// exporter from starting and reporting pg_up 0, which is the whole point of
// having a pg_up.
func Open(dsn string) (*SQLSource, error) {
	if dsn == "" {
		return nil, errors.New("postgres: empty DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	db.SetMaxOpenConns(PoolSize)
	db.SetMaxIdleConns(PoolSize)
	// A connection kept forever survives a failover and keeps talking to
	// the old primary's address through a stale DNS answer.
	db.SetConnMaxLifetime(10 * time.Minute)
	return &SQLSource{db: db}, nil
}

// Ping decides pg_up.
func (s *SQLSource) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *SQLSource) Close() error {
	return s.db.Close()
}

// Replication reads the walsenders and the slots.
func (s *SQLSource) Replication(ctx context.Context) (Replication, error) {
	var out Replication

	if err := s.db.QueryRowContext(ctx, QueryInRecovery).Scan(&out.InRecovery); err != nil {
		return out, fmt.Errorf("postgres: in recovery: %w", err)
	}

	standbys, err := s.db.QueryContext(ctx, QueryStandbys)
	if err != nil {
		return out, fmt.Errorf("postgres: standbys: %w", err)
	}
	defer func() { _ = standbys.Close() }()
	for standbys.Next() {
		var row Standby
		if err := standbys.Scan(&row.ApplicationName, &row.State, &row.LagBytes, &row.ReplayLagSeconds); err != nil {
			return out, fmt.Errorf("postgres: standby row: %w", err)
		}
		out.Standbys = append(out.Standbys, row)
	}
	if err := standbys.Err(); err != nil {
		return out, fmt.Errorf("postgres: standbys: %w", err)
	}

	slots, err := s.db.QueryContext(ctx, QuerySlots)
	if err != nil {
		return out, fmt.Errorf("postgres: slots: %w", err)
	}
	defer func() { _ = slots.Close() }()
	for slots.Next() {
		var row Slot
		if err := slots.Scan(&row.Name, &row.Type, &row.Active, &row.RetainedWALBytes); err != nil {
			return out, fmt.Errorf("postgres: slot row: %w", err)
		}
		out.Slots = append(out.Slots, row)
	}
	if err := slots.Err(); err != nil {
		return out, fmt.Errorf("postgres: slots: %w", err)
	}

	return out, nil
}

// Bloat reads the vacuum statistics for the tables inside lim.
func (s *SQLSource) Bloat(ctx context.Context, lim Limits) (Bloat, error) {
	var out Bloat

	// A NULL LIMIT is LIMIT ALL in PostgreSQL. A negative one is an error,
	// so an uncapped run passes NULL rather than -1.
	var rowCap any
	if lim.MaxTables > 0 {
		rowCap = lim.MaxTables
	}
	rows, err := s.db.QueryContext(ctx, QueryTables, lim.MinTableBytes, rowCap)
	if err != nil {
		return out, fmt.Errorf("postgres: tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			row      Table
			lastVac  sql.NullTime
			matching int
		)
		if err := rows.Scan(
			&row.Schema, &row.Name, &row.DeadRows, &row.LiveRows, &row.SizeBytes,
			&row.AutovacuumThresholdRows, &lastVac, &row.Autovacuums, &matching,
		); err != nil {
			return out, fmt.Errorf("postgres: table row: %w", err)
		}
		if lastVac.Valid {
			t := lastVac.Time
			row.LastAutovacuum = &t
		}
		out.Matching = matching
		out.Tables = append(out.Tables, row)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("postgres: tables: %w", err)
	}

	return out, nil
}

// Wraparound reads the transaction id horizon.
func (s *SQLSource) Wraparound(ctx context.Context) (Wraparound, error) {
	var out Wraparound

	rows, err := s.db.QueryContext(ctx, QueryWraparound)
	if err != nil {
		return out, fmt.Errorf("postgres: wraparound: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row Database
		if err := rows.Scan(&row.Name, &row.XIDAge, &out.FreezeMaxAge); err != nil {
			return out, fmt.Errorf("postgres: database row: %w", err)
		}
		out.Databases = append(out.Databases, row)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("postgres: wraparound: %w", err)
	}

	return out, nil
}
