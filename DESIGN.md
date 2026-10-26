# Design notes

Why edgeproxy is built the way it is, what it deliberately doesn't do, and
what would change at 100× the scale. The [README](README.md) explains *what*
each piece does; this explains the *choices*.

## Goals and non-goals

**Goals:** an L7 HTTP load balancer that stays correct when backends fail,
degrades gracefully when its own dependencies (Redis, a collector) fail, can
be reconfigured live, and can be observed and tested end to end.

**Non-goals:** L4 (TCP/UDP) proxying, routing by host or path to different
backend pools, WebAssembly or scripting extensions, a control-plane API like
Envoy's xDS. Each is a reasonable next step (see the last section), but none
is needed to show the core ideas.

## Request path

```
RequestID → Tracing → AccessLog → RateLimit → Proxy → (balancer, retries) → backend
```

Each middleware is a plain `func(http.Handler) http.Handler`, so the chain is
built once in `main` and costs one function call per layer. Ordering
decisions:

- **Request ID first** so every later layer can record it.
- **Logging and tracing outside the rate limiter** so rejected requests still
  show up. Rate-limited traffic is exactly what you want to see during an attack.
- **Tracing is only added when enabled.** The proxy's per-attempt spans use the
  global OpenTelemetry tracer, which is a no-op until configured, so tracing
  costs nothing when off.

## Concurrency model

- **One goroutine per request** (Go's `net/http` model); no request touches
  another's state except through the backend table.
- **Backend state is atomics, not locks:** `alive`, `inflight`, `weight`,
  `maxInflight` and the circuit-breaker fields are `atomic.*` values, because
  every request reads them and the health checker, outlier detector and
  reloads write them. A mutex there would serialise all traffic.
- **Pools are immutable and swapped with `atomic.Pointer`.** A reload builds a
  complete new pool (backends, balancer, forwarders, detector, retry limit) and
  publishes it in one store. A request loads the pointer once and uses that
  generation throughout, so it never sees half a reload, and nothing has to
  be locked or drained.
- **Where locks remain, they're scoped small:** smooth WRR needs a mutex (its
  scores are a shared sequence); the local rate limiter shards its map into 16
  locks; the outlier detector locks only while deciding an ejection, so
  `max_ejection_percent` can't be overshot by two simultaneous ejections.
- Every test runs under `-race`, including the e2e binaries, because these
  invariants are easy to break silently.

## Health: two signals, combined conservatively

A backend is eligible when **all** of these hold: active probes say up, the
circuit breaker isn't open, it's under its in-flight cap, and this request
hasn't tried it yet.

- **Active checks** catch dead backends even without traffic, but are slow
  (interval × threshold) and can be fooled by a backend whose `/healthz` is
  fine while real work fails.
- **Passive checks** catch exactly that case immediately, but need traffic to
  notice anything. Using both covers each one's blind spot.
- **Thresholds and exponential ejection** trade detection speed for
  stability. A flapping backend is worse than a slow-to-detect one, because
  flapping moves load around unpredictably and defeats caching.
- **`max_ejection_percent`** exists because the most dangerous failure is a
  *correlated* one: a bad deploy or a shared database outage makes every
  backend fail at once. Ejecting them all would turn partial errors into a
  total outage, so past 50% the detector stops ejecting and keeps serving.

## Retries

Retrying makes individual requests more reliable and the system as a whole
*less* stable, so retries are deliberately narrow:

- **Only when no response came back.** A 500 is an answer; retrying it would
  double the load on a backend that's probably overloaded already.
- **Only idempotent methods with no body.** The proxy can't know whether a
  POST was processed before the connection died. Correctness beats availability here.
- **Never the same backend twice** in one request.
- **The retry budget (20% of the last 10 s, 10/s floor)** bounds the
  amplification: in the worst case traffic grows 1.2×, not 3×. The floor keeps
  quiet services able to retry a lone failure.

## Load balancing

| Strategy | When to use it | Cost per pick |
|---|---|---|
| Round robin | Identical, stateless backends | O(1), lock-free |
| Smooth weighted RR | Backends of different sizes | O(n), one mutex |
| Least connections | Variable request cost, one proxy | O(n) scan |
| P2C | Variable cost, *many* proxies | O(1) expected |
| Consistent hash | Per-user caches, sticky sessions | O(log n) binary search |

P2C is the recommended default for production fleets. Least-connections is
optimal for one proxy, but with many proxies each sees only its own in-flight
counts and they all converge on the same "best" backend at once (herding).
Sampling two at random keeps nearly all the benefit and removes the herding.

Consistent hashing uses 128 virtual nodes per backend, a 64-bit FNV hash with
a splitmix64 finaliser (FNV alone clusters similar strings like `host#1`,
`host#2`), and *skips* unavailable backends on the ring rather than rebuilding
it. So a backend that recovers gets exactly its old keys back.

## Rate limiting

