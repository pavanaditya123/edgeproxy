# Benchmarks

Measured on 2026-10-26 with the `docker compose` stack and k6.

**Machine:** laptop, Intel Core i7-12700H (14 cores / 20 threads), 16 GB RAM,
Docker Desktop on Windows 11 (WSL 2 VM with 20 CPUs, 8 GB).

**Important caveat:** the load generator (k6), both edgeproxy instances, the
three backends, Redis, Toxiproxy, Prometheus, Grafana and Jaeger all share
that one machine. These numbers show relative behaviour and a lower bound on
what edgeproxy can do; they are not a measurement of its ceiling.

**Load shape:** k6 `constant-arrival-rate` (an open model: requests keep
arriving at the target rate even when responses slow down, like real users),
split evenly across the two proxies. Each request is `GET /api/items/{n}`
with one of 1,000 `X-User-ID` values. Unless stated otherwise, every request
also passes through the Redis-backed rate limiter (a rule with a limit high
enough never to reject), so each one pays for a Redis round trip.

Reproduce:

```bash
docker compose up -d --build
```

```bash
RATE=3000 DURATION=20s scripts/bench.sh
```

```bash
RATE=500 scripts/chaos.sh
```

## Balancing strategies at 3,000 req/s

Each strategy was switched in with a hot reload (`POST /-/reload`) and run for
20 s after a warm-up.

| Strategy | Achieved req/s | Errors | p50 | p95 | p99 |
|---|---|---|---|---|---|
| round_robin | 3,000 | 0% | 1.41 ms | 2.53 ms | 6.99 ms |
| weighted_round_robin | 3,000 | 0% | 1.41 ms | 2.49 ms | 5.68 ms |
| least_conn | 3,000 | 0% | 1.41 ms | 2.45 ms | 5.63 ms |
| p2c | 3,000 | 0% | 1.42 ms | 2.44 ms | 6.95 ms |
| consistent_hash | 3,000 | 0% | 1.43 ms | 2.50 ms | 5.77 ms |

With identical, healthy backends the strategies perform alike, as expected:
the differences only show when backends differ, which the next two
sections (and the e2e tests) cover. The first strategy measured without a
warm-up showed a 56 ms p99 from cold connection pools, which is why
`bench.sh` now warms up first.

## Cost of the Redis rate limiter

p2c at 3,000 req/s, 20 s each:

| Setup | p50 | p99 |
|---|---|---|
| Rate limit rule matching every request (Redis Lua call per request) | 1.43 ms | 6.23 ms |
| No matching rule (no Redis call) | 0.98 ms | 3.69 ms |
| **Added by Redis** | **+0.45 ms** | **+2.5 ms** |

The 50 ms Redis timeout caps the worst case: a slow Redis can never add more
than that before the limiter falls back to local token buckets.

## Higher load

p2c at 6,000 req/s: **5,985 req/s achieved, 0 errors**, p50 1.91 ms, p99
63.4 ms. p99 climbing here is the shared laptop saturating (k6 alone needs
several cores at this rate), not a proxy error.

## Chaos test: breaking things under load

`scripts/chaos.sh` at 500 req/s for 60 s:

| Time | Event |
|---|---|
| 10 s | `docker compose kill backend-a` (crash, no graceful shutdown) |
| 20 s | 500 ms latency added to backend-c through Toxiproxy |
| 30 s | Redis stopped |
| 40 s | Everything restored |

| Requests | Failed | p50 | p95 | p99 | Max |
|---|---|---|---|---|---|
| 30,001 | **0 (0.000%)** | 1.22 ms | 51.4 ms | 52.1 ms | 999 ms |

What made zero failures possible:

- **Crashed backend:** requests that hit backend-a after it died got a
  connection error *before* any response, so they were retried on another
  backend; active health checks removed it within about 2 s.
- **Slow backend:** p2c kept sending to whichever of two sampled backends had
  fewer requests in flight, so the 500 ms backend got little traffic. That's
  what lifted p95/p99 to ~51 ms instead of 500 ms.
- **Redis down:** the rate limiter fell back to per-instance token buckets
  (`on_redis_error: local`), so no request failed because of the limiter.

## Earlier smoke test

1,000 req/s for 30 s: 30,001 requests, 0 errors, p50 1.21 ms, p99 3.36 ms.

## In CI (GitHub Actions, `ubuntu-latest`)

The `Load and chaos tests` job runs the same stack on every push. A run on
2026-10-26:

| Run | req/s | Errors | p50 | p99 |
|---|---|---|---|---|
| Load: 1,000 req/s for 30 s | 1,000 | 0% | 0.44 ms | 1.28 ms |
| Chaos: 500 req/s for 60 s, with the failures above | 500 | **0%** | 0.43 ms | 50.5 ms |

The build fails if the load test crosses 0.1% errors or 250 ms p99, or the
chaos test crosses 0.1% errors.
