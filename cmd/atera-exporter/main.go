// Command atera-exporter polls the Atera RMM API and exposes fleet, alert and
// ticket state as Prometheus metrics.
//
// Run with -dump <endpoint> to print a raw API page. Atera's field names are not
// fully documented and vary by plan; use dump mode to verify the payload against
// the structs in internal/atera/models.go before trusting the metrics.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/guyvolvo/atera-exporter/internal/atera"
	"github.com/guyvolvo/atera-exporter/internal/collector"
	"github.com/guyvolvo/atera-exporter/internal/config"
)

func main() {
	dump := flag.String("dump", "", "print one raw API page for an endpoint (e.g. /agents) and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0 or 1")
	flag.Parse()

	// The distroless image has no shell and no curl, so the container's
	// healthcheck re-executes this binary rather than shelling out.
	if *healthcheck {
		if err := probe(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log, *dump); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// probe hits /healthz on the loopback address. It deliberately does not load the
// full config, so a healthcheck cannot fail merely because the API key is unset.
func probe() error {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":9199"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func run(log *slog.Logger, dump string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	apiRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "atera_api_requests_total",
		Help: "Atera API requests by endpoint path and HTTP status. Status 0 means the request never completed.",
	}, []string{"endpoint", "code"})
	reg.MustRegister(apiRequests)

	client := atera.NewClient(atera.Options{
		BaseURL:           cfg.BaseURL,
		APIKey:            cfg.APIKey,
		RequestsPerSecond: cfg.RequestsPerSecond,
		Burst:             cfg.Burst,
		Logger:            log,
		Observer: func(endpoint string, code int) {
			// Label with the path only. The query string carries a page number,
			// which would otherwise create a new series per page.
			apiRequests.WithLabelValues(pathOnly(endpoint), fmt.Sprint(code)).Inc()
		},
	})

	if dump != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		body, err := client.DumpPage(ctx, dump)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	}

	sched := collector.NewScheduler(log, cfg.PollTimeout,
		collector.NewAgents(client, cfg.AgentInterval),
		collector.NewAlerts(client, cfg.AlertInterval),
		collector.NewTickets(client, cfg.TicketInterval, cfg.TicketStatuses, cfg.TicketLiveStatuses),
	)
	if err := sched.Register(reg); err != nil {
		return fmt.Errorf("register collectors: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go sched.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func pathOnly(endpoint string) string {
	for i, r := range endpoint {
		if r == '?' {
			return endpoint[:i]
		}
	}
	return endpoint
}
