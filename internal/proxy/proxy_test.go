package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"edgeproxy/internal/backend"
	"edgeproxy/internal/balancer"
	"edgeproxy/internal/metrics"
	"edgeproxy/internal/outlier"
)

// liveServer answers every request with its name.
func liveServer(t *testing.T, name string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// deadServer returns the URL of a server that has already shut down, so
// connections to it are refused.
func deadServer() string {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

// newTestProxy fronts the given backend URLs with a round-robin proxy. Every
// backend starts marked alive, as if the health checker hasn't noticed a failure yet.
func newTestProxy(t *testing.T, urls ...string) (*httptest.Server, []*backend.Backend, *metrics.Metrics) {
	t.Helper()
	srv, bs, m, _ := newTestProxyWith(t, nil, urls...)
	return srv, bs, m
}

// newTestProxyWith is newTestProxy with an outlier policy (nil disables
// outlier detection), also returning the Proxy itself.
func newTestProxyWith(t *testing.T, policy *outlier.Policy, urls ...string) (*httptest.Server, []*backend.Backend, *metrics.Metrics, *Proxy) {
	t.Helper()
	bs := make([]*backend.Backend, len(urls))
	states := make([]*outlier.State, len(urls))
	for i, u := range urls {
		b, err := backend.New(u)
		if err != nil {
			t.Fatal(err)
		}
		b.SetAlive(true)
		bs[i] = b
		states[i] = &b.Outlier
	}
	m := metrics.New()
	p := New(Config{
		Transport: NewTransport(TransportConfig{
			DialTimeout: time.Second, ResponseHeaderTimeout: 2 * time.Second, IdleConnTimeout: time.Minute,
		}),
		Metrics: m,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var det *outlier.Detector
	if policy != nil {
		det = outlier.NewDetector(*policy, states)
	}
	p.SetPool(PoolConfig{Backends: bs, Balancer: balancer.NewRoundRobin(bs), Outlier: det, MaxRetries: 2})
	m.WatchBackends(p.Backends)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, bs, m, p
}

// statusServer answers every request with the given status.
func statusServer(t *testing.T, code int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, req)
}

func TestProxyBalancesAcrossBackends(t *testing.T) {
	srv, _, _ := newTestProxy(t, liveServer(t, "a"), liveServer(t, "b"))
	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		code, body := get(t, srv.URL)
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		counts[body]++
	}
	if counts["a"] != 5 || counts["b"] != 5 {
		t.Errorf("got %v, want 5 requests on each backend", counts)
	}
}

func TestProxyRetriesGetOnAnotherBackend(t *testing.T) {
	srv, bs, m := newTestProxy(t, deadServer(), liveServer(t, "live"))
	for i := 0; i < 6; i++ {
		code, body := get(t, srv.URL)
		if code != http.StatusOK || body != "live" {
			t.Fatalf("request %d: got %d %q, want 200 from the live backend", i, code, body)
		}
	}
	if got := testutil.ToFloat64(m.Retries); got == 0 {
		t.Error("expected retries to be counted")
	}
	if got := testutil.ToFloat64(m.UpstreamErrors.WithLabelValues(bs[0].Name())); got == 0 {
		t.Error("expected upstream errors on the dead backend")
	}
}

func TestProxyDoesNotRetryPost(t *testing.T) {
	// Round robin starts at the first backend, so this POST hits the dead one.
	srv, _, _ := newTestProxy(t, deadServer(), liveServer(t, "live"))
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"order":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, req); code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: a POST must not be replayed", code)
	}
}

func TestProxyReturns502WhenAllBackendsFail(t *testing.T) {
	srv, _, _ := newTestProxy(t, deadServer(), deadServer())
	if code, _ := get(t, srv.URL); code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", code)
	}
}

func TestProxyReturns503WithNoHealthyBackends(t *testing.T) {
	srv, bs, m := newTestProxy(t, liveServer(t, "a"))
	bs[0].SetAlive(false)
	if code, _ := get(t, srv.URL); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	if got := testutil.ToFloat64(m.Requests.WithLabelValues("none", "503")); got != 1 {
		t.Errorf("requests{backend=none,code=503} = %v, want 1", got)
	}
}

