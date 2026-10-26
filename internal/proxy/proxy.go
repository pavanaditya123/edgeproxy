// Package proxy is the HTTP handler that forwards client requests to backends.
package proxy

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"edgeproxy/internal/backend"
	"edgeproxy/internal/balancer"
	"edgeproxy/internal/metrics"
	"edgeproxy/internal/outlier"
)

type Config struct {
	Transport http.RoundTripper
	// MaxRetries is how many other backends a failed retryable request may be tried on.
	MaxRetries int
	// RetryBudget caps retries across all requests; nil means no cap.
	RetryBudget *RetryBudget
	Metrics     *metrics.Metrics
	Logger      *slog.Logger
}

type Proxy struct {
	pool       atomic.Pointer[pool]
	transport  http.RoundTripper
	errorLog   *log.Logger
	maxRetries int
	budget     *RetryBudget
	metrics    *metrics.Metrics
	logger     *slog.Logger
}

// pool is one immutable generation of backends and the balancer over them.
// A config reload swaps in a new pool atomically; requests already in
// progress finish on the pool they started with.
type pool struct {
	backends   []*backend.Backend
	balancer   balancer.Balancer
	outlier    *outlier.Detector
	forwarders map[*backend.Backend]*httputil.ReverseProxy
}

// New returns a proxy with no backends; call SetPool before serving traffic.
func New(cfg Config) *Proxy {
	p := &Proxy{
		transport:  cfg.Transport,
		errorLog:   slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelWarn),
		maxRetries: cfg.MaxRetries,
		budget:     cfg.RetryBudget,
		metrics:    cfg.Metrics,
		logger:     cfg.Logger,
	}
	p.pool.Store(&pool{})
	return p
}

// SetPool atomically replaces the backends, balancer and outlier detector.
func (p *Proxy) SetPool(backends []*backend.Backend, bal balancer.Balancer, det *outlier.Detector) {
	forwarders := make(map[*backend.Backend]*httputil.ReverseProxy, len(backends))
	for _, b := range backends {
		forwarders[b] = newForwarder(b, p.transport, p.errorLog)
	}
	p.pool.Store(&pool{backends: backends, balancer: bal, outlier: det, forwarders: forwarders})
}

// Backends returns the backends currently in rotation.
func (p *Proxy) Backends() []*backend.Backend { return p.pool.Load().backends }

// TransportConfig holds the timeouts for connections from the proxy to backends.
type TransportConfig struct {
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
}

// NewTransport returns a connection pool tuned for a proxy rather than a
// typical HTTP client.
func NewTransport(cfg TransportConfig) *http.Transport {
	return &http.Transport{
		// Never route backend traffic through an HTTP_PROXY set in the environment.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   cfg.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      1000,
		// The default of 2 forces a fresh TCP (and TLS) handshake for most
		// requests under load; a proxy wants to keep many warm connections per backend.
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
}

type attemptErrKey struct{}

func newForwarder(b *backend.Backend, transport http.RoundTripper, errorLog *log.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(b.URL)
			// Rewrite strips any client-sent X-Forwarded-* headers before this
			// runs, so backends only ever see the real client IP.
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorLog:  errorLog,
		// Report transport errors back to ServeHTTP instead of writing a 502
		// here, so the request can be retried on another backend.
		ErrorHandler: func(_ http.ResponseWriter, r *http.Request, err error) {
			if slot, ok := r.Context().Value(attemptErrKey{}).(*error); ok {
				*slot = err
			}
		},
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	pl := p.pool.Load()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	p.budget.RecordRequest()

	attempts := 1
	if isRetryable(r) {
		attempts += p.maxRetries
	}
	var tried []*backend.Backend
	eligible := func(b *backend.Backend) bool {
		return b.Alive() && !b.Outlier.Ejected(time.Now()) && b.HasCapacity() && !slices.Contains(tried, b)
	}

	for pl.balancer != nil && len(tried) < attempts {
		b := pl.balancer.Next(r, eligible)
		if b == nil {
			break
		}
		if len(tried) > 0 {
			if !p.budget.TryRetry() {
				p.metrics.RetryBudgetExhausted.Inc()
				break
			}
			p.metrics.Retries.Inc()
		}
		tried = append(tried, b)

		err := p.forward(rec, r, pl, b)
		if err != nil && r.Context().Err() != nil {
			// The client went away; there is nobody left to answer, and it's
			// not the backend's fault. 499 is nginx's "client closed request".
			p.observe(b.Name(), 499, start)
			return
		}
		p.recordOutcome(pl, b, err != nil || rec.status >= 500)
		if err == nil {
			p.observe(b.Name(), rec.status, start)
			return
		}
		p.metrics.UpstreamErrors.WithLabelValues(b.Name()).Inc()
		p.logger.Warn("upstream request failed",
			"backend", b.Name(), "attempt", len(tried), "method", r.Method, "path", r.URL.Path, "error", err)
	}

	if len(tried) == 0 {
		http.Error(rec, "no healthy backend available", http.StatusServiceUnavailable)
		p.observe("none", http.StatusServiceUnavailable, start)
		return
	}
	http.Error(rec, "bad gateway", http.StatusBadGateway)
	p.observe(tried[len(tried)-1].Name(), http.StatusBadGateway, start)
}

// forward proxies r to b and returns the transport error, if any. A nil error
// means a response (of any status) was written to w.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, pl *pool, b *backend.Backend) error {
	b.Acquire()
	defer b.Release()

	var err error
	ctx := context.WithValue(r.Context(), attemptErrKey{}, &err)
	pl.forwarders[b].ServeHTTP(w, r.WithContext(ctx))
	return err
}

// recordOutcome feeds passive health checking: a transport error or a 5xx
// from the backend counts as a failure.
func (p *Proxy) recordOutcome(pl *pool, b *backend.Backend, failed bool) {
	if d := pl.outlier.Report(&b.Outlier, failed, time.Now()); d > 0 {
		p.metrics.OutlierEjections.WithLabelValues(b.Name()).Inc()
		p.logger.Warn("backend ejected by outlier detection", "backend", b.Name(), "duration", d.String())
	}
}

func (p *Proxy) observe(backendName string, status int, start time.Time) {
	p.metrics.Requests.WithLabelValues(backendName, strconv.Itoa(status)).Inc()
	p.metrics.Duration.WithLabelValues(backendName).Observe(time.Since(start).Seconds())
}

// isRetryable reports whether a failed request can safely be replayed on another
// backend: the method must be idempotent and there must be no body, since the
// first attempt may already have consumed it.
func isRetryable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.Body == nil || r.Body == http.NoBody
	}
	return false
}

// statusRecorder captures the status code written by the reverse proxy.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	// 1xx informational responses (other than a protocol switch) precede the real status.
	if !s.wroteHeader && (code >= 200 || code == http.StatusSwitchingProtocols) {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer, which the
// reverse proxy needs for flushing streamed responses and WebSocket upgrades.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
