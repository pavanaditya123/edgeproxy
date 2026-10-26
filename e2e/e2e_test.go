//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRoundRobinSpreadsRequestsEvenly(t *testing.T) {
	a, b, c := startBackend(t, "a"), startBackend(t, "b"), startBackend(t, "c")
	ep := startProxy(t, proxyOpts{}, a, b, c)

	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		counts[ep.whoAnswers(t, nil)]++
	}
	for _, name := range []string{"a", "b", "c"} {
		if counts[name] != 10 {
			t.Errorf("backend %s served %d of 30 requests, want 10 (all counts: %v)", name, counts[name], counts)
		}
	}
	if got := ep.metric(t, "edgeproxy_requests_total"); got != 30 {
		t.Errorf("edgeproxy_requests_total = %v, want 30", got)
	}
}

func TestRequestIsForwardedIntact(t *testing.T) {
	a := startBackend(t, "a")
	ep := startProxy(t, proxyOpts{}, a)

	code, e, err := ep.fetch(http.MethodPost, "/orders/42", map[string]string{"X-Forwarded-For": "6.6.6.6"}, `{"qty":1}`)
	if err != nil || code != http.StatusOK {
		t.Fatalf("POST through proxy: %d, %v", code, err)
	}
	if e.Method != http.MethodPost || e.Path != "/orders/42" {
		t.Errorf("backend saw %s %s, want POST /orders/42", e.Method, e.Path)
	}
	if e.XForwardedFor != "127.0.0.1" {
		t.Errorf("backend saw X-Forwarded-For %q, want the real client IP 127.0.0.1, not the spoofed value", e.XForwardedFor)
	}
}

func TestUnhealthyBackendIsTakenOutAndBroughtBack(t *testing.T) {
	a, b, c := startBackend(t, "a"), startBackend(t, "b"), startBackend(t, "c")
	ep := startProxy(t, proxyOpts{}, a, b, c)

	a.toggleHealth(t)
	waitFor(t, 5*time.Second, "backend a to be marked down", func() bool { return !ep.backendAlive(t, a) })
	for i := 0; i < 20; i++ {
		if who := ep.whoAnswers(t, nil); who == "a" {
			t.Fatalf("request %d went to backend a after it was marked down", i)
		}
	}
	if up := ep.metric(t, "edgeproxy_backend_up"); up != 2 {
		t.Errorf("edgeproxy_backend_up summed to %v, want 2", up)
	}

	a.toggleHealth(t)
	waitFor(t, 5*time.Second, "backend a to be marked up again", func() bool { return ep.backendAlive(t, a) })
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		seen[ep.whoAnswers(t, nil)] = true
	}
	if !seen["a"] {
		t.Errorf("backend a got no traffic after recovering (saw %v)", seen)
	}
}

func TestCrashedBackendIsRetriedForGetButNotPost(t *testing.T) {
	a, b, c := startBackend(t, "a"), startBackend(t, "b"), startBackend(t, "c")
	// A long health-check interval means the checker won't notice the crash
	// during the test, so every request routed to b exercises the retry path.
	ep := startProxy(t, proxyOpts{healthInterval: time.Minute}, a, b, c)
	b.kill()

	for i := 0; i < 12; i++ {
		code, e, err := ep.fetch(http.MethodGet, "/", nil, "")
		if err != nil || code != http.StatusOK {
			t.Fatalf("GET %d: %d, %v; want 200 via a retry on a live backend", i, code, err)
		}
		if e.Backend == "b" {
			t.Fatalf("GET %d was answered by the crashed backend", i)
		}
	}
	if got := ep.metric(t, "edgeproxy_retries_total"); got < 1 {
		t.Errorf("edgeproxy_retries_total = %v, want at least 1", got)
	}

	failed := 0
	for i := 0; i < 6; i++ {
		code, _, err := ep.fetch(http.MethodPost, "/", nil, `{"order":1}`)
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		switch code {
		case http.StatusOK:
		case http.StatusBadGateway:
			failed++
		default:
			t.Fatalf("POST %d: status %d, want 200 or 502", i, code)
		}
	}
	if failed == 0 {
		t.Error("no POST failed: POSTs routed to the crashed backend must return 502, not be replayed elsewhere")
	}
}

