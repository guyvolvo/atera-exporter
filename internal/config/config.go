package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIKey     string
	BaseURL    string
	ListenAddr string

	// RequestsPerSecond is deliberately conservative. Atera enforces a rate
	// limit and returns 429 when exceeded; check the response headers against
	// your plan and raise this only if you have headroom.
	RequestsPerSecond float64
	Burst             int

	// PollTimeout bounds a full cycle for one domain, pagination included. It
	// must exceed the worst case of (pages / RequestsPerSecond) or large fleets
	// will time out mid-walk and never produce a snapshot.
	PollTimeout time.Duration

	AgentInterval  time.Duration
	AlertInterval  time.Duration
	TicketInterval time.Duration

	// TicketStatuses are counted via totalItemCount (one cheap request each).
	// TicketLiveStatuses are additionally fetched in full for age and priority
	// detail — keep this to statuses that represent live work. Fetching Resolved
	// would mean walking 30k tickets that will never change again.
	TicketStatuses     []string
	TicketLiveStatuses []string
}

func Load() (*Config, error) {
	key := os.Getenv("ATERA_API_KEY")
	if key == "" {
		return nil, errors.New("ATERA_API_KEY is required")
	}

	cfg := &Config{
		APIKey:            key,
		BaseURL:           envStr("ATERA_BASE_URL", "https://app.atera.com/api/v3"),
		ListenAddr:        envStr("LISTEN_ADDR", ":9199"),
		RequestsPerSecond: envFloat("ATERA_RPS", 3),
		Burst:             envInt("ATERA_BURST", 5),
		PollTimeout:       envDur("POLL_TIMEOUT", 4*time.Minute),
		AgentInterval:     envDur("POLL_INTERVAL_AGENTS", 5*time.Minute),
		AlertInterval:     envDur("POLL_INTERVAL_ALERTS", time.Minute),
		TicketInterval:    envDur("POLL_INTERVAL_TICKETS", 2*time.Minute),

		// Deleted is ~47% of this estate's ticket corpus and is easy to miss;
		// omitting it would leave sum(atera_tickets) far below atera_tickets_total
		// with no obvious cause.
		TicketStatuses:     envList("ATERA_TICKET_STATUSES", "Open,Pending,Resolved,Closed,Deleted"),
		TicketLiveStatuses: envList("ATERA_TICKET_LIVE_STATUSES", "Open,Pending"),
	}

	if cfg.RequestsPerSecond <= 0 {
		return nil, fmt.Errorf("ATERA_RPS must be > 0, got %v", cfg.RequestsPerSecond)
	}
	return cfg, nil
}

// envList parses a comma-separated list, trimming blanks so a trailing comma or
// stray space cannot produce an empty status that queries the API for nothing.
func envList(key, def string) []string {
	raw := envStr(key, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