- **Two algorithms on purpose.** Locally, a token bucket is the cheapest
  correct choice (one float per key, refilled lazily on access). Across a
  fleet, a sliding-window counter needs only two integers per key in Redis
  and one round trip, where a sliding *log* would store every timestamp.
- **Atomicity via Lua**, not `WATCH/MULTI`: the read-check-increment runs as
  one script, so concurrent proxies can't both take the last slot, and there's
  no optimistic-retry loop under contention.
- **Redis Cluster ready:** both window keys share a `{hash tag}`, so they live
  on the same shard, which a multi-key script requires.
- **Failure handling is a policy, not an accident.** A 50 ms timeout bounds
  the latency Redis can add; `on_redis_error` chooses between local limiting
  (default), fail-open and fail-closed. Measured cost of the Redis path:
  +0.45 ms p50, +2.5 ms p99 ([BENCHMARKS.md](BENCHMARKS.md)).
- **Bounded memory:** the local limiter evicts least-recently-used keys per
  shard, so a flood of distinct client IPs can't exhaust memory. The trade-off
  is that an evicted client gets a fresh bucket, which is acceptable because an
  attacker rotating through enough IPs to cause evictions isn't stopped by a
  per-IP limit anyway.
- **Known gap:** limits key on the TCP peer IP. Behind another load balancer
  every client shares that LB's IP. The fix is a `trusted_proxies` list (CIDRs
  whose `X-Forwarded-For` may be believed); until then, key on an API-key
  header in that setup.

## Configuration and reload

- **Fail loudly at load time:** unknown fields are rejected (a typo like
  `max_retires` would otherwise be silently ignored), `Validate` reports every
  problem at once, and `-check` validates without starting anything. The
  parser is fuzzed in CI.
- **Reload is all-or-nothing:** a new config is parsed, validated, its new
  backends probed, and only then swapped in. A bad file changes nothing except
  a log line and the `edgeproxy_config_reloads_total{result="failure"}` counter.
- **Some settings are restart-only** (listeners, timeouts, TLS, tracing, Redis
  connection), because changing them live means re-plumbing long-lived objects
  for little benefit. edgeproxy logs which ones changed instead of silently
  ignoring them.
- **DNS discovery sorts addresses** (DNS rotates answer order) so the backend
  list and the hash ring stay stable, and keeps the previous answer when a
  lookup fails, so a DNS blip can't empty the pool.

## Shutdown

Readiness fails first, then the process keeps serving for `drain_delay`, then
the listener closes and in-flight requests get `shutdown_grace`. That order
matters: in Kubernetes, endpoint removal propagates asynchronously, so closing
the listener immediately on SIGTERM would reset connections that are still
being routed to the pod. `terminationGracePeriodSeconds` (30 s) is set above
`drain_delay + shutdown_grace` (20 s) so the kubelet never kills the pod mid-drain.

## Security posture

- Admin endpoints (metrics, backend list, reload) on a separate port that the
  public Service doesn't expose.
- Client-supplied `X-Forwarded-For` is discarded, not appended to; request IDs
  are validated against a safe character set (no log injection); query
  strings never reach the access log.
- `ReadHeaderTimeout` defends against slowloris; outbound connections never
  honour `HTTP_PROXY` from the environment.
- TLS 1.2 minimum; certificates rotate without a restart; Redis passwords come
  from the environment, never the config file.
- Distroless, non-root, read-only-filesystem containers with every Linux
  capability dropped; govulncheck gates every build (it caught an x/net
  vulnerability during development).

## What I'd change at 100× scale

1. **A control plane instead of files.** Push config (backends, routes,
   limits) over a streaming API like Envoy's xDS, so thousands of proxies
   converge in seconds without file distribution.
2. **Routing by host and path to separate pools**, with per-route timeouts,
   retry policies and limits. One pool per service is the next real feature.
3. **Locality-aware balancing:** prefer backends in the same zone, fail over
   across zones, to cut latency and cross-zone network cost.
4. **Rate limiting off the hot path:** check against a local token bucket and
   reconcile with Redis asynchronously in batches (or use a dedicated
   rate-limit service), trading a little accuracy for zero added latency.
   At very high request rates per key, a single Redis key becomes a hot shard.
5. **Adaptive concurrency limits** (as in Netflix's concurrency-limits):
   measure latency and shrink each backend's in-flight cap automatically when
   it slows down, instead of a fixed `max_inflight_per_backend`.
6. **Hedged requests** for idempotent reads: if the first attempt is slower
   than p95, send a second one and take whichever answers first, cutting tail
   latency at a small cost in extra load.
7. **Connection-level efficiency:** HTTP/2 or HTTP/3 to backends (fewer
   connections), `SO_REUSEPORT` listeners, and profile-guided optimisation
   builds. Measure first; at the rates tested here the proxy isn't the bottleneck.
8. **L4 mode in front:** at very large scale an L4 balancer (ECMP plus
   something like Maglev or Katran) spreads connections across many L7 proxies
   like this one, which is how the large cloud and social companies layer it.
