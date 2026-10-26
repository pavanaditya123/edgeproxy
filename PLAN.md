# edgeproxy: build plan

A production-grade **Layer-7 load balancer + distributed rate limiter** in Go,
with full observability, load tests and chaos tests. It is a small version of
what Envoy / Nginx / Google's GFE do, built so you can explain every line of it
in an interview.

```
                 ┌──────────────────────── edgeproxy ────────────────────────┐
 client ──HTTP──▶│ rate limiter ─▶ balancer ─▶ reverse proxy ─▶ retry logic │──▶ backend A
                 │      │            │  ▲            │                       │──▶ backend B
                 │      ▼            │  │ health     ▼                       │──▶ backend C
                 │    Redis          │  └── checker  metrics / logs / traces │
                 └───────────────────┴──────────────────────┬────────────────┘
                                                            ▼
                                                 Prometheus ─▶ Grafana
```

**Time:** 6 weeks at ~10–12 h/week. Each phase ends with something you can demo.

---

## Phase 1 (week 1): core proxy ✅ scaffolded

- [x] Go module layout (`cmd/`, `internal/`)
- [x] Reverse proxy built on `httputil.ReverseProxy` with a tuned connection pool
- [x] Three balancing strategies: round robin, least connections, consistent hashing (virtual nodes)
- [x] Active health checks with healthy/unhealthy thresholds (no flapping)
- [x] Retries on a *different* backend, only for idempotent requests without a body
- [x] Safe `X-Forwarded-For` (client-supplied value is discarded, not trusted)
- [x] Prometheus metrics, `/healthz`, `/readyz`, `/backends` on a separate admin port
- [x] Graceful shutdown with a drain delay (the Kubernetes-friendly way)
- [x] JSON config with validation (unknown fields rejected, catching typos)
- [x] Unit + integration tests, demo backend with latency/failure injection
- [x] GitHub Actions CI on Linux, in stages: lint → unit (`-race`, coverage) → e2e
- [x] End-to-end suite running the real binaries: balancing, forwarding, health ejection, retries, 503, stickiness, least-conn, graceful shutdown on SIGTERM
- [x] Go installed, tests passing, end-to-end demo verified
- [x] Pushed to GitHub, CI green

**Interview topics:** L4 vs L7, keep-alive and connection pooling, why
`MaxIdleConnsPerHost` matters, consistent hashing and why only ~1/N keys move,
why POST isn't retried, liveness vs readiness.

## Phase 2 (week 2): resilience

- [x] **Passive health / outlier ejection:** eject a backend after N consecutive 5xx/transport errors, before the next active probe; never eject more than `max_ejection_percent` of the pool
- [x] **Retry budget:** retries capped at 20% of request volume over 10 s (with a 10/s floor), so retries can't amplify an outage (retry storm)
- [x] **Per-try timeout:** `timeouts.response_header` bounds each attempt's time to first byte; there's deliberately no whole-request deadline, so long streaming responses aren't cut off
- [x] **Circuit breaker** per backend (closed → open → half-open), with exponential ejection backoff
- [x] **Load shedding:** `max_inflight_per_backend` stops sending to a saturated backend
- [x] **Power-of-two-choices** balancer (what Envoy/Finagle use); benchmarked against least-conn in Phase 5
- [x] **Weighted backends** (smooth weighted round robin, the Nginx algorithm)
- [x] **Hot reload:** file watch, SIGHUP or `POST /-/reload`; the new pool is probed, then swapped in via `atomic.Pointer` without dropping requests; invalid configs are rejected
- [x] Fuzz test the config parser (30 s in CI); all tests run with `-race`

**Done when:** killing a backend mid-load causes < 0.1% failed requests.

## Phase 3 (week 3): rate limiting

