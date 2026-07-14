package atera

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(url string) *Client {
	return NewClient(Options{
		BaseURL:           url,
		APIKey:            "test",
		RequestsPerSecond: 1000,
		Burst:             1000,
	})
}

// A 401 means the API key is wrong. Retrying it four times with backoff wastes
// ~7 seconds and four requests per poll and cannot possibly succeed.
func TestAuthFailureIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Agents(context.Background())
	if err == nil {
		t.Fatal("expected an error on 401")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("401 was attempted %d times, want 1 (4xx must not be retried)", n)
	}
}

// 429 is the one status where retrying is correct, and Retry-After is the server
// telling us exactly how long to wait. Ignoring it means hammering an API that
// has just asked us to stop.
func TestRateLimitIsRetriedAndRetryAfterHonoured(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":          []map[string]any{{"AgentID": 1, "MachineName": "PC1"}},
			"page":           1,
			"itemsInPage":    1,
			"totalItemCount": 1,
			"totalPages":     1,
		})
	})) //nolint:bodyclose
	defer srv.Close()

	start := time.Now()
	agents, err := testClient(srv.URL).Agents(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected recovery after 429: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	if calls.Load() != 2 {
		t.Fatalf("made %d calls, want 2 (one 429, one success)", calls.Load())
	}
	if elapsed < time.Second {
		t.Fatalf("retried after %v, want at least the 1s Retry-After", elapsed)
	}
}

// A transient 500 must not fail the poll outright - Atera has bad minutes.
func TestServerErrorIsRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":          []map[string]any{{"AgentID": 7}},
			"page":           1,
			"itemsInPage":    1,
			"totalItemCount": 1,
			"totalPages":     1,
		})
	}))
	defer srv.Close()

	agents, err := testClient(srv.URL).Agents(context.Background())
	if err != nil {
		t.Fatalf("expected recovery after transient 500s: %v", err)
	}
	if len(agents) != 1 || agents[0].AgentID != 7 {
		t.Fatalf("got %+v, want one agent with ID 7", agents)
	}
}

// The walk must stop on totalPages. Trusting the item count alone, or looping
// until an empty page, risks an unbounded walk against a misbehaving API.
func TestListStopsAtTotalPages(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":          []map[string]any{{"AgentID": page}},
			"page":           page,
			"itemsInPage":    1,
			"totalItemCount": 3,
			"totalPages":     3,
		})
	}))
	defer srv.Close()

	agents, err := testClient(srv.URL).Agents(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(agents))
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("made %d requests, want exactly 3", n)
	}
}

// count must read totalItemCount without downloading any items. This is what
// keeps the ticket corpus from being walked on every poll.
func TestCountTransfersNoItems(t *testing.T) {
	var gotItemsInPage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotItemsInPage = r.URL.Query().Get("itemsInPage")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":          []map[string]any{},
			"totalItemCount": 12000,
			"totalPages":     12000,
		})
	}))
	defer srv.Close()

	n, err := testClient(srv.URL).TicketCount(context.Background(), "Resolved")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 12000 {
		t.Fatalf("count = %d, want 12000", n)
	}
	if gotItemsInPage != "1" {
		t.Fatalf("itemsInPage=%q, want \"1\" - a count must not transfer items", gotItemsInPage)
	}
}

// Atera returns timestamps in several shapes depending on the endpoint. A strict
// time.Time unmarshal rejects the zone-less ones and would fail the whole page.
func TestTolerantTimeParsing(t *testing.T) {
	cases := map[string]bool{
		`"2026-07-13T22:36:33Z"`:        true,
		`"2026-07-13T22:36:33.5766667"`: true,
		`"2026-07-13T22:36:33"`:         true,
		`"2026-07-13"`:                  true,
		`""`:                            false,
		`null`:                          false,
		`"not-a-timestamp-at-all"`:      false,
	}

	for input, wantParsed := range cases {
		var tm Time
		if err := json.Unmarshal([]byte(input), &tm); err != nil {
			t.Fatalf("%s: unmarshal must never error, got %v", input, err)
		}
		if got := !tm.IsZero(); got != wantParsed {
			t.Errorf("%s: parsed=%v, want %v", input, got, wantParsed)
		}
	}
}

// Hebrew-locale tenants embed bidi marks in strings. They are invisible, so a
// label carrying them looks right while never matching in PromQL.
func TestCleanStripsBidiMarks(t *testing.T) {
	got := clean("\u200F\u200FMicrosoft Windows 11 Pro  x64")
	want := "Microsoft Windows 11 Pro x64"
	if got != want {
		t.Fatalf("clean() = %q, want %q", got, want)
	}
}
