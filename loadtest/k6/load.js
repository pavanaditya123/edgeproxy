// Steady load across both proxies, with pass/fail thresholds.
//
//   docker compose run --rm k6 run /scripts/load.js
//   docker compose run --rm -e RATE=3000 -e DURATION=60s k6 run /scripts/load.js
//
// RATE is requests per second (an open model: k6 keeps sending at that rate
// even if responses slow down, the way real users do).
import http from "k6/http";
import { check } from "k6";

const targets = (__ENV.TARGETS || "http://localhost:8080").split(",");
const rate = Number(__ENV.RATE || 1000);
const duration = __ENV.DURATION || "30s";
const label = __ENV.LABEL || "load";

export const options = {
  discardResponseBodies: true,
  scenarios: {
    steady: {
      executor: "constant-arrival-rate",
      rate,
      timeUnit: "1s",
      duration,
      preAllocatedVUs: Math.max(50, rate / 10),
      maxVUs: Math.max(500, rate),
    },
  },
  thresholds: {
    http_req_failed: [__ENV.MAX_ERROR_RATE ? `rate<${__ENV.MAX_ERROR_RATE}` : "rate<0.001"],
    http_req_duration: [`p(99)<${__ENV.MAX_P99_MS || 250}`],
  },
  summaryTrendStats: ["avg", "med", "p(90)", "p(95)", "p(99)", "max"],
};

let i = 0;
export default function () {
  const target = targets[i++ % targets.length];
  // A spread of users so consistent hashing has keys to spread.
  const res = http.get(`${target}/api/items/${i % 100}`, {
    headers: { "X-User-ID": `user-${(__VU * 7919 + i) % 1000}` },
  });
  check(res, { "status is 200": (r) => r.status === 200 });
}

export function handleSummary(data) {
  const m = data.metrics;
  const summary = {
    label,
    rate_target: rate,
    requests: m.http_reqs.values.count,
    rps: Math.round(m.http_reqs.values.rate),
    error_rate: m.http_req_failed.values.rate,
    p50_ms: m.http_req_duration.values.med,
    p95_ms: m.http_req_duration.values["p(95)"],
    p99_ms: m.http_req_duration.values["p(99)"],
    max_ms: m.http_req_duration.values.max,
  };
  return {
    stdout: JSON.stringify(summary, null, 2) + "\n",
    [`/results/${label}.json`]: JSON.stringify(summary, null, 2),
  };
}
