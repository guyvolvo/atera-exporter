
# atera-exporter

Prometheus exporter for the Atera RMM API. Polls Atera on a background schedule and
serves the latest metrics at `/metrics`.

Atera's API paginates at a hard cap of 50 items per page and is rate limited.
Fetching during a scrape would mean N sequential HTTP calls inside Prometheus' 10s
scrape timeout, and every additional scraper would multiply the API load.

Instead each domain refreshes on its own ticker into an immutable snapshot, and a
scrape is a memory read. Scrape interval and poll interval are therefore
independent.

## Quick start
### Docker run
```
docker run -d --name atera-exporter \
  -e ATERA_API_KEY=your-key-here \
  -p 127.0.0.1:9199:9199 \
  ghcr.io/guyvolvo/atera-exporter:latest
```
Test : 
`curl -s localhost:9199/metrics | grep atera_agents_total`
### Docker compose 
A ready-made compose file is in [`deploy/docker-compose.yml`](deploy/docker-compose.yml).
**Or**
create a `docker-compose.yml` and an adjacent `.env`
holding `ATERA_API_KEY`

Binding to `127.0.0.1` means `/metrics` never reaches the network, so it needs no
firewall rule and no auth in front of it. If Prometheus runs on a *different* host,
bind to a routable address and restrict it via the firewall

## API version and auth

This targets the **v3** API (`https://app.atera.com/api/v3`) with
`Authorization: Bearer <key>`. v3 keys are JWTs.


### Debugging and Verifying

Atera's field names vary by plan. Dump a raw page and check it against
`internal/atera/models.go`:

```go
ATERA_API_KEY=xxx go run ./cmd/atera-exporter -dump /agents
ATERA_API_KEY=xxx go run ./cmd/atera-exporter -dump /alerts
ATERA_API_KEY=xxx go run ./cmd/atera-exporter -dump /tickets
```
**PowerShell:**
```powershell
$env:ATERA_API_KEY = 'your-key-here'
go run ./cmd/atera-exporter -dump /agents
```

## Add a scrape job to prometheus.yml

```yaml
  - job_name: 'atera'
    static_configs:
      - targets: ['127.0.0.1:9199']
```

Then `promtool check config prometheus.yml` and reload Prometheus. Confirm the
target is UP under **Status > Targets**.

## Dashboard

The exporter comes with a handy pre-built Grafana dashboard :)
 
Import [`deploy/grafana/atera-fleet.json`](deploy/grafana/atera-fleet.json) and pick
your Prometheus datasource.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `ATERA_API_KEY` | — | Required. |
| `ATERA_BASE_URL` | `https://app.atera.com/api/v3` | |
| `LISTEN_ADDR` | `:9199` | Not 9100 — that is node_exporter. |
| `ATERA_RPS` | `3` | Request budget shared across all domains. |
| `ATERA_BURST` | `5` | |
| `POLL_TIMEOUT` | `4m` | Must exceed pages ÷ `ATERA_RPS` for your fleet size. |
| `POLL_INTERVAL_AGENTS` | `5m` | Atera agents only check in about this often anyway. |
| `POLL_INTERVAL_ALERTS` | `1m` | |
| `POLL_INTERVAL_TICKETS` | `2m` | |
| `ATERA_TICKET_STATUSES` | `Open,Pending,Resolved,Closed,Deleted` | Counted cheaply, one request each. |
| `ATERA_TICKET_LIVE_STATUSES` | `Open,Pending` | Fetched in full. Live work only. |

### Why tickets are counted, not fetched

Note that an Atera tenant can hold a large number of tickets which would cause the cycle to time out and to not report on tickets at all 

So counts come from `totalItemCount` on a filtered `itemsInPage=1` request: one
request per status. Only `ATERA_TICKET_LIVE_STATUSES` is
walked in full, for queue age and the priority/technician breakdown.

## Metrics

Descriptive labels live only on `atera_agent_info`. Join them onto numeric series at
query time, so a hostname or OS change does not churn series everywhere:

```promql
atera_agent_online * on(agent_id) group_left(hostname,folder,os) atera_agent_info
```

Percent of the fleet online — the reason this exists. It is a query, not exporter
code:

```promql
100 * sum(atera_agent_online) / count(atera_agent_online)
100 * sum by (folder) (atera_agent_online) / count by (folder) (atera_agent_online)
```

Aggregates group by `folder`, not `customer`. On a single-tenant estate every agent
carries the same `CustomerName`, so a per-customer breakdown is one degenerate
bucket; folders are the grouping that means something. MSPs can group by the
`customer` label on `atera_agent_info` instead. Agents with no folder report
`folder="unassigned"` rather than an empty label, which Prometheus treats as absent.

**Fleet** — `atera_agent_info`, `atera_agent_online`, `atera_agents_total`,
`atera_agent_last_seen_timestamp_seconds`, `atera_agent_last_reboot_timestamp_seconds`,
`atera_agent_memory_bytes`, `atera_agent_disk_total_bytes`, `atera_agent_disk_free_bytes`.

**Alerts** — `atera_alerts`, `atera_oldest_alert_timestamp_seconds`. Archived alerts
are excluded: Atera's `/alerts` returns the full history, and counting archived
alerts as open gives a permanently red dashboard that never clears.

**Tickets** — `atera_tickets` (per status), `atera_tickets_total` (unfiltered),
`atera_open_tickets` (per priority), `atera_open_tickets_by_technician`,
`atera_oldest_open_ticket_timestamp_seconds`.

`atera_tickets_total` exists so tickets carrying a status the API's filter cannot
express surface as a gap instead of vanishing:

```promql
atera_tickets_total - sum(atera_tickets)
```

**Polling health** — `atera_up`, `atera_poll_errors_total`,
`atera_poll_duration_seconds`, `atera_last_success_timestamp_seconds`,
`atera_api_requests_total`.

Watch `atera_api_requests_total{code="429"}`. If it moves off zero, Atera is rate
limiting you — lower `ATERA_RPS`.

## Not exported

Billing and invoices. They are rows in a table, not time series; forcing them into
Prometheus produces ugly metrics and no insight. Point Grafana's Infinity datasource
at the API directly if you want them on a dashboard.
