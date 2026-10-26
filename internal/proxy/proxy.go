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
	"time"

	"edgeproxy/internal/backend"
	"edgeproxy/internal/balancer"
	"edgeproxy/internal/metrics"
)

type Config struct {
	Backends  []*backend.Backend
	Balancer  balancer.Balancer
	Transport http.RoundTripper
	// MaxRetries is how many other backends a failed retryable request may be tried on.
	MaxRetries int
	Metrics    *metrics.Metrics
	Logger     *slog.Logger
}

type Proxy struct {
	balancer   balancer.Balancer
	maxRetries int
	forwarders map[*backend.Backend]*httputil.ReverseProxy
	metrics    *metrics.Metrics
	logger     *slog.Logger
}

func New(cfg Config) *Proxy {
	errorLog := slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelWarn)
	forwarders := make(map[*backend.Backend]*httputil.ReverseProxy, len(cfg.Backends))
	for _, b := range cfg.Backends {
		forwarders[b] = newForwarder(b, cfg.Transport, errorLog)
	}
	return &Proxy{
		balancer:   cfg.Balancer,
		maxRetries: cfg.MaxRetries,
		forwarders: forwarders,
		metrics:    cfg.Metrics,
		logger:     cfg.Logger,
	}
}

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
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	attempts := 1
	if isRetryable(r) {
		attempts += p.maxRetries
	}
	var tried []*backend.Backend
	eligible := func(b *backend.Backend) bool { return b.Alive() && !slices.Contains(tried, b) }

	for len(tried) < attempts {
		b := p.balancer.Next(r, eligible)
		if b == nil {
			break
		}
		if len(tried) > 0 {
			p.metrics.Retries.Inc()
		}
		tried = append(tried, b)

		err := p.forward(rec, r, b)
		if err == nil {
			p.observe(b.Name(), rec.status, start)
			return
		}
		p.metrics.UpstreamErrors.WithLabelValues(b.Name()).Inc()
		if r.Context().Err() != nil {
			// The client went away; there is nobody left to answer. 499 is
			// nginx's convention for "client closed request".
			p.observe(b.Name(), 499, start)
			return
		}
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
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, b *backend.Backend) error {
	b.Acquire()
	defer b.Release()

	var err error
	ctx := context.WithValue(r.Context(), attemptErrKey{}, &err)
	p.forwarders[b].ServeHTTP(w, r.WithContext(ctx))
	return err
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
