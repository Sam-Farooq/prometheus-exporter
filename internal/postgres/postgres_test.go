package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

func tbl(schema, name string, size float64) postgres.Table {
	return postgres.Table{Schema: schema, Name: name, SizeBytes: size}
}

func names(tables []postgres.Table) []string {
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		out = append(out, t.Schema+"."+t.Name)
	}
	return out
}

func TestApplyLimits(t *testing.T) {
	t.Parallel()

	all := []postgres.Table{
		tbl("public", "events", 900),
		tbl("public", "users", 100),
		tbl("billing", "invoices", 500),
		tbl("public", "lookup", 10),
		// Two tables of exactly equal size, to pin the tie-break. Without
		// one the kept set for a cap of 3 would depend on map or scan
		// order, and a series that appears and disappears between scrapes
		// for no reason is worse than one that is absent.
		tbl("zeta", "same", 500),
	}

	cases := []struct {
		name     string
		lim      postgres.Limits
		want     []string
		matching int
	}{
		{
			name:     "no filter and no cap keeps everything, largest first",
			lim:      postgres.Limits{},
			want:     []string{"public.events", "billing.invoices", "zeta.same", "public.users", "public.lookup"},
			matching: 5,
		},
		{
			name:     "size filter is inclusive at the boundary",
			lim:      postgres.Limits{MinTableBytes: 500},
			want:     []string{"public.events", "billing.invoices", "zeta.same"},
			matching: 3,
		},
		{
			name:     "cap keeps the largest and still counts the rest",
			lim:      postgres.Limits{MaxTables: 2},
			want:     []string{"public.events", "billing.invoices"},
			matching: 5,
		},
		{
			name:     "equal sizes break by schema then name, not by input order",
			lim:      postgres.Limits{MinTableBytes: 500, MaxTables: 2},
			want:     []string{"public.events", "billing.invoices"},
			matching: 3,
		},
		{
			name:     "a cap of zero is not a cap of nothing",
			lim:      postgres.Limits{MaxTables: 0, MinTableBytes: 900},
			want:     []string{"public.events"},
			matching: 1,
		},
		{
			name:     "a filter nothing passes returns an empty set, not an error",
			lim:      postgres.Limits{MinTableBytes: 1 << 40},
			want:     []string{},
			matching: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kept, matching := postgres.ApplyLimits(all, tc.lim)
			got := names(kept)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("kept = %v, want %v", got, tc.want)
			}
			if matching != tc.matching {
				t.Errorf("matching = %d, want %d", matching, tc.matching)
			}
		})
	}
}

func TestApplyLimitsDoesNotReorderTheCaller(t *testing.T) {
	t.Parallel()

	// The collector holds no reference to the slice it passed, but the fake
	// passes the fixture's own slice on every scrape. Sorting that in place
	// would make the second scrape of a fixture differ from the first.
	in := []postgres.Table{tbl("a", "small", 1), tbl("b", "big", 100)}
	postgres.ApplyLimits(in, postgres.Limits{})
	if in[0].Name != "small" {
		t.Fatalf("input was reordered: %v", names(in))
	}
}