- [x] Middleware chain (`func(http.Handler) http.Handler`)
- [x] **Local token bucket** per key (client IP / API key header) with bounded memory (16 locked shards, LRU eviction)
- [x] Standard response headers: `429`, `Retry-After`, `RateLimit-Limit/Remaining/Reset`
- [x] **Distributed limiter:** sliding-window counter in Redis, made atomic with a Lua script (Redis Cluster hash tags), so several proxy instances share one limit
- [x] **Failure mode:** `on_redis_error` = `local` (default) / `allow` / `deny`, with a metric and an alert
- [x] Per-route rules in config (e.g. `POST /login` 5/min, `/` 100/s per API key), reloadable without a restart
- [ ] Measure the added p99 latency of the Redis round trip (Phase 5 load tests)
- [x] **E2E tests in CI** (Redis as a GitHub Actions service container): over-limit requests get `429` + `Retry-After`; per-API-key quotas; two proxies sharing one Redis enforce one shared limit; stopping Redis falls back to the local limiter

**Interview topics:** token bucket vs leaky bucket vs sliding window, race
conditions without Lua/`MULTI`, fail-open vs fail-closed, hot keys.

## Phase 4 (week 4): observability

- [x] Request IDs (`X-Request-ID`, generated if missing or unsafe, propagated to the backend and back)
- [x] Structured access logs (method, path, status, latency, backend, attempts, bytes, request ID, trace ID; never the query string)
- [x] **OpenTelemetry tracing** with W3C `traceparent` propagation: a server span per request, a client span per backend attempt, OTLP/HTTP export
- [x] Grafana dashboard JSON committed to the repo: RED metrics (Rate, Errors, Duration) per backend, health, retries, rate limits, runtime
- [x] Prometheus alert rules: multi-window SLO burn rate, no healthy backends, Redis down, and more, with `promtool` unit tests

## Phase 5 (week 5): deploy, load test, break it

- [x] Multi-stage `Dockerfile` → static, distroless image, runs as non-root
- [x] `docker-compose.yml`: 2× edgeproxy, Redis, 3× demo backends (one behind Toxiproxy), Prometheus, Grafana, Jaeger
- [x] **k6** load tests; req/s and p50/p99 recorded for each strategy ([BENCHMARKS.md](BENCHMARKS.md))
- [x] Redis round-trip cost measured: +0.45 ms p50, +2.5 ms p99
- [x] **Chaos tests:** kill a backend, inject 500 ms latency with Toxiproxy, stop Redis, all under load: 0 failures in 30,001 requests
- [x] Extend CI: `staticcheck`, `govulncheck` (caught and fixed an x/net vulnerability), Docker image build, `promtool` rule tests, `kubeconform`, load and chaos tests
- [x] Kubernetes manifests: readiness-gated draining, zero-downtime rolling updates, PDB, HPA, hardened security context
- [x] **DNS service discovery** so Kubernetes pods behind a headless Service are found automatically

**Done when:** killing a backend mid-load causes < 0.1% failed requests. ✅ Measured 0%.

## Phase 6 (week 6): polish and tell the story

- [x] TLS termination with HTTP/2, TLS 1.2+ minimum, HTTP→HTTPS redirect, and certificate rotation without a restart
- [x] `DESIGN.md`: architecture, trade-offs, known gaps, what I'd do at 100× scale
- [x] README rewritten for complete beginners, with 21 diagrams; benchmark and chaos results in [BENCHMARKS.md](BENCHMARKS.md)
- [x] Resume bullets with real numbers (below)
- [ ] Grafana screenshot in the README (take one from http://localhost:3000 while `docker compose run --rm k6 run /scripts/load.js` runs)

---

## Resume bullets (real, measured numbers)

- Built a production-style Layer-7 load balancer in Go with 5 balancing strategies (incl. power-of-two-choices and consistent hashing), sustaining **3,000 req/s at 1.4 ms p50 / ~6 ms p99** with 0 errors in a containerised benchmark
- Designed active health checks, per-backend circuit breakers and budgeted retries; chaos tests that killed a backend, injected 500 ms latency and stopped Redis under load produced **0 failed requests out of 30,001**
- Implemented a distributed sliding-window rate limiter with an atomic Redis Lua script shared across proxy replicas, adding only **0.45 ms p50**, with automatic fallback to sharded, memory-bounded local token buckets during Redis outages
- Added hot config reload, DNS service discovery, TLS with certificate rotation, and graceful draining for zero-downtime Kubernetes rollouts
- Instrumented RED metrics, OpenTelemetry tracing and SLO burn-rate alerts (unit-tested with promtool); gated every commit on 91 tests, fuzzing, the race detector, govulncheck and k6 load/chaos tests in GitHub Actions
