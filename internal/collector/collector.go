// Package collector implements prometheus.Collector over a postgres.Source.
//
// The interface is four methods and three of them are easy. What is not easy
// is the set of promises a Prometheus collector makes without saying so:
// Describe and Collect have to agree, Collect must return whether or not the
// database does, nothing may vanish on failure, and two concurrent scrapes of
// the same endpoint must not see each other. Every one of those is a test in
// this package.
package collector

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Sam-Farooq/prometheus-exporter/internal/postgres"
)

// DefaultTimeout bounds one scrape. It is below the Prometheus default
// scrape_timeout of 10s on purpose: if the exporter gives up first it can
// still answer with pg_up 0, and if Prometheus gives up first the whole
// target goes stale and says nothing about why.
const DefaultTimeout = 5 * time.Second

// ReplicationStates is every value the state label can take. The set is closed
// and all of it is emitted on every scrape, including the states with no
// walsender in them.
//
// This is the point of the whole metric: the thing worth alerting on is the
// streaming count reaching zero, and a series that only exists while the count
// is non-zero can never report a zero.
var ReplicationStates = []string{"startup", "catchup", "streaming", "backup", "stopping", "unknown"}

// Config is what New needs.
type Config struct {
	Source  postgres.Source
	Limits  postgres.Limits
	Timeout time.Duration
	Version string
	Logger  *slog.Logger
}

// Exporter is the collector registered at /metrics.
type Exporter struct {
	src     postgres.Source
	limits  postgres.Limits
	timeout time.Duration
	version string
	log     *slog.Logger

	d descs

	// The only state that survives a scrape, and both are counters.
	//
	// Nothing else may live here. A gauge held on the struct keeps the last
	// value of every label set it has ever seen, and a table that was
	// dropped two weeks ago keeps reporting the dead row count it had when
	// it went. Every data metric is built fresh per scrape with
	// MustNewConstMetric, which makes a series that stops being observed
	// stop being reported, by construction rather than by remembering to
	// call Delete.
	//
	// The counters are the inverse: rebuilt per scrape they would reset to
	// zero on every scrape, and rate() over them would be a flat line at
	// zero forever.
	scrapes      prometheus.Counter
	scrapeErrors *prometheus.CounterVec
}

// New builds an exporter. It registers nothing: the caller owns the registry,
// so a test can use a fresh one per case.
func New(cfg Config) *Exporter {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Version == "" {
		cfg.Version = "unknown"
	}

	e := &Exporter{
		src:     cfg.Source,
		limits:  cfg.Limits,
		timeout: cfg.Timeout,
		version: cfg.Version,
		log:     cfg.Logger,
		d:       newDescs(),
		scrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "exporter",
			Name:      "scrapes_total",
			Help:      "Scrapes of this endpoint since the process started. Divide the error counter by this to get a failure rate that does not depend on the scrape interval.",
		}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "exporter",
			Name:      "scrape_errors_total",
			Help:      "Failed scrapes per collector since the process started. Present at zero from the first scrape, so an increase() over a window that starts before the first failure has a baseline to compare against.",
		}, []string{"collector"}),
	}

	// Create every collector's error counter at zero. A counter that only
	// appears on its first increment makes every query over it start from
	// no data, and absent is not zero.
	for _, n := range CollectorNames {
		e.scrapeErrors.WithLabelValues(n)
	}

	return e
}

// Describe implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range e.d.all() {
		ch <- d
	}
	e.scrapes.Describe(ch)
	e.scrapeErrors.Describe(ch)
}

// Collect implements prometheus.Collector.
//
// It returns on every path. A collector that returns nothing because the
// database is slow produces a scrape with no pg_up at all, and a target whose
// series are simply absent looks like a target nobody is scraping.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.scrapes.Inc()

	// Collect takes no context, so the deadline is the collector's own. The
	// alternative is a scrape that blocks until the kernel gives up on the
	// socket, which on a hung server is minutes.
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	ch <- prometheus.MustNewConstMetric(e.d.buildInfo, prometheus.GaugeValue, 1, e.version)

	start := time.Now()
	err := e.src.Ping(ctx)
	e.finish(ch, CollectorConnect, start, err)

	if err != nil {
		ch <- prometheus.MustNewConstMetric(e.d.up, prometheus.GaugeValue, 0)
		// The three data collectors did not run, and say so rather than
		// disappearing. No duration is emitted for them: a zero would be
		// a measurement of something that never happened.
		for _, n := range []string{CollectorReplication, CollectorBloat, CollectorWraparound} {
			ch <- prometheus.MustNewConstMetric(e.d.collectorSuccess, prometheus.GaugeValue, 0, n)
		}
		e.collectSelf(ch)
		return
	}

	ch <- prometheus.MustNewConstMetric(e.d.up, prometheus.GaugeValue, 1)

	e.collectReplication(ctx, ch)
	e.collectBloat(ctx, ch)
	e.collectWraparound(ctx, ch)
	e.collectSelf(ch)
}

// finish records the outcome of one collector.
func (e *Exporter) finish(ch chan<- prometheus.Metric, collector string, start time.Time, err error) {
	ok := 1.0
	if err != nil {
		ok = 0
		e.scrapeErrors.WithLabelValues(collector).Inc()
		e.log.Error("scrape failed", "collector", collector, "error", err.Error())
	}
	ch <- prometheus.MustNewConstMetric(e.d.collectorSuccess, prometheus.GaugeValue, ok, collector)
	ch <- prometheus.MustNewConstMetric(e.d.collectorDuration, prometheus.GaugeValue, time.Since(start).Seconds(), collector)
}

