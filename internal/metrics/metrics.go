// Package metrics defines edgeproxy's Prometheus metrics.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"edgeproxy/internal/backend"
)

type Metrics struct {
	Registry *prometheus.Registry

	// Requests counts finished client requests by the backend that answered and status code.
	Requests *prometheus.CounterVec
	// Duration is end-to-end client request latency, including retries.
	Duration *prometheus.HistogramVec
	// UpstreamErrors counts transport-level failures (refused, reset, timeout) per backend.
	UpstreamErrors *prometheus.CounterVec
	// Retries counts attempts made on a second or later backend.
	Retries prometheus.Counter
	// RetryBudgetExhausted counts retries skipped because the retry budget was empty.
	RetryBudgetExhausted prometheus.Counter
	// OutlierEjections counts backends ejected by passive health checking.
	OutlierEjections *prometheus.CounterVec
	// RateLimited counts requests rejected with 429, by rule.
	RateLimited *prometheus.CounterVec
	// RateLimitFallbacks counts rate-limit checks answered locally because Redis failed.
	RateLimitFallbacks prometheus.Counter
	// ConfigReloads counts config reload attempts by result ("success" or "failure").
	ConfigReloads *prometheus.CounterVec
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		Registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "edgeproxy_requests_total",
			Help: "Client requests handled, by backend and HTTP status code.",
		}, []string{"backend", "code"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "edgeproxy_request_duration_seconds",
			Help:    "End-to-end client request latency, including retries.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"backend"}),
		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "edgeproxy_upstream_errors_total",
			Help: "Transport-level failures talking to a backend.",
		}, []string{"backend"}),
		Retries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "edgeproxy_retries_total",
			Help: "Request attempts retried on another backend.",
		}),
		RetryBudgetExhausted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "edgeproxy_retry_budget_exhausted_total",
			Help: "Retries skipped because the retry budget was empty.",
		}),
		OutlierEjections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "edgeproxy_outlier_ejections_total",
			Help: "Backends ejected by passive health checking (outlier detection).",
		}, []string{"backend"}),
		RateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "edgeproxy_rate_limited_total",
			Help: "Requests rejected with 429 Too Many Requests, by rate-limit rule.",
		}, []string{"rule"}),
		RateLimitFallbacks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "edgeproxy_rate_limit_fallbacks_total",
			Help: "Rate-limit checks where Redis failed and the on_redis_error policy was applied.",
		}),
		ConfigReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "edgeproxy_config_reloads_total",
			Help: "Config reload attempts, by result.",
		}, []string{"result"}),
	}
	reg.MustRegister(m.Requests, m.Duration, m.UpstreamErrors, m.Retries, m.RetryBudgetExhausted,
		m.OutlierEjections, m.RateLimited, m.RateLimitFallbacks, m.ConfigReloads)
	return m
}

// WatchBackends exports per-backend gauges for whatever backends source
// returns at scrape time, so the gauges follow config reloads.
func (m *Metrics) WatchBackends(source func() []*backend.Backend) {
	m.Registry.MustRegister(&backendCollector{
		source: source,
		up: prometheus.NewDesc("edgeproxy_backend_up",
			"1 if the backend is passing active health checks, else 0.", []string{"backend"}, nil),
		inflight: prometheus.NewDesc("edgeproxy_backend_inflight_requests",
			"Requests currently being proxied to the backend.", []string{"backend"}, nil),
		ejected: prometheus.NewDesc("edgeproxy_backend_ejected",
			"1 if outlier detection has ejected the backend, else 0.", []string{"backend"}, nil),
	})
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

type backendCollector struct {
	source                func() []*backend.Backend
	up, inflight, ejected *prometheus.Desc
}

func (c *backendCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.inflight
	ch <- c.ejected
}

func (c *backendCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	for _, b := range c.source() {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, boolValue(b.Alive()), b.Name())
		ch <- prometheus.MustNewConstMetric(c.inflight, prometheus.GaugeValue, float64(b.Inflight()), b.Name())
		ch <- prometheus.MustNewConstMetric(c.ejected, prometheus.GaugeValue, boolValue(b.Outlier.Ejected(now)), b.Name())
	}
}

func boolValue(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