func TestProxyReplacesSpoofedForwardedFor(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("X-Forwarded-For"))
	}))
	t.Cleanup(echo.Close)

	srv, _, _ := newTestProxy(t, echo.URL)
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	if _, body := do(t, req); body != "127.0.0.1" {
		t.Errorf("backend saw X-Forwarded-For %q, want only the real client IP 127.0.0.1", body)
	}
}

func TestProxyEjectsBackendThatKeepsReturning5xx(t *testing.T) {
	policy := &outlier.Policy{ConsecutiveFailures: 3, BaseEjection: time.Minute, MaxEjection: time.Hour, MaxEjectionPercent: 50}
	srv, bs, m, _ := newTestProxyWith(t, policy, statusServer(t, http.StatusInternalServerError), liveServer(t, "good"))

	// Round robin alternates, so the first 6 requests give the bad backend its 3 failures.
	for i := 0; i < 6; i++ {
		get(t, srv.URL)
	}
	if !bs[0].Outlier.Ejected(time.Now()) {
		t.Fatal("backend returning 500s should be ejected after 3 in a row")
	}
	for i := 0; i < 10; i++ {
		if code, body := get(t, srv.URL); code != http.StatusOK || body != "good" {
			t.Fatalf("after ejection got %d %q, want every request on the good backend", code, body)
		}
	}
	if got := testutil.ToFloat64(m.OutlierEjections.WithLabelValues(bs[0].Name())); got != 1 {
		t.Errorf("outlier ejections = %v, want 1", got)
	}
}

func TestProxyStopsRetryingWhenBudgetIsEmpty(t *testing.T) {
	srv, _, m, p := newTestProxyWith(t, nil, deadServer(), liveServer(t, "live"))
	p.budget = NewRetryBudget(0, 0) // no retries at all

	// Round robin starts on the dead backend; with no budget there's no retry.
	if code, _ := get(t, srv.URL); code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 because the retry was refused", code)
	}
	if got := testutil.ToFloat64(m.RetryBudgetExhausted); got != 1 {
		t.Errorf("retry budget exhausted = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.Retries); got != 0 {
		t.Errorf("retries = %v, want 0", got)
	}
}

func TestProxySkipsBackendAtCapacity(t *testing.T) {
	srv, bs, _ := newTestProxy(t, liveServer(t, "a"), liveServer(t, "b"))
	bs[0].SetMaxInflight(1)
	bs[0].Acquire() // simulate one request already in flight on a
	defer bs[0].Release()
	for i := 0; i < 6; i++ {
		if _, body := get(t, srv.URL); body != "b" {
			t.Fatalf("request %d went to %q, want b while a is at capacity", i, body)
		}
	}
}

func TestSetPoolSwapsBackendsForNewRequests(t *testing.T) {
	srv, _, _, p := newTestProxyWith(t, nil, liveServer(t, "old"))
	if _, body := get(t, srv.URL); body != "old" {
		t.Fatalf("got %q before the swap, want old", body)
	}
	b, err := backend.New(liveServer(t, "new"))
	if err != nil {
		t.Fatal(err)
	}
	b.SetAlive(true)
	bs := []*backend.Backend{b}
	p.SetPool(PoolConfig{Backends: bs, Balancer: balancer.NewRoundRobin(bs)})
	for i := 0; i < 5; i++ {
		if _, body := get(t, srv.URL); body != "new" {
			t.Fatalf("got %q after the swap, want new", body)
		}
	}
}

func TestProxyWithEmptyPoolReturns503(t *testing.T) {
	p := New(Config{Transport: http.DefaultTransport, Metrics: metrics.New(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(p)
	defer srv.Close()
	if code, _ := get(t, srv.URL); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
}

func TestProxyPassesThroughBackendStatus(t *testing.T) {
	teapot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(teapot.Close)

	srv, bs, m := newTestProxy(t, teapot.URL)
	if code, _ := get(t, srv.URL); code != http.StatusTeapot {
		t.Fatalf("status %d, want 418", code)
	}
	if got := testutil.ToFloat64(m.Requests.WithLabelValues(bs[0].Name(), "418")); got != 1 {
		t.Errorf("requests{code=418} = %v, want 1", got)
	}
}
