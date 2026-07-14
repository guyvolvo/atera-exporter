package collector

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/guyvolvo/atera-exporter/internal/atera"
)

// Alerts exposes open alert counts, aggregated rather than one series per alert.
// Per-alert series would churn constantly as alerts open and close, and each
// dead alert would leave a stale series behind for the retention period.
type Alerts struct {
	client   *atera.Client
	interval time.Duration
	snap     atomic.Pointer[alertsSnapshot]

	count  *prometheus.Desc
	oldest *prometheus.Desc
}

type alertsSnapshot struct {
	counts map[alertKey]int
	oldest map[string]time.Time // severity -> creation time of the oldest open alert
}

type alertKey struct {
	severity string
	category string
	source   string
}

func NewAlerts(client *atera.Client, interval time.Duration) *Alerts {
	return &Alerts{
		client:   client,
		interval: interval,

		count: prometheus.NewDesc(
			"atera_alerts",
			"Open (non-archived) alerts by severity, category and source.",
			[]string{"severity", "category", "source"}, nil),

		oldest: prometheus.NewDesc(
			"atera_oldest_alert_timestamp_seconds",
			"Creation time of the oldest open alert per severity. Detects alerts nobody is actioning.",
			[]string{"severity"}, nil),
	}
}

func (a *Alerts) Name() string            { return "alerts" }
func (a *Alerts) Interval() time.Duration { return a.interval }

func (a *Alerts) Refresh(ctx context.Context) error {
	alerts, err := a.client.Alerts(ctx)
	if err != nil {
		return err
	}

	snap := &alertsSnapshot{
		counts: make(map[alertKey]int),
		oldest: make(map[string]time.Time),
	}

	for _, al := range alerts {
		if al.Archived {
			continue
		}
		k := alertKey{severity: al.Severity, category: al.Category, source: al.Source}
		snap.counts[k]++

		if al.Created.IsZero() {
			continue
		}
		if cur, ok := snap.oldest[al.Severity]; !ok || al.Created.Before(cur) {
			snap.oldest[al.Severity] = al.Created.Time
		}
	}

	a.snap.Store(snap)
	return nil
}

func (a *Alerts) Describe(ch chan<- *prometheus.Desc) {
	ch <- a.count
	ch <- a.oldest
}

func (a *Alerts) Collect(ch chan<- prometheus.Metric) {
	snap := a.snap.Load()
	if snap == nil {
		return
	}

	for k, n := range snap.counts {
		ch <- prometheus.MustNewConstMetric(a.count, prometheus.GaugeValue, float64(n),
			k.severity, k.category, k.source)
	}

	for severity, t := range snap.oldest {
		ch <- prometheus.MustNewConstMetric(a.oldest, prometheus.GaugeValue,
			float64(t.Unix()), severity)
	}
}
