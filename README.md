# atera-exporter

Prometheus exporter for the Atera RMM API. Polls Atera on a background schedule
and serves the latest snapshot at `/metrics`.

Ships only the exporter. Prometheus, Alertmanager and Grafana already run on
`ubuntu-server` — this is one more scrape target for them, not a second stack.

## Why it polls instead of fetching on scrape

Atera's API is paginated at 50 items per page and rate limited. Fetching during a
scrape would mean N sequential HTTP calls inside Prometheus' 10s scrape timeout,
and every additional scraper would multiply the API load. Instead each domain
refreshes on its own ticker into an immutable snapshot, and a scrape is a memory
read. Scrape interval and poll interval are therefore independent.

## First run: verify the field names

Atera's field names vary by plan and are not fully documented. The structs in
`internal/atera/models.go` were written from the docs, not from a live payload.
Check them against what your tenant actually returns before trusting any number.

Set the key once per shell rather than inlining it, so it does not end up in shell
history on every command.

**PowerShell:**

```powershell
$env:ATERA_API_KEY = 'paste-key-here'

go run ./cmd/atera-exporter -dump /agents
go run ./cmd/atera-exporter -dump /alerts
go run ./cmd/atera-exporter -dump /tickets
```

**bash / Git Bash:**

```bash
export ATERA_API_KEY='paste-key-here'

go run ./cmd/atera-exporter -dump /agents
```

`VAR=value cmd` is bash-only syntax — in PowerShell it is parsed as a command name
and fails.

Fix `models.go` to match the payload. Everything downstream is wrong if this is
wrong.

Then run it and sanity-check the count against Atera's own GUI:

```powershell
go run ./cmd/atera-exporter
# in another shell:
(Invoke-WebRequest localhost:9199/metrics).Content -split "`n" | Select-String atera_agents_total
```

## Deploying

    scp deploy/docker-compose.yml administrator@192.168.1.19:/opt/atera-exporter/
    # create /opt/atera-exporter/.env with ATERA_API_KEY=..., then chmod 600 .env
    docker compose up -d

The container binds `127.0.0.1:9199`. Prometheus scrapes from the same host, so
`/metrics` never reaches the LAN, needs no firewall rule, and needs no auth in
front of it.

## Manual step: add the scrape job to Prometheus

Prometheus does not merge config fragments, so this has to be appended by hand to
`scrape_configs` in **`/etc/prometheus/prometheus.yml`** on `192.168.1.19`. It
follows the existing convention — `static_configs` with a `hostname` label, same
as the `windows_exporter` and `n8n` jobs already in that file:

```yaml
  - job_name: 'atera'
    static_configs:
      - targets: ['127.0.0.1:9199']
        labels:
          hostname: 'ubuntu-server'
```

Then:

    promtool check config /etc/prometheus/prometheus.yml
    sudo systemctl reload prometheus

Confirm the target is UP under **Status > Targets** in the Prometheus UI.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `ATERA_API_KEY` | — | Required. |
| `LISTEN_ADDR` | `:9199` | Not 9100 — node_exporter already owns that on this host. |
| `ATERA_RPS` | `3` | Request budget shared across all domains. |
| `ATERA_BURST` | `5` | |
| `POLL_TIMEOUT` | `4m` | Must exceed pages ÷ `ATERA_RPS` for your fleet size. |
| `POLL_INTERVAL_AGENTS` | `5m` | Atera agents only check in about this often anyway. |
| `POLL_INTERVAL_ALERTS` | `1m` | |
| `POLL_INTERVAL_TICKETS` | `2m` | |

## Metrics

Descriptive labels live only on `atera_agent_info`. Join them onto numeric series
at query time, so a hostname or OS change does not churn series everywhere:

    atera_agent_online * on(machine_id) group_left(hostname,customer,os) atera_agent_info

Percent of the fleet online — the reason this exists. It is a query, not exporter
code:

    sum(atera_agent_online) / count(atera_agent_online)
    sum by (customer) (atera_agent_online) / count by (customer) (atera_agent_online)

Fleet: `atera_agent_info`, `atera_agent_online`, `atera_agents_total`,
`atera_agent_last_seen_timestamp_seconds`, `atera_agent_disk_{total,free}_bytes`.

Alerts and tickets: `atera_alerts`, `atera_oldest_alert_timestamp_seconds`,
`atera_tickets`, `atera_oldest_open_ticket_timestamp_seconds`.

Health of polling itself: `atera_up`, `atera_poll_errors_total`,
`atera_poll_duration_seconds`, `atera_last_success_timestamp_seconds`,
`atera_api_requests_total`.

Watch `atera_api_requests_total{code="429"}`. If it moves off zero, Atera is rate
limiting — lower `ATERA_RPS`.

## Deliberately not exported

Billing and invoices. They are rows in a table, not time series — storing them in
Prometheus produces ugly metrics and no insight. Point Grafana's Infinity
datasource at the API directly if you ever want them on a dashboard.