func (e *Exporter) collectSelf(ch chan<- prometheus.Metric) {
	e.scrapes.Collect(ch)
	e.scrapeErrors.Collect(ch)
}

func (e *Exporter) collectReplication(ctx context.Context, ch chan<- prometheus.Metric) {
	start := time.Now()
	rep, err := e.src.Replication(ctx)
	e.finish(ch, CollectorReplication, start, err)
	if err != nil {
		return
	}

	recovery := 0.0
	if rep.InRecovery {
		recovery = 1
	}
	ch <- prometheus.MustNewConstMetric(e.d.inRecovery, prometheus.GaugeValue, recovery)

	// Per-scrape maps, declared here rather than on the Exporter. A map on
	// the struct is shared by two concurrent scrapes of the same endpoint,
	// which Prometheus will happily do.
	byState := make(map[string]float64, len(ReplicationStates))
	known := make(map[string]bool, len(ReplicationStates))
	for _, s := range ReplicationStates {
		known[s] = true
		byState[s] = 0
	}

	type folded struct {
		lagBytes float64
		lagSecs  *float64
	}
	byName := make(map[string]folded, len(rep.Standbys))
	for _, s := range rep.Standbys {
		state := s.State
		if !known[state] {
			state = "unknown"
		}
		byState[state]++

		// Two standbys that never set application_name both arrive as the
		// same value. Emitting both would send two metrics with the same
		// name and the same labels, and the registry rejects the whole
		// scrape for it: /metrics starts returning 500 and the exporter
		// looks down because two replicas share a connection string.
		prev, seen := byName[s.ApplicationName]
		next := folded{lagBytes: s.LagBytes, lagSecs: s.ReplayLagSeconds}
		if seen {
			if prev.lagBytes > next.lagBytes {
				next.lagBytes = prev.lagBytes
			}
			next.lagSecs = maxLag(prev.lagSecs, next.lagSecs)
		}
		byName[s.ApplicationName] = next
	}

	for _, state := range ReplicationStates {
		ch <- prometheus.MustNewConstMetric(e.d.standbys, prometheus.GaugeValue, byState[state], state)
	}
	ch <- prometheus.MustNewConstMetric(e.d.standbysFolded, prometheus.GaugeValue, float64(len(rep.Standbys)-len(byName)))

	for _, app := range sortedKeys(byName) {
		s := byName[app]
		ch <- prometheus.MustNewConstMetric(e.d.standbyLagBytes, prometheus.GaugeValue, s.lagBytes, app)
		if s.lagSecs != nil {
			ch <- prometheus.MustNewConstMetric(e.d.standbyLagSecs, prometheus.GaugeValue, *s.lagSecs, app)
		}
	}

	for _, slot := range rep.Slots {
		active := 0.0
		if slot.Active {
			active = 1
		}
		ch <- prometheus.MustNewConstMetric(e.d.slotActive, prometheus.GaugeValue, active, slot.Name, slot.Type)
		ch <- prometheus.MustNewConstMetric(e.d.slotRetained, prometheus.GaugeValue, slot.RetainedWALBytes, slot.Name, slot.Type)
	}
}

func (e *Exporter) collectBloat(ctx context.Context, ch chan<- prometheus.Metric) {
	start := time.Now()
	bloat, err := e.src.Bloat(ctx, e.limits)
	e.finish(ch, CollectorBloat, start, err)
	if err != nil {
		return
	}

	for _, t := range bloat.Tables {
		ch <- prometheus.MustNewConstMetric(e.d.tableDeadRows, prometheus.GaugeValue, t.DeadRows, t.Schema, t.Name)
		ch <- prometheus.MustNewConstMetric(e.d.tableLiveRows, prometheus.GaugeValue, t.LiveRows, t.Schema, t.Name)
		ch <- prometheus.MustNewConstMetric(e.d.tableSizeBytes, prometheus.GaugeValue, t.SizeBytes, t.Schema, t.Name)
		ch <- prometheus.MustNewConstMetric(e.d.tableVacThreshold, prometheus.GaugeValue, t.AutovacuumThresholdRows, t.Schema, t.Name)

		// autovacuum_count only goes up, apart from a statistics reset,
		// which is what a counter means. Typed as a gauge the exposition
		// says gauge, and rate() over a gauge is not rejected, it is
		// quietly wrong across a reset.
		ch <- prometheus.MustNewConstMetric(e.d.tableAutovacsTotal, prometheus.CounterValue, t.Autovacuums, t.Schema, t.Name)

		if t.LastAutovacuum != nil {
			ch <- prometheus.MustNewConstMetric(e.d.tableLastAutovac, prometheus.GaugeValue, float64(t.LastAutovacuum.Unix()), t.Schema, t.Name)
		}
	}

	ch <- prometheus.MustNewConstMetric(e.d.tableOmitted, prometheus.GaugeValue, bloat.Omitted())
}

func (e *Exporter) collectWraparound(ctx context.Context, ch chan<- prometheus.Metric) {
	start := time.Now()
	w, err := e.src.Wraparound(ctx)
	e.finish(ch, CollectorWraparound, start, err)
	if err != nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(e.d.freezeMaxAge, prometheus.GaugeValue, w.FreezeMaxAge)
	for _, db := range w.Databases {
		ch <- prometheus.MustNewConstMetric(e.d.databaseXIDAge, prometheus.GaugeValue, db.XIDAge, db.Name)
	}
}

func maxLag(a, b *float64) *float64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a > *b:
		return a
	default:
		return b
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
