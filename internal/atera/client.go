package atera

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

const (
	// v3 issues JWT keys and authenticates with a Bearer header. v1 used a plain
	// key in X-API-KEY and rejects a v3 token with 401.
	DefaultBaseURL = "https://app.atera.com/api/v3"
	pageSize       = 50
	maxAttempts    = 4
)

// RequestObserver is called once per completed HTTP attempt, including retried
// ones, so the exporter can count API usage and rate limiting.
type RequestObserver func(endpoint string, statusCode int)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	limiter *rate.Limiter
	log     *slog.Logger
	observe RequestObserver
}

type Options struct {
	BaseURL           string
	APIKey            string
	RequestsPerSecond float64
	Burst             int
	Timeout           time.Duration
	Logger            *slog.Logger
	Observer          RequestObserver
}

func NewClient(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.Timeout == 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Observer == nil {
		opts.Observer = func(string, int) {}
	}
	return &Client{
		baseURL: opts.BaseURL,
		apiKey:  opts.APIKey,
		http:    &http.Client{Timeout: opts.Timeout},
		limiter: rate.NewLimiter(rate.Limit(opts.RequestsPerSecond), opts.Burst),
		log:     opts.Logger,
		observe: opts.Observer,
	}
}

// do issues one GET with rate limiting, retry and backoff. It returns the
// decoded body on success. Retries cover 429 and 5xx; 4xx is fatal because no
// amount of retrying fixes a bad API key.
func (c *Client) do(ctx context.Context, endpoint string, target any) error {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("rate limiter: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			c.observe(endpoint, 0)
			lastErr = fmt.Errorf("request %s: %w", endpoint, err)
			if err := c.backoff(ctx, attempt, 0); err != nil {
				return err
			}
			continue
		}

		c.observe(endpoint, resp.StatusCode)

		switch {
		case resp.StatusCode == http.StatusOK:
			err := json.NewDecoder(resp.Body).Decode(target)
			_ = resp.Body.Close()
			if err != nil {
				return fmt.Errorf("decode %s: %w", endpoint, err)
			}
			return nil

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			drain(resp.Body)
			lastErr = fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
			c.log.Warn("atera api retryable error",
				"endpoint", endpoint, "status", resp.StatusCode, "attempt", attempt)
			if err := c.backoff(ctx, attempt, retryAfter); err != nil {
				return err
			}

		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return fmt.Errorf("%s: status %d: %s", endpoint, resp.StatusCode, body)
		}
	}

	return fmt.Errorf("exhausted %d attempts: %w", maxAttempts, lastErr)
}

// backoff waits out an exponential delay with jitter, honoring Retry-After when
// the server supplied it. It returns an error only if the context dies.
func (c *Client) backoff(ctx context.Context, attempt int, retryAfter time.Duration) error {
	if attempt >= maxAttempts {
		return nil
	}
	delay := retryAfter
	if delay <= 0 {
		delay = time.Duration(1<<uint(attempt-1)) * time.Second
		delay += time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func drain(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}

// list walks every page of a paginated endpoint and returns the flattened
// items. It stops on totalPages rather than trusting nextLink, and hard-caps
// the walk so a malformed response cannot spin forever.
func list[T any](ctx context.Context, c *Client, path string, params url.Values) ([]T, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("itemsInPage", strconv.Itoa(pageSize))

	var out []T
	const maxPages = 1000

	for pageNum := 1; pageNum <= maxPages; pageNum++ {
		params.Set("page", strconv.Itoa(pageNum))
		endpoint := path + "?" + params.Encode()

		var p page[T]
		if err := c.do(ctx, endpoint, &p); err != nil {
			return nil, err
		}

		out = append(out, p.Items...)

		if p.TotalPages <= pageNum || len(p.Items) == 0 {
			break
		}
	}

	return out, nil
}

// count returns totalItemCount without transferring any items. The estate has
// ~59k tickets and itemsInPage is hard-capped at 50 by the API, so walking them
// to count them would be 1,182 requests per poll. This is one.
func count(ctx context.Context, c *Client, path string, params url.Values) (int, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("page", "1")
	params.Set("itemsInPage", "1")

	var p page[json.RawMessage]
	if err := c.do(ctx, path+"?"+params.Encode(), &p); err != nil {
		return 0, err
	}
	return p.TotalItemCount, nil
}

func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	return list[Agent](ctx, c, "/agents", nil)
}

func (c *Client) Alerts(ctx context.Context) ([]Alert, error) {
	return list[Alert](ctx, c, "/alerts", nil)
}

// TicketCount counts tickets in one status. Note that the filter parameter is
// ticketStatus; a misspelling such as `status` is silently ignored by the API and
// returns the unfiltered total, which looks like a working filter.
func (c *Client) TicketCount(ctx context.Context, status string) (int, error) {
	return count(ctx, c, "/tickets", url.Values{"ticketStatus": {status}})
}

// TicketCountAll is the unfiltered total, used to expose tickets whose status is
// not in the configured list rather than letting them vanish silently.
func (c *Client) TicketCountAll(ctx context.Context) (int, error) {
	return count(ctx, c, "/tickets", nil)
}

// TicketsByStatus walks every ticket in one status. Only call this for statuses
// that represent live work — the closed-and-resolved corpus is tens of thousands
// of rows that will never change again.
func (c *Client) TicketsByStatus(ctx context.Context, status string) ([]Ticket, error) {
	return list[Ticket](ctx, c, "/tickets", url.Values{"ticketStatus": {status}})
}

func (c *Client) Contracts(ctx context.Context) ([]Contract, error) {
	return list[Contract](ctx, c, "/contracts", nil)
}

// DumpPage fetches the first page of any endpoint undecoded. Atera's field
// names are not fully documented; use this to check the real payload against
// the structs in models.go.
func (c *Client) DumpPage(ctx context.Context, path string) ([]byte, error) {
	var raw json.RawMessage
	if err := c.do(ctx, path+"?page=1&itemsInPage=2", &raw); err != nil {
		return nil, err
	}
	return json.MarshalIndent(raw, "", "  ")
}
