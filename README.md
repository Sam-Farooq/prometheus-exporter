# prometheus-exporter

A Prometheus exporter for PostgreSQL replication, table bloat and transaction
id age. One Go binary, two direct dependencies, and no database needed to run
its tests.

```
cmd/pgexporter/                flags, three endpoints, graceful stop
internal/collector/            prometheus.Collector, 20 descriptors, the series budget
internal/collector/testdata/   six hand-written fixtures, eight golden exposition files
internal/postgres/             row shapes, four SQL statements, one database/sql source, the fake
```

The exporter interface is a Go interface, which is the reason this is Go and
not Python. `prometheus.Collector` is two methods, and the hard part is not
implementing them: it is the set of promises they make that nothing enforces.
Describe and Collect have to agree. Collect has to return whether or not the
database does. Nothing may vanish when a scrape fails. Two concurrent scrapes
of one endpoint must not see each other. Each of those is a test here, and two
of them were wrong first.

## The four promises, and what they cost

### A failed scrape sets `pg_up` to 0 rather than going quiet

A series that disappears reads as a healthy quiet period. An alert written as
`pg_replication_slot_retained_wal_bytes > 40e9` is not true of a target that
has stopped reporting the metric, so a database that fell over silences the
alert that was watching it.

So the exporter always answers. A scrape of a server it cannot reach is twelve
series and every one of them says so:

```
$ DATA_SOURCE_NAME='postgres://nobody@127.0.0.1:1/none?sslmode=disable' \
    ./bin/pgexporter -web.runtime-metrics=false &
$ curl -s localhost:9187/metrics | grep -v '^#' | sort
pg_exporter_build_info{version="0.16.4"} 1
pg_exporter_collector_success{collector="bloat"} 0
pg_exporter_collector_success{collector="connect"} 0
pg_exporter_collector_success{collector="replication"} 0
pg_exporter_collector_success{collector="wraparound"} 0
pg_exporter_scrape_duration_seconds{collector="connect"} 0.002249667
pg_exporter_scrape_errors_total{collector="bloat"} 0
pg_exporter_scrape_errors_total{collector="connect"} 1
pg_exporter_scrape_errors_total{collector="replication"} 0
pg_exporter_scrape_errors_total{collector="wraparound"} 0
pg_exporter_scrapes_total 1
pg_up 0
```

Three things in that list are deliberate.

The error counters are present at zero before anything has failed. A counter
that appears with its first increment gives `increase()` no baseline over any
window that starts earlier, so the first failure of a collector is invisible
for one window.

`pg_up` is 0 only when the ping failed. A role created without `pg_monitor`
cannot read `pg_stat_replication`, and reporting that as `pg_up 0` sends
somebody to look at a database that is fine. That case is
`pg_exporter_collector_success{collector="replication"} 0` with `pg_up` still
at 1, and the other two collectors still carrying their values.

There is no duration for a collector that did not run. A zero there would be a
measurement of something that never happened, so three of the four duration
series are absent in the block above. That is the one place where a missing
series is the honest answer, and it is a metric about the exporter rather than
about the database.

### Per-scrape state must not leak between scrapes

The six table metrics were `GaugeVec`s held on the collector. That works, right
up to the second scrape. A `GaugeVec` keeps the last value of every label set
it has ever been given, so a table dropped from the database, or pushed out of
the top-N by a table that grew, kept being exported with the numbers it had
when it went, for as long as the process ran.

One scrape of a stable table set renders identically either way, which is why
the first version looked finished.
`TestStaleSeriesDoNotSurviveAScrape` is a second scrape with one table removed.

Every data metric is now built per scrape with `prometheus.MustNewConstMetric`,
so a series that stops being observed stops being reported by construction
rather than by somebody remembering to call `Delete`.

**What that cost:** a held gauge keeps exporting its last value when a scrape
fails, and const metrics do not. A failed bloat scrape now leaves a gap in the
table graphs instead of a flat line. The gap is correct and it is less
comfortable to look at, and the trade is worth making because the flat line is
indistinguishable from a table that stopped changing.

The two counters stay on the struct, because they have the opposite failure:
rebuilt per scrape they reset to zero every scrape and `rate()` over them is a
flat line at zero forever. `TestCountersSurviveScrapesAndGaugesDoNot` is both
halves.

### A scrape must not block forever

`Collect` takes no context, so the deadline has to be the collector's own.
`-scrape-timeout` defaults to 5s, below the Prometheus default
`scrape_timeout` of 10s on purpose: if the exporter gives up first it can still
answer `pg_up 0`, and if Prometheus gives up first the whole target goes stale
and says nothing about why.

