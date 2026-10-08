# testdata

Two kinds of file, and both are written by hand.

## The `.json` fixtures

Five server states, decoded into `postgres.Fixture` and served by
`postgres.Fake`. **None of them was captured from a PostgreSQL server,
recorded from a client, or dumped by a running exporter.** Every number was
chosen to put one decision under test, and each file says so in its `note`
field. `TestFixturesDeclareTheirOrigin` fails a fixture whose note stops
saying it, so the claim cannot quietly stop being true.

| File | What it is for |
|---|---|
| `primary-healthy.json` | A primary with two named standbys, two active slots, three tables above the 8 MiB filter and one below it. The small table is there to put the size filter in a golden file. |
| `primary-degraded.json` | Four faults at once: two standbys sharing the default `application_name`, a standby that has never reported a replay time, two inactive slots holding 48 GiB of WAL between them, and the largest table never autovacuumed. |
| `standby.json` | `pg_is_in_recovery()` true, nothing replicating from it. The case where every replication metric is legitimately empty and the state counts still have to be present at zero. |
| `empty.json` | Every query returns no rows. The golden file for it is the floor of what the exporter always emits. |
| `table-cap.json` | Six tables, used with `MaxTables` set to 2, so `pg_table_series_omitted` has something to report. |

The autovacuum thresholds in every fixture follow
`autovacuum_vacuum_threshold + autovacuum_vacuum_scale_factor * live_rows` at
the PostgreSQL defaults of 50 and 0.2.
`TestFixtureThresholdsMatchTheDocumentedFormula` checks the arithmetic, because
a fixture whose threshold does not follow from its own row count describes a
server state that cannot exist.

`LoadFixture` sets `DisallowUnknownFields`. A misspelled field name would
otherwise decode to a zero value and the test that depended on it would pass
against nothing.

## The `.prom` golden files

Rendered exposition text, one file per case in `TestExposition`. Regenerate
with:

```bash
go test ./internal/collector/ -update -count=1
```

Read the diff before committing it. This text is the contract Prometheus
parses, so a change here is a change to what every dashboard and alert rule
would see. The `# TYPE` lines are the part worth checking: a counter typed as a
gauge is invisible in Go and plain in the diff, which is how
`pg_table_autovacuums_total` got fixed.

`pg_exporter_scrape_duration_seconds` is left out of the comparison. It is wall
time, and a golden file holding it fails on a slow runner for no reason;
`TestDurationIsRecordedForCollectorsThatRan` covers it instead.
