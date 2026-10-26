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
- [x] GitHub Actions CI on Linux: gofmt, `go mod tidy` check, `go vet`, build, `go test -race`
- [x] Go installed, tests passing, end-to-end demo verified
- [ ] Push to GitHub and confirm the first CI run is green (first real `-race` run)
- [ ] Verify graceful shutdown with a real Ctrl+C / SIGTERM

**Interview topics:** L4 vs L7, keep-alive and connection pooling, why
`MaxIdleConnsPerHost` matters, consistent hashing and why only ~1/N keys move,
why POST isn't retried, liveness vs readiness.

## Phase 2 (week 2): resilience

- [ ] **Passive health / outlier ejection:** eject a backend after N consecutive 5xx/transport errors, before the next active probe
- [ ] **Retry budget:** retries capped at ~20% of request volume, so retries can't amplify an outage (retry storm)
- [ ] **Per-try timeout** separate from the overall request deadline
- [ ] **Circuit breaker** per backend (closed → open → half-open)
- [ ] **Power-of-two-choices** balancer (what Envoy/Finagle use) and benchmark it against least-conn
- [ ] **Weighted backends** (smooth weighted round robin, the Nginx algorithm)
- [ ] **Hot reload:** swap the backend set on config-file change via `atomic.Pointer`, without dropping requests
- [ ] Fuzz test the config parser; run all tests with `-race`

**Done when:** killing a backend mid-load causes < 0.1% failed requests.

## Phase 3 (week 3): rate limiting

- [ ] Middleware chain (`func(http.Handler) http.Handler`)
- [ ] **Local token bucket** per key (client IP / API key header) with bounded memory (LRU eviction)
- [ ] Standard response headers: `429`, `Retry-After`, `RateLimit-Limit/Remaining/Reset`
- [ ] **Distributed limiter:** sliding-window counter in Redis, made atomic with a Lua script, so several proxy instances share one limit
- [ ] **Failure mode:** if Redis is down, fail open to the local limiter (and expose a metric + alert)
- [ ] Per-route rules in config (e.g. `/login` 5/min, `/api/*` 100/s)
- [ ] Measure the added p99 latency of the Redis round trip

**Interview topics:** token bucket vs leaky bucket vs sliding window, race
conditions without Lua/`MULTI`, fail-open vs fail-closed, hot keys.

## Phase 4 (week 4): observability

- [ ] Request IDs (`X-Request-ID`, generated if missing, propagated upstream)
- [ ] Structured access logs (method, path, status, latency, backend, bytes, request ID)
- [ ] **OpenTelemetry tracing** with W3C `traceparent` propagation
- [ ] Grafana dashboard JSON committed to the repo: RED metrics (Rate, Errors, Duration) per backend
- [ ] Prometheus alert rules: no healthy backends, error-rate SLO burn, Redis down

## Phase 5 (week 5): deploy, load test, break it

- [ ] Multi-stage `Dockerfile` → distroless image, runs as non-root
- [ ] `docker-compose.yml`: 2× edgeproxy, Redis, 3× demo backends, Prometheus, Grafana
- [ ] **k6** load tests; record req/s and p50/p99 for each strategy
- [ ] **Chaos tests:** kill a backend mid-test, stop Redis, inject latency with Toxiproxy or `tc netem`
- [ ] Extend CI: `staticcheck`, `govulncheck`, Docker image build
- [ ] (Stretch) Kubernetes manifests with readiness probe + `preStop` drain

## Phase 6 (week 6): polish and tell the story

- [ ] TLS termination (stretch: HTTP/2 to clients, automatic certs)
- [ ] `DESIGN.md`: architecture, trade-offs, what you'd do at 100× scale
- [ ] README benchmark table + Grafana screenshot + chaos-test results
- [ ] Resume bullets with **your real numbers** (template below)

---

## Resume bullet template (fill in the measured numbers)

- Built a Layer-7 load balancer in Go supporting round-robin, least-connections and consistent hashing, sustaining **__K req/s at __ ms p99** on 4 cores
- Designed active + passive health checking and budgeted retries, keeping error rate under **__%** while backends were killed mid-load
- Implemented a distributed sliding-window rate limiter with Redis + Lua shared across proxy replicas, with automatic fail-open to local token buckets during Redis outages
- Instrumented RED metrics, OpenTelemetry tracing and SLO alerts; validated with k6 load tests and Toxiproxy chaos tests in CI
