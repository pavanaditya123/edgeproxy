# edgeproxy

A Layer-7 HTTP load balancer written in Go: health-checked backends, three
balancing strategies, safe retries, Prometheus metrics and graceful shutdown.
See [PLAN.md](PLAN.md) for the roadmap (rate limiting, tracing, load and chaos tests).

## Features (phase 1)

| Area | What it does |
|---|---|
| Balancing | `round_robin`, `least_conn`, `consistent_hash` (128 virtual nodes per backend, keyed on client IP or a header) |
| Health | Active probes with healthy/unhealthy thresholds; first result applies at startup |
| Retries | Failed `GET`/`HEAD`/`OPTIONS` without a body are retried on a *different* backend; `POST` never is |
| Security | Client-sent `X-Forwarded-For` is discarded; metrics live on a separate admin port |
| Ops | `/metrics`, `/healthz` (liveness), `/readyz` (readiness), `/backends`; drain delay + graceful shutdown on SIGTERM |
| Config | JSON with defaults; unknown fields and invalid values are rejected at startup |

## Layout

```
cmd/edgeproxy/       entrypoint, admin server, shutdown
cmd/demobackend/     toy upstream with latency/failure injection
internal/backend/    backend state (health, in-flight count)
internal/balancer/   round robin, least conn, consistent hash
internal/health/     active health checker
internal/proxy/      reverse proxy handler, retries, connection pool
internal/metrics/    Prometheus metrics
internal/config/     config loading and validation
configs/local.json   config for local runs
```

## Run it locally

Requires Go 1.22+. Fetch dependencies and run the tests:

```bash
go mod tidy
```

```bash
go test -race ./...
```

Start three demo backends, each in its own terminal:

```bash
go run ./cmd/demobackend -addr :9001 -name a
```

```bash
go run ./cmd/demobackend -addr :9002 -name b -latency 50ms
```

```bash
go run ./cmd/demobackend -addr :9003 -name c -fail-rate 0.1
```

Start the proxy:

```bash
go run ./cmd/edgeproxy -config configs/local.json
```

Send traffic and watch it rotate across `a`, `b`, `c`:

```bash
curl -s localhost:8080/hello
```

### Things to try

- **Take a backend out:** `curl -X POST localhost:9001/toggle-health`, then watch `curl -s localhost:9090/backends` flip it to `"alive": false` after two failed probes.
- **Kill a backend outright** (Ctrl+C in its terminal): `GET` requests keep succeeding because they're retried on another backend.
- **Sticky routing:** set `"strategy": "consistent_hash"` and send `curl -s -H "X-User-ID: 42" localhost:8080/` repeatedly; the same user always lands on the same backend.
- **Slow backend:** with `"strategy": "least_conn"`, backend `b` (50 ms latency) receives fewer requests under concurrent load.
- **Metrics:** `curl -s localhost:9090/metrics | grep edgeproxy_`

## Admin endpoints (port 9090)

| Path | Meaning |
|---|---|
| `/metrics` | Prometheus metrics: `edgeproxy_requests_total`, `edgeproxy_request_duration_seconds`, `edgeproxy_upstream_errors_total`, `edgeproxy_retries_total`, `edgeproxy_backend_up`, `edgeproxy_backend_inflight_requests` |
| `/healthz` | Liveness: the process is running |
| `/readyz` | Readiness: 503 while draining or when no backend is healthy |
| `/backends` | JSON health and in-flight count per backend |