The test for it uses a fake that waits on the context rather than a sleep, so
it is deterministic and the whole case runs in 20 milliseconds.

### Describe and Collect have to agree

`Describe` walks one list and `Collect` reads from the same one, so the two
cannot hold different opinions about which metrics exist. The check is a
`prometheus.NewPedanticRegistry`, which rejects a collected metric whose
descriptor was never described, run over all six fixtures.

## The duplicate that returned a 500

Two standbys that never set `application_name` both arrive from
`pg_stat_replication` as `walreceiver`. Emitting both sends the registry two
metrics with the same name and the same labels, `Gather` returns an error, and
`/metrics` answers 500. The exporter looks down because two replicas share a
connection string.

So duplicates are folded, keeping the larger lag, and
`pg_replication_standbys_folded` reports how many rows were merged.

**What that cost:** the two standbys are no longer distinguishable. Keeping
them apart would mean a label that is unique per connection, and the available
one is `client_addr`, which is a pod IP that changes on every restart and
orphans a series each time. A gauge saying "two of your standbys are reporting
as one, and here is the worse of them" is more useful than either a 500 or a
series per IP address.

## The metrics

22 of them. Base units in the name, `_total` only on counters, and no
`_count`, `_sum` or `_bucket` suffix anywhere, because those three are what a
histogram generates. A test reads the type off the rendered exposition rather
than off the Go call site and holds every name to that, which is what caught
`pg_table_autovacuums_total` being a gauge.

**Replication.** `pg_replication_standby_lag_bytes{application_name}` comes
from LSN arithmetic and is always available.
`pg_replication_standby_replay_lag_seconds{application_name}` is what the
standby last reported, and is absent until it reports, so the bytes metric is
the one to alert on. `pg_replication_slot_retained_wal_bytes{slot_name,
slot_type}` is the number that fills a disk, next to
`pg_replication_slot_active`, because an inactive logical slot retains WAL
until the data directory is full.

`state` is a label on the count of standbys, not on the per-standby lag. A
standby moves from `catchup` to `streaming`, and a label whose value changes
over the life of the thing it describes starts a new series on every change and
leaves the old one behind. All six values of `pg_replication_standbys{state}`
are emitted on every scrape including the empty ones, because the thing worth
alerting on is the streaming count reaching zero and a series that only exists
while the count is non-zero can never report a zero.

**Bloat.** `pg_table_dead_rows`, `pg_table_live_rows`, `pg_table_size_bytes`,
`pg_table_autovacuum_threshold_rows`,
`pg_table_last_autovacuum_timestamp_seconds` and `pg_table_autovacuums_total`,
all labelled `{schema, table}`. The threshold is
`autovacuum_vacuum_threshold + autovacuum_vacuum_scale_factor * live_rows`
read from `pg_settings`, so dead rows can be read against the number that
decides whether anything will happen about them. A table over its threshold
with no recent autovacuum is the actionable case; dead rows on their own are
not.

The timestamp is absent for a table autovacuum has never processed, which on a
large table is itself the finding. Zero there is 1970, and `time() - 0 > 86400`
is a true statement about a table that has no last vacuum rather than an old
one.

**Wraparound.** `pg_database_xid_age_transactions{datname}` and
`pg_autovacuum_freeze_max_age_transactions`, which is the server's own setting
rather than an assumed 200 million. Databases with `datallowconn` false are
excluded: `template0` is frozen and its age is not something anybody can act
on.

### What is deliberately not here

Cardinality is what kills a Prometheus, and it is a product rather than a sum:
one label carrying a value per customer multiplies every other label by the
number of customers. These were all considered and left out.

| Not exported | Why |
|---|---|
| Anything from `pg_stat_statements` | `queryid` is unbounded by construction, so no ceiling can be stated for it at all |
| Per-index metrics | An index count exceeds a table count, and the labels would be `{schema, table, index}` |
| `client_addr` as a standby label | A pod IP changes on every restart and orphans a series each time |
| Per-backend metrics | A pid is recycled, so the label value means nothing across a restart |
| `n_tup_ins`, `n_tup_upd`, `n_tup_del` | Six more series per table, 3,000 more at the cap, to answer a question the write path already answers |
| A bloat ratio | The well-known estimate query is wrong often enough to page for nothing, and `pgstattuple` reads every page of the table it measures, which is a scan competing with the workload every fifteen seconds |
| `xid_age` as a percentage of the limit | Dividing two gauges in PromQL is free, and a precomputed ratio hides which of the two moved |
| `relid` or `datid` labels | An OID changes on a table rewrite, so a join on it breaks exactly when the table was rebuilt |

