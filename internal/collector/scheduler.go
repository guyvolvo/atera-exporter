// Package collector polls the Atera API on a schedule and exposes the results
// as Prometheus metrics.
//
// The central design decision: polling is fully decoupled from scraping. Each
// Domain refreshes on its own ticker into an immutable snapshot, and a scrape
// only reads the newest snapshot. Fetching during a scrape would mean N
// paginated API calls inside Prometheus' scrape timeout, and would multiply
// Atera API load by the number of scrapers.
package collector

import (
	"context"
	"log/slog"
	"math/rand"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Domain is one Atera resource (agents, alerts, tickets) that knows how to
// refresh itself and emit its own metrics. Refresh and Collect are called from
// different goroutines, so implementations must swap snapshots atomically.
type Domain interface {
	prometheus.Collector

	Name() string
	Interval() time.Duration
	Refresh(ctx context.Context) error
}

// Scheduler runs each Domain on its own ticker and owns the meta-metrics that
// describe the health of polling itself.
type Scheduler struct {
	domains []Domain
	timeout time.Duration
	log     *slog.Logger

	up          *prometheus.GaugeVec
	duration    *prometheus.GaugeVec
	lastSuccess *prometheus.GaugeVec
	errors      *prometheus.CounterVec
}

func NewScheduler(log *slog.Logger, timeout time.Duration, domains ...Domain) *Scheduler {
	return &Scheduler{
		domains: domains,
		timeout: timeout,
		log:     log,

		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "atera_up",
			Help: "1 if the last poll of this domain succeeded, 0 if it failed.",
		}, []string{"domain"}),

		duration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "atera_poll_duration_seconds",
			Help: "Wall time of the last poll cycle, including all pages and retries.",
		}, []string{"domain"}),

		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "atera_last_success_timestamp_seconds",
			Help: "Unix time of the last successful poll. Alert on this going stale.",
		}, []string{"domain"}),

		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "atera_poll_errors_total",
			Help: "Failed poll cycles.",
		}, []string{"domain"}),
	}
}

// Register wires the scheduler's meta-metrics and every domain into reg. We use
// an explicit registry rather than the default one so the collectors are
// constructible in tests without duplicate-registration panics.
func (s *Scheduler) Register(reg prometheus.Registerer) error {
	cs := []prometheus.Collector{s.up, s.duration, s.lastSuccess, s.errors}
	for _, d := range s.domains {
		cs = append(cs, d)
	}
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// Run polls every domain until ctx is cancelled. It blocks.
func (s *Scheduler) Run(ctx context.Context) {
	done := make(chan struct{}, len(s.domains))

	for _, d := range s.domains {
		go func(d Domain) {
			defer func() { done <- struct{}{} }()
			s.loop(ctx, d)
		}(d)
	}

	for range s.domains {
		<-done
	}
}

func (s *Scheduler) loop(ctx context.Context, d Domain) {
	// Initialise up to 0 so a domain that has never polled is distinguishable
	// from one that is healthy. Without this the series is simply absent and
	// alerting expressions silently match nothing.
	s.up.WithLabelValues(d.Name()).Set(0)

	s.pollOnce(ctx, d)

	// Stagger the tickers. Every domain starting at t=0 would burst the whole
	// per-second API budget at once and trip Atera's rate limiter.
	jitter := time.Duration(rand.Int63n(int64(5 * time.Second)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	ticker := time.NewTicker(d.Interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollOnce(ctx, d)
		}
	}
}

// pollOnce refreshes a domain and records the outcome. A failure leaves the
// previous snapshot intact: serving slightly stale data beats zeroing every
// gauge and firing false alerts every time Atera has a bad minute.
func (s *Scheduler) pollOnce(ctx context.Context, d Domain) {
	name := d.Name()

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	start := time.Now()
	err := d.Refresh(ctx)
	elapsed := time.Since(start)

	s.duration.WithLabelValues(name).Set(elapsed.Seconds())

	if err != nil {
		s.up.WithLabelValues(name).Set(0)
		s.errors.WithLabelValues(name).Inc()
		s.log.Error("poll failed", "domain", name, "duration", elapsed, "err", err)
		return
	}

	s.up.WithLabelValues(name).Set(1)
	s.lastSuccess.WithLabelValues(name).SetToCurrentTime()
	s.log.Info("poll ok", "domain", name, "duration", elapsed)
}
