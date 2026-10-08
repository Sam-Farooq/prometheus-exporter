package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
)

// Fixture is the on-disk form of one server state.
//
// The files that decode into this are written by hand. None of them was
// captured from a PostgreSQL server, dumped from a client, or recorded from a
// running exporter: the numbers were chosen to put a particular decision under
// test, and Note says which. TestFixturesDeclareTheirOrigin refuses a fixture
// whose Note does not say it was written by hand, so the claim cannot quietly
// stop being true.
type Fixture struct {
	Note        string      `json:"note"`
	Replication Replication `json:"replication"`
	// Tables is the unfiltered set. Fake runs ApplyLimits over it, so a
	// fixture can exercise the row cap by holding more tables than the cap.
	Tables     []Table    `json:"tables"`
	Wraparound Wraparound `json:"wraparound"`
}

// LoadFixture reads one fixture file.
func LoadFixture(path string) (Fixture, error) {
	var f Fixture
	raw, err := os.ReadFile(path) //nolint:gosec // test fixtures, path from the caller
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// An unknown field in a fixture is a typo in a field name, and a typo
	// in a field name is a zero value that silently passes a test.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("fixture %s: %w", path, err)
	}
	return f, nil
}

// Fake is a Source backed by a Fixture. It is the offline half of the test
// suite: no socket, no DSN, no container.
//
// A zero Fake is a server with no standbys, no slots and no tables, which is a
// state worth testing on its own.
type Fake struct {
	Data Fixture

	// Each of these, when set, is returned by the matching method. The four
	// are separate because a server that answers Ping and refuses
	// pg_stat_replication is a real permission failure, and it has to look
	// different from a server that is down.
	PingErr        error
	ReplicationErr error
	BloatErr       error
	WraparoundErr  error

	// Hang makes every method wait for the context instead of answering.
	// This is how the scrape timeout is tested without a slow server and
	// without a sleep.
	Hang bool

	pings      atomic.Int64
	replicates atomic.Int64
	bloats     atomic.Int64
	wraps      atomic.Int64
	cancelled  atomic.Int64
}

// Counts is how many times each method was called, and how many calls ended
// because the context was done.
type Counts struct {
	Ping, Replication, Bloat, Wraparound, Cancelled int64
}

// Counts reports the call counters. It is safe to call while a scrape runs.
func (f *Fake) Counts() Counts {
	return Counts{
		Ping:        f.pings.Load(),
		Replication: f.replicates.Load(),
		Bloat:       f.bloats.Load(),
		Wraparound:  f.wraps.Load(),
		Cancelled:   f.cancelled.Load(),
	}
}

func (f *Fake) wait(ctx context.Context) error {
	if !f.Hang {
		return ctx.Err()
	}
	<-ctx.Done()
	f.cancelled.Add(1)
	return ctx.Err()
}

// Ping implements Source.
func (f *Fake) Ping(ctx context.Context) error {
	f.pings.Add(1)
	if err := f.wait(ctx); err != nil {
		return err
	}
	return f.PingErr
}

// Replication implements Source.
func (f *Fake) Replication(ctx context.Context) (Replication, error) {
	f.replicates.Add(1)
	if err := f.wait(ctx); err != nil {
		return Replication{}, err
	}
	if f.ReplicationErr != nil {
		return Replication{}, f.ReplicationErr
	}
	return f.Data.Replication, nil
}

// Bloat implements Source. It applies the same row-selection rule the real
// query applies, through ApplyLimits.
func (f *Fake) Bloat(ctx context.Context, lim Limits) (Bloat, error) {
	f.bloats.Add(1)
	if err := f.wait(ctx); err != nil {
		return Bloat{}, err
	}
	if f.BloatErr != nil {
		return Bloat{}, f.BloatErr
	}
	kept, matching := ApplyLimits(f.Data.Tables, lim)
	return Bloat{Tables: kept, Matching: matching}, nil
}

// Wraparound implements Source.
func (f *Fake) Wraparound(ctx context.Context) (Wraparound, error) {
	f.wraps.Add(1)
	if err := f.wait(ctx); err != nil {
		return Wraparound{}, err
	}
	if f.WraparoundErr != nil {
		return Wraparound{}, f.WraparoundErr
	}
	return f.Data.Wraparound, nil
}

// Close implements Source.
func (f *Fake) Close() error { return nil }
