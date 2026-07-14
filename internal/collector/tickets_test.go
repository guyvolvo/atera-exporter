package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/guyvolvo/atera-exporter/internal/atera"
)

// fakeTickets models the real estate: tens of thousands of Resolved and Deleted
// tickets that must never be downloaded, and a small live queue that must be.
// It records every request so a test can assert on how the data was obtained,
// not merely on the numbers that came out.
type fakeTickets struct {
	mu sync.Mutex

	// counts by status, returned via totalItemCount
	counts map[string]int
	// full ticket bodies, only for statuses the collector is allowed to walk
	live map[string][]map[string]any

	requests []string
}

func (f *fakeTickets) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.String())
		f.mu.Unlock()

		status := r.URL.Query().Get("ticketStatus")
		itemsInPage, _ := strconv.Atoi(r.URL.Query().Get("itemsInPage"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}

		total := 0
		if status == "" {
			for _, n := range f.counts {
				total += n
			}
		} else {
			total = f.counts[status]
		}

		// A count-only request (itemsInPage=1) gets the total and no bodies.
		var items []map[string]any
		if itemsInPage > 1 {
			all := f.live[status]
			start := (page - 1) * itemsInPage
			if start < len(all) {
				end := min(start+itemsInPage, len(all))
				items = all[start:end]
			}
		}

		totalPages := 0
		if itemsInPage > 0 {
			totalPages = (total + itemsInPage - 1) / itemsInPage
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":          items,
			"page":           page,
			"itemsInPage":    len(items),
			"totalItemCount": total,
			"totalPages":     totalPages,
		})
	})
}

func (f *fakeTickets) walked(status string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.requests {
		// A walk is any request for this status asking for more than one item.
		if strings.Contains(u, "ticketStatus="+status) && !strings.Contains(u, "itemsInPage=1&") {
			return true
		}
	}
	return false
}

func ticket(id int, priority, tech string, created time.Time) map[string]any {
	return map[string]any{
		"TicketID":           id,
		"TicketStatus":       "Open",
		"TicketPriority":     priority,
		"TechnicianFullName": tech,
		"TicketCreatedDate":  created.UTC().Format(time.RFC3339),
	}
}

func newTestTickets(t *testing.T, srvURL string, statuses, live []string) *Tickets {
	t.Helper()
	client := atera.NewClient(atera.Options{
		BaseURL:           srvURL,
		APIKey:            "test",
		RequestsPerSecond: 1000,
		Burst:             1000,
	})
	return NewTickets(client, time.Minute, statuses, live)
}