func TestOmitted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		b    postgres.Bloat
		want float64
	}{
		{"nothing omitted", postgres.Bloat{Tables: make([]postgres.Table, 3), Matching: 3}, 0},
		{"four over the cap", postgres.Bloat{Tables: make([]postgres.Table, 6), Matching: 10}, 4},
		{"matching unset cannot go negative", postgres.Bloat{Tables: make([]postgres.Table, 2)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.b.Omitted(); got != tc.want {
				t.Errorf("Omitted() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestQueriesStillFilter is the other half of the duplication ApplyLimits
// creates. The rule lives twice, once in Go for the fake and once in SQL for
// the server, and the failure mode is somebody moving the filter into Go and
// leaving the exporter pulling every table in the database over the wire.
func TestQueriesStillFilter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "tables filters, orders and caps in SQL",
			query: postgres.QueryTables,
			want:  []string{">= $1", "ORDER BY size_bytes DESC", "LIMIT $2", "count(*) OVER ()"},
		},
		{
			// Without this the whole replication scrape fails on a
			// cascading standby, where pg_current_wal_lsn() errors.
			name:  "standby lag switches LSN on recovery state",
			query: postgres.QueryStandbys,
			want:  []string{"pg_is_in_recovery()", "pg_last_wal_receive_lsn()", "pg_current_wal_lsn()", "::float8"},
		},
		{
			name:  "slots coalesce an unreserved restart_lsn to zero",
			query: postgres.QuerySlots,
			want:  []string{"restart_lsn", "coalesce("},
		},
		{
			// template0 is frozen. Reporting its age means an alert on
			// xid age fires on a database nobody can connect to.
			name:  "wraparound excludes databases that refuse connections",
			query: postgres.QueryWraparound,
			want:  []string{"d.datallowconn", "age(d.datfrozenxid)"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, want := range tc.want {
				if !strings.Contains(tc.query, want) {
					t.Errorf("query lost %q:\n%s", want, tc.query)
				}
			}
		})
	}
}

func TestQueriesAreReadOnly(t *testing.T) {
	t.Parallel()

	banned := []string{"INSERT", "UPDATE", "DELETE", "DROP", "CREATE", "ALTER", "TRUNCATE", "GRANT", "COPY"}
	queries := map[string]string{
		"in_recovery": postgres.QueryInRecovery,
		"standbys":    postgres.QueryStandbys,
		"slots":       postgres.QuerySlots,
		"tables":      postgres.QueryTables,
		"wraparound":  postgres.QueryWraparound,
	}
	for name, q := range queries {
		upper := strings.ToUpper(q)
		for _, word := range banned {
			if strings.Contains(upper, word) {
				t.Errorf("query %s contains %s; the monitoring role is read-only and should stay that way", name, word)
			}
		}
	}
}

func TestOpenRejectsAnEmptyDSN(t *testing.T) {
	t.Parallel()

	// An empty DATA_SOURCE_NAME is the commonest way to deploy this, and
	// database/sql would accept it and fail later at connect time, which
	// reads as a down database rather than a missing setting.
	if _, err := postgres.Open(""); err == nil {
		t.Fatal("Open(\"\") returned no error")
	}
}

func TestFakeReportsItsCalls(t *testing.T) {
	t.Parallel()

	f := &postgres.Fake{}
	ctx := context.Background()
	if err := f.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := f.Replication(ctx); err != nil {
		t.Fatalf("Replication: %v", err)
	}
	got := f.Counts()
	if got.Ping != 1 || got.Replication != 1 || got.Bloat != 0 {
		t.Errorf("Counts() = %+v", got)
	}
}

func TestFakeReturnsTheErrorItWasGiven(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("permission denied for view pg_stat_replication")
	f := &postgres.Fake{ReplicationErr: sentinel}
	if _, err := f.Replication(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Replication error = %v, want %v", err, sentinel)
	}
	// Ping is unaffected: the server is up, one view is not readable.
	if err := f.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestFakeHangsUntilTheContextEnds(t *testing.T) {
	t.Parallel()

	f := &postgres.Fake{Hang: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Bloat(ctx, postgres.DefaultLimits); !errors.Is(err, context.Canceled) {
		t.Fatalf("Bloat error = %v, want context.Canceled", err)
	}
	if n := f.Counts().Cancelled; n != 1 {
		t.Fatalf("Cancelled = %d, want 1", n)
	}
}

func TestDefaultLimits(t *testing.T) {
	t.Parallel()

	// Both numbers are quoted in the README and in the flag help. A change
	// here without a change there makes the documentation wrong, which is
	// the only reason this assertion exists.
	if postgres.DefaultLimits.MinTableBytes != 8<<20 {
		t.Errorf("MinTableBytes = %d, want 8388608", postgres.DefaultLimits.MinTableBytes)
	}
	if postgres.DefaultLimits.MaxTables != 500 {
		t.Errorf("MaxTables = %d, want 500", postgres.DefaultLimits.MaxTables)
	}
}