func TestAllBackendsDownReturns503(t *testing.T) {
	a, b := startBackend(t, "a"), startBackend(t, "b")
	ep := startProxy(t, proxyOpts{}, a, b)

	a.toggleHealth(t)
	b.toggleHealth(t)
	waitFor(t, 5*time.Second, "/readyz to report no healthy backends", func() bool {
		code, _ := get(ep.adminURL + "/readyz")
		return code == http.StatusServiceUnavailable
	})
	code, _, err := ep.fetch(http.MethodGet, "/", nil, "")
	if err != nil || code != http.StatusServiceUnavailable {
		t.Fatalf("GET with every backend down: %d, %v; want 503", code, err)
	}
}

func TestConsistentHashKeepsEachUserOnOneBackend(t *testing.T) {
	a, b, c := startBackend(t, "a"), startBackend(t, "b"), startBackend(t, "c")
	ep := startProxy(t, proxyOpts{strategy: "consistent_hash", hashKey: "header:X-User-ID"}, a, b, c)

	owners := map[string]bool{}
	for u := 0; u < 30; u++ {
		h := map[string]string{"X-User-ID": fmt.Sprintf("user-%d", u)}
		first := ep.whoAnswers(t, h)
		for i := 0; i < 4; i++ {
			if got := ep.whoAnswers(t, h); got != first {
				t.Fatalf("user-%d moved from backend %s to %s", u, first, got)
			}
		}
		owners[first] = true
	}
	if len(owners) < 2 {
		t.Errorf("all 30 users landed on %v; hashing is not spreading load", owners)
	}
}

func TestLeastConnSendsLessTrafficToSlowBackend(t *testing.T) {
	a := startBackend(t, "a")
	slow := startBackend(t, "slow", "-latency", "300ms")
	c := startBackend(t, "c")
	ep := startProxy(t, proxyOpts{strategy: "least_conn"}, a, slow, c)

	const workers, perWorker = 6, 15
	var mu sync.Mutex
	counts := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				code, e, err := ep.fetch(http.MethodGet, "/", nil, "")
				if err != nil || code != http.StatusOK {
					t.Errorf("GET: %d, %v", code, err)
					return
				}
				mu.Lock()
				counts[e.Backend]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Round robin would give the slow backend a third of the traffic.
	if share := float64(counts["slow"]) / (workers * perWorker); share > 0.2 {
		t.Errorf("slow backend served %.0f%% of requests, want under 20%% (counts: %v)", share*100, counts)
	}
	if counts["a"] == 0 || counts["c"] == 0 {
		t.Errorf("fast backends should share the load, got %v", counts)
	}
}

func TestGracefulShutdownFinishesInflightRequests(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs SIGTERM, which Windows cannot deliver to another process")
	}
	slow := startBackend(t, "slow", "-latency", "1s")
	ep := startProxy(t, proxyOpts{drainDelay: 500 * time.Millisecond}, slow)

	type result struct {
		code int
		err  error
	}
	inflight := make(chan result, 1)
	go func() {
		code, _, err := ep.fetch(http.MethodGet, "/", nil, "")
		inflight <- result{code, err}
	}()
	time.Sleep(200 * time.Millisecond) // let the request reach the backend

	if err := ep.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "/readyz to report draining", func() bool {
		code, _ := get(ep.adminURL + "/readyz")
		return code == http.StatusServiceUnavailable
	})

	if r := <-inflight; r.err != nil || r.code != http.StatusOK {
		t.Fatalf("in-flight request during shutdown: %d, %v; want 200", r.code, r.err)
	}
	select {
	case <-ep.done:
		if ep.err != nil {
			t.Fatalf("edgeproxy exited with %v, want a clean exit", ep.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("edgeproxy did not exit after SIGTERM")
	}
}