// The whole point of this collector. Resolved and Deleted hold ~58k tickets that
// will never change again; walking them is 1,182 requests and several minutes,
// which times out every cycle. They must be counted, never downloaded.
func TestClosedTicketsAreCountedNotWalked(t *testing.T) {
	fake := &fakeTickets{
		counts: map[string]int{
			"Open": 2, "Pending": 0, "Resolved": 30535, "Closed": 659, "Deleted": 27879,
		},
		live: map[string][]map[string]any{
			"Open": {
				ticket(1, "High", "Guy", time.Now().Add(-48*time.Hour)),
				ticket(2, "Low", "", time.Now().Add(-2*time.Hour)),
			},
		},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	tk := newTestTickets(t, srv.URL,
		[]string{"Open", "Pending", "Resolved", "Closed", "Deleted"},
		[]string{"Open", "Pending"})

	if err := tk.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	for _, status := range []string{"Resolved", "Closed", "Deleted"} {
		if fake.walked(status) {
			t.Fatalf("%s was walked — the corpus must only ever be counted", status)
		}
	}
	if !fake.walked("Open") {
		t.Fatal("Open was not walked, so queue age and priority cannot be computed")
	}

	// One unfiltered count + one count per status + one page per live status.
	// Nowhere near the 1,182 a naive walk would cost.
	if n := len(fake.requests); n > 10 {
		t.Fatalf("used %d requests; the count-only design should need under 10", n)
	}

	expected := `
# HELP atera_tickets Ticket count per status, from the API's own totalItemCount.
# TYPE atera_tickets gauge
atera_tickets{status="Closed"} 659
atera_tickets{status="Deleted"} 27879
atera_tickets{status="Open"} 2
atera_tickets{status="Pending"} 0
atera_tickets{status="Resolved"} 30535
`
	if err := testutil.CollectAndCompare(tk, strings.NewReader(expected), "atera_tickets"); err != nil {
		t.Fatal(err)
	}
}

// atera_tickets_total exists so that tickets carrying a status the filter cannot
// express (4 of 59,077 in the real estate) show up as a gap rather than vanishing.
func TestUnfilteredTotalExposesMissingStatuses(t *testing.T) {
	fake := &fakeTickets{
		counts: map[string]int{"Open": 0, "Resolved": 100, "Ghost": 4},
		live:   map[string][]map[string]any{},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	// Ghost is deliberately not in the configured status list.
	tk := newTestTickets(t, srv.URL, []string{"Open", "Resolved"}, []string{"Open"})
	if err := tk.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP atera_tickets_total Unfiltered ticket count. Compare against sum(atera_tickets) to spot statuses missing from the configured list.
# TYPE atera_tickets_total gauge
atera_tickets_total 104
`
	if err := testutil.CollectAndCompare(tk, strings.NewReader(expected), "atera_tickets_total"); err != nil {
		t.Fatal(err)
	}
}

// An unassigned ticket must carry an explicit label. An empty label value is
// treated as absent by Prometheus and would drop the series out of `sum by`.
func TestUnassignedTechnicianIsLabelled(t *testing.T) {
	fake := &fakeTickets{
		counts: map[string]int{"Open": 2},
		live: map[string][]map[string]any{
			"Open": {
				ticket(1, "High", "Guy Voloshin", time.Now()),
				ticket(2, "Low", "", time.Now()),
			},
		},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	tk := newTestTickets(t, srv.URL, []string{"Open"}, []string{"Open"})
	if err := tk.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP atera_open_tickets_by_technician Open ticket count per assigned technician. Answers who is sitting on work.
# TYPE atera_open_tickets_by_technician gauge
atera_open_tickets_by_technician{technician="Guy Voloshin"} 1
atera_open_tickets_by_technician{technician="unassigned"} 1
`
	if err := testutil.CollectAndCompare(tk, strings.NewReader(expected),
		"atera_open_tickets_by_technician"); err != nil {
		t.Fatal(err)
	}
}

// Queue age is the number that says whether anyone is working the queue, so the
// oldest open ticket must win regardless of the order the API returns them in.
func TestOldestOpenTicketWins(t *testing.T) {
	oldest := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second)

	fake := &fakeTickets{
		counts: map[string]int{"Open": 3},
		live: map[string][]map[string]any{
			"Open": {
				ticket(1, "Low", "Guy", time.Now().Add(-time.Hour)),
				ticket(2, "High", "Guy", oldest),
				ticket(3, "Low", "Guy", time.Now().Add(-2*time.Hour)),
			},
		},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	tk := newTestTickets(t, srv.URL, []string{"Open"}, []string{"Open"})
	if err := tk.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got := gaugeValue(t, tk, "atera_oldest_open_ticket_timestamp_seconds")
	if int64(got) != oldest.Unix() {
		t.Fatalf("oldest open ticket = %d, want %d", int64(got), oldest.Unix())
	}
}

// gaugeValue reads a single unlabelled gauge out of a collector that emits many.
// testutil.ToFloat64 cannot be used here — it panics unless the collector yields
// exactly one metric.
func gaugeValue(t *testing.T, c prometheus.Collector, name string) float64 {
	t.Helper()

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		m := f.GetMetric()
		if len(m) != 1 {
			t.Fatalf("%s: got %d series, want 1", name, len(m))
		}
		return m[0].GetGauge().GetValue()
	}
	t.Fatalf("%s not found in collected metrics", name)
	return 0
}

// An empty queue must publish a zero, not nothing. A missing series is
// indistinguishable from a broken exporter on a dashboard.
func TestEmptyQueueStillReportsZero(t *testing.T) {
	fake := &fakeTickets{
		counts: map[string]int{"Open": 0, "Pending": 0},
		live:   map[string][]map[string]any{},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	tk := newTestTickets(t, srv.URL, []string{"Open", "Pending"}, []string{"Open", "Pending"})
	if err := tk.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP atera_tickets Ticket count per status, from the API's own totalItemCount.
# TYPE atera_tickets gauge
atera_tickets{status="Open"} 0
atera_tickets{status="Pending"} 0
`
	if err := testutil.CollectAndCompare(tk, strings.NewReader(expected), "atera_tickets"); err != nil {
		t.Fatal(err)
	}
}
