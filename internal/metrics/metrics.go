// Package metrics defines edgeproxy's Prometheus metrics.
package metrics

import (
	"net/http"

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
}

// New registers all metrics, including per-backend health and load gauges that
// are read straight from the backends at scrape time.
func New(backends []*backend.Backend) *Metrics {
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
	}
	reg.MustRegister(m.Requests, m.Duration, m.UpstreamErrors, m.Retries)

	for _, b := range backends {
		labels := prometheus.Labels{"backend": b.Name()}
		reg.MustRegister(
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name:        "edgeproxy_backend_up",
				Help:        "1 if the backend is passing health checks, else 0.",
				ConstLabels: labels,
			}, func() float64 {
				if b.Alive() {
					return 1
				}
				return 0
			}),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name:        "edgeproxy_backend_inflight_requests",
				Help:        "Requests currently being proxied to the backend.",
				ConstLabels: labels,
			}, func() float64 { return float64(b.Inflight()) }),
		)
	}
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