### The ceiling

`-series-budget` computes the worst case from the configured limits, so the
number below is a command's output rather than a claim about one. CI greps this
file for it.

```
$ ./bin/pgexporter -series-budget
metric                                      series  from
pg_table_autovacuum_threshold_rows          500     500 tables
pg_table_autovacuums_total                  500     500 tables
pg_table_dead_rows                          500     500 tables
pg_table_last_autovacuum_timestamp_seconds  500     500 tables
pg_table_live_rows                          500     500 tables
pg_table_size_bytes                         500     500 tables
pg_replication_slot_active                  32      32 slots
pg_replication_slot_retained_wal_bytes      32      32 slots
pg_database_xid_age_transactions            16      16 databases
pg_replication_standby_lag_bytes            16      16 standbys
pg_replication_standby_replay_lag_seconds   16      16 standbys
pg_replication_standbys                     6       every state, always
pg_exporter_collector_success               4       one per collector
pg_exporter_scrape_duration_seconds         4       one per collector
pg_exporter_scrape_errors_total             4       one per collector
pg_autovacuum_freeze_max_age_transactions   1       no labels
pg_exporter_build_info                      1       one build
pg_exporter_scrapes_total                   1       no labels
pg_in_recovery                              1       no labels
pg_replication_standbys_folded              1       no labels
pg_table_series_omitted                     1       no labels
pg_up                                       1       no labels

worst case     3137  at 500 tables, 16 standbys, 32 slots, 16 databases
table metrics  3000  96% of the total
```

3,000 of 3,137 is the table metrics, so the table cap is effectively the whole
number, and it is the only one of the four assumptions the code enforces. It is
enforced in the query, not after it, so a server with 40,000 tables does not
send 40,000 rows over the wire to have most of them discarded:
`-collector.bloat.min-table-bytes` defaults to 8 MiB and
`-collector.bloat.max-tables` to 500, ordered by total relation size
descending.

**What that cost:** the table metrics are a top-N, and a top-N looks exactly
like a complete list. `pg_table_series_omitted` is how that is visible, and
membership churns when a table crosses the boundary, which adds and removes a
series. Table sizes move slowly, so in practice it churns slowly.

`TestBudgetCoversEveryMetric` refuses a descriptor that has no line in the
budget, so adding a metric without thinking about its cardinality fails the
suite rather than the Prometheus.

The Go runtime and process collectors are registered as well and are not in
that budget, because client_golang owns them and the process collector differs
by platform. On this machine `grep -c '^go_\|^process_'` over the endpoint
counts 35 of them. `-web.runtime-metrics=false` removes them.

## Running it

```bash
make build
DATA_SOURCE_NAME='postgres://exporter@db.internal:5432/app?sslmode=require' ./bin/pgexporter
```

The DSN comes from the environment and is deliberately not a flag. A command
line is readable by every user on the host through `ps`, and a PostgreSQL DSN
carries a password. The error for a missing one says that, because the first
reaction to an absent `-dsn` is that somebody forgot it.

It starts without the database, which is the point of having a `pg_up`:

```bash
DATA_SOURCE_NAME='postgres://nobody@127.0.0.1:1/none?sslmode=disable' ./bin/pgexporter &
curl -s localhost:9187/metrics | grep '^pg_up'
curl -s localhost:9187/healthz
```

`/healthz` answers 200 while the database is unreachable. A health endpoint
that fails during a database outage gets the exporter restarted or dropped from
service discovery, and that removes the `pg_up 0` which was reporting the
outage.

The role needs `pg_monitor` and nothing more. Every query is a `SELECT`, and a
test fails the build if any of the five gains an `INSERT`, `UPDATE`, `DELETE`,
`CREATE`, `ALTER`, `DROP`, `TRUNCATE`, `GRANT` or `COPY`:

```sql
CREATE ROLE exporter LOGIN PASSWORD '...';
GRANT pg_monitor TO exporter;
```

Scrape it at an interval that is not shorter than the scrape timeout:

```yaml
scrape_configs:
  - job_name: postgres
    scrape_interval: 15s
    scrape_timeout: 10s
    static_configs:
      - targets: ['db.internal:9187']
```

There are no alert rules and no dashboards in this repo. The HELP string on
every metric says what a reader should conclude from it, and that is the whole
of what is shipped on that side.

## Tests

```bash
go test ./... -race -count=1
gofmt -l .
go vet ./...
make lint
```

