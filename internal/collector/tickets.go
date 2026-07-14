package collector

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/guyvolvo/atera-exporter/internal/atera"
)

// Tickets exposes queue depth and age.
//
// It never walks the ticket corpus. A mature tenant holds tens of thousands of
// tickets and the API caps itemsInPage at 50, so a full walk runs to over a
// thousand requests and takes minutes — it would time out on every cycle. Instead:
//
//   - Counts come from totalItemCount on a filtered, itemsInPage=1 request. One
//     request per status, zero tickets transferred.
//   - Only the live statuses are fetched in full, to compute queue age and the
//     priority and technician breakdown. Resolved and Closed tickets are history:
//     they are counted, never downloaded.
//
// Some tickets match no ticketStatus value the filter accepts, so the per-status
// counts can sum slightly below the unfiltered total. atera_tickets_total is
// exported precisely so that gap is visible rather than silently absorbed.
type Tickets struct {
	client   *atera.Client
	interval time.Duration

	// statuses counted cheaply; liveStatuses is the subset fetched in full.
	statuses     []string
	liveStatuses []string

	snap atomic.Pointer[ticketsSnapshot]

	byStatus     *prometheus.Desc
	total        *prometheus.Desc
	openDetail   *prometheus.Desc
	oldestOpen   *prometheus.Desc
	byTechnician *prometheus.Desc
}

type ticketsSnapshot struct {
	counts map[string]int // status -> count, from totalItemCount
	total  int            // unfiltered total

	openByPriority   map[string]int
	openByTechnician map[string]int
	oldestOpen       time.Time
}

func NewTickets(client *atera.Client, interval time.Duration, statuses, liveStatuses []string) *Tickets {
	return &Tickets{
		client:       client,
		interval:     interval,
		statuses:     statuses,
		liveStatuses: liveStatuses,

		byStatus: prometheus.NewDesc(
			"atera_tickets",
			"Ticket count per status, from the API's own totalItemCount.",
			[]string{"status"}, nil),

		total: prometheus.NewDesc(
			"atera_tickets_total",
			"Unfiltered ticket count. Compare against sum(atera_tickets) to spot statuses missing from the configured list.",
			nil, nil),

		openDetail: prometheus.NewDesc(
			"atera_open_tickets",
			"Open ticket count by priority.",
			[]string{"priority"}, nil),

		byTechnician: prometheus.NewDesc(
			"atera_open_tickets_by_technician",
			"Open ticket count per assigned technician. Answers who is sitting on work.",
			[]string{"technician"}, nil),

		oldestOpen: prometheus.NewDesc(
			"atera_oldest_open_ticket_timestamp_seconds",
			"Creation time of the oldest open ticket. Subtract from time() for queue age.",
			nil, nil),
	}
}

func (t *Tickets) Name() string            { return "tickets" }
func (t *Tickets) Interval() time.Duration { return t.interval }

func (t *Tickets) Refresh(ctx context.Context) error {
	snap := &ticketsSnapshot{
		counts:           make(map[string]int, len(t.statuses)),
		openByPriority:   make(map[string]int),
		openByTechnician: make(map[string]int),
	}

	total, err := t.client.TicketCountAll(ctx)
	if err != nil {
		return err
	}
	snap.total = total

	for _, status := range t.statuses {
		n, err := t.client.TicketCount(ctx, status)
		if err != nil {
			return err
		}
		snap.counts[status] = n
	}

	for _, status := range t.liveStatuses {
		tickets, err := t.client.TicketsByStatus(ctx, status)
		if err != nil {
			return err
		}
		for _, tk := range tickets {
			snap.openByPriority[tk.TicketPriority]++
			snap.openByTechnician[tk.Technician()]++

			if tk.Created.IsZero() {
				continue
			}
			if snap.oldestOpen.IsZero() || tk.Created.Before(snap.oldestOpen) {
				snap.oldestOpen = tk.Created.Time
			}
		}
	}

	t.snap.Store(snap)
	return nil
}

func (t *Tickets) Describe(ch chan<- *prometheus.Desc) {
	ch <- t.byStatus
	ch <- t.total
	ch <- t.openDetail
	ch <- t.byTechnician
	ch <- t.oldestOpen
}

func (t *Tickets) Collect(ch chan<- prometheus.Metric) {
	snap := t.snap.Load()
	if snap == nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(t.total, prometheus.GaugeValue, float64(snap.total))

	for status, n := range snap.counts {
		ch <- prometheus.MustNewConstMetric(t.byStatus, prometheus.GaugeValue, float64(n), status)
	}

	for priority, n := range snap.openByPriority {
		ch <- prometheus.MustNewConstMetric(t.openDetail, prometheus.GaugeValue, float64(n), priority)
	}

	for tech, n := range snap.openByTechnician {
		ch <- prometheus.MustNewConstMetric(t.byTechnician, prometheus.GaugeValue, float64(n), tech)
	}

	if !snap.oldestOpen.IsZero() {
		ch <- prometheus.MustNewConstMetric(t.oldestOpen, prometheus.GaugeValue,
			float64(snap.oldestOpen.Unix()))
	}
}