`go test ./... -v` reports 104 passing cases, 54 top-level tests and 50
subtests, across 1,655 lines of test against 1,624 lines of everything else.
Not one of them opens a socket, reads a credential or starts a container. The
database is behind `postgres.Source`, and `postgres.Fake` serves one of six
JSON fixtures.

The fixtures are written by hand. **None of them was captured from a PostgreSQL
server, recorded from a client, or dumped by a running exporter.** Every number
was chosen to put one decision under test, and each file says so in a `note`
field that `TestFixturesDeclareTheirOrigin` checks.
`internal/collector/testdata/README.md` goes through them file by file, and
`TestFixtureThresholdsMatchTheDocumentedFormula` checks every fixture's
autovacuum threshold follows from its own live row count at the PostgreSQL
defaults of 50 and 0.2, because a fixture that fails that describes a server
state which cannot exist.

Eight golden files hold rendered exposition text, because that text is the
contract. A test asserting on a Go struct passes while the output carries the
wrong metric name or a counter typed as a gauge, and the output is the only
thing Prometheus parses. `go test ./internal/collector/ -update` rewrites them
and CI fails on a diff.

`testutil.CollectAndLint` runs the same rules `promtool check metrics` applies.
That is what found the gauge with a `_total` suffix, five `# TYPE` lines at a
time.

One fixture is there for the escaping alone. A table name may hold a double
quote, a backslash and a newline, all three legal inside a PostgreSQL quoted
identifier, and all three have to be escaped in the exposition format.
`TestOddIdentifiersSurviveTheExpositionFormat` parses the rendering back with
`expfmt.TextParser` and compares the label values with what went in, because
anything that formatted a line by hand would emit output Prometheus cannot
parse and the target would go down over a table somebody named badly.

`make lint` runs golangci-lint 1.61.0, pinned by tag in both the Makefile and
CI, with 15 linters on top of the defaults. The ones that earn their place here
are `sqlclosecheck`, `nilerr` and `noctx`: rows left unclosed hold a pool
connection, returning nil after checking an error is the failure this whole
repo is about, and a query with no context cannot be timed out.

## Dependencies

Two direct, and both are load-bearing.

`github.com/prometheus/client_golang` is the registry, the exposition encoder,
`promhttp` and `testutil`. `github.com/jackc/pgx/v5` appears once, as a blank
import in `internal/postgres/sqlsource.go`, to register a `database/sql`
driver. Every query goes through `database/sql`, so replacing the driver is
that one line: `lib/pq` is in maintenance mode and pgx is the one being worked
on.

No testify. The assertions here are string containment over exposition text and
exact comparison against golden files, and a table of cases with a `want` field
reads the same with or without a matcher library. What was given up is the
failure message: a plain `t.Errorf` has to say which line was missing itself,
which is why the helpers in `collector_test.go` print the line they looked for.

## What this does not do

- **It has never been pointed at a PostgreSQL server.** `SQLSource` is four
  `Query` calls and a `Scan` per column, and there is no test in this repo that
  exercises it, because a test that exercises it needs a server and this suite
  needs none. What is tested is everything above that line. The surface has
  been kept as small as it can be made rather than described as covered.
- **The limit rule exists twice.** `ApplyLimits` states it in Go for the fake
  and `QueryTables` states it in SQL for the server. Two statements of one rule
  can drift, so `TestQueriesStillFilter` asserts the SQL has not quietly lost
  its half and left the exporter pulling every table in the database across the
  wire on every scrape. That is a guard, not a proof.
- **The autovacuum threshold is wrong for a table with reloptions.** It is
  computed from the server-wide `autovacuum_vacuum_threshold` and
  `autovacuum_vacuum_scale_factor`. A table carrying its own
  `autovacuum_vacuum_scale_factor` in `reloptions` has a different real
  threshold, and this reports the server's.
- **`pg_stat_user_tables` is an estimate, not a count.** Dead and live rows
  come from the statistics collector, and both read zero immediately after a
  statistics reset on a table that is full of them.
- **No alert rules, no dashboards, no Helm chart, no Docker image.** The HELP
  strings are the handover.
- **No replication slot consumer lag in time.** A logical slot's lag is
  available in bytes and turning that into seconds needs a confirmed flush
  position the subscriber may not send.
- **Every number in this file is computed from the repository.** The series
  figures are arithmetic over the configured limits, printed by
  `./bin/pgexporter -series-budget`. The twelve-series block is `curl` against a
  binary built from this source with the DSN pointed at a closed port. The test
  and line counts are `go test -v` and `wc -l`. There is no staging
  environment, no production instance and no benchmark behind any of it.
