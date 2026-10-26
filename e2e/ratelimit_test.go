//go:build e2e

package e2e

import (
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// sharedRedis returns a Redis address: E2E_REDIS_ADDR if set (CI runs a real
// Redis service), otherwise an in-process miniredis. The key prefix is unique
// per test so tests sharing one real Redis can't see each other's counters.
func sharedRedis(t *testing.T) (addr, keyPrefix string) {
	t.Helper()
	keyPrefix = "e2e:" + t.Name() + ":" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	if addr := os.Getenv("E2E_REDIS_ADDR"); addr != "" {
		return addr, keyPrefix
	}
	return miniredis.RunT(t).Addr(), keyPrefix
}

func rateLimitConfig(redisAddr, keyPrefix string, rules ...map[string]any) map[string]any {
	cfg := map[string]any{"rules": rules}
	if redisAddr != "" {
		cfg["redis"] = map[string]any{"addr": redisAddr, "key_prefix": keyPrefix, "timeout": "200ms"}
	}
	return cfg
}

func TestRateLimitRejectsOverLimitWith429(t *testing.T) {
	a := startBackend(t, "a")
	ep := startProxy(t, proxyOpts{extra: map[string]any{
		"rate_limit": rateLimitConfig("", "", map[string]any{
			"name": "limited", "path_prefix": "/limited", "limit": 5, "window": "1m",
		}),
	}}, a)

	for i := 0; i < 5; i++ {
		code, h, err := ep.do(http.MethodGet, "/limited", nil)
		if err != nil || code != http.StatusOK {
			t.Fatalf("request %d: %d, %v; want 200", i, code, err)
		}
		if got, want := h.Get("RateLimit-Remaining"), strconv.Itoa(4-i); got != want {
			t.Errorf("request %d: RateLimit-Remaining = %q, want %q", i, got, want)
		}
	}
	code, h, err := ep.do(http.MethodGet, "/limited", nil)
	if err != nil || code != http.StatusTooManyRequests {
		t.Fatalf("6th request: %d, %v; want 429", code, err)
	}
	if ra, _ := strconv.Atoi(h.Get("Retry-After")); ra < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", h.Get("Retry-After"))
	}

	code, h, err = ep.do(http.MethodGet, "/other", nil)
	if err != nil || code != http.StatusOK || h.Get("RateLimit-Limit") != "" {
		t.Errorf("a path outside the rule: %d %v, want 200 with no rate-limit headers", code, err)
	}
	if got := ep.metric(t, "edgeproxy_rate_limited_total"); got != 1 {
		t.Errorf("edgeproxy_rate_limited_total = %v, want 1", got)
	}
}

func TestRateLimitPerAPIKey(t *testing.T) {
	a := startBackend(t, "a")
	ep := startProxy(t, proxyOpts{extra: map[string]any{
		"rate_limit": rateLimitConfig("", "", map[string]any{
			"name": "api", "path_prefix": "/", "limit": 2, "window": "1m", "key": "header:X-API-Key",
		}),
	}}, a)

	for i := 0; i < 2; i++ {
		ep.do(http.MethodGet, "/", map[string]string{"X-API-Key": "alice"})
	}
	if code, _, _ := ep.do(http.MethodGet, "/", map[string]string{"X-API-Key": "alice"}); code != http.StatusTooManyRequests {
		t.Errorf("alice's 3rd request: %d, want 429", code)
	}
	if code, _, _ := ep.do(http.MethodGet, "/", map[string]string{"X-API-Key": "bob"}); code != http.StatusOK {
		t.Errorf("bob has his own quota, got %d", code)
	}
}

func TestRateLimitIsSharedAcrossProxiesThroughRedis(t *testing.T) {
	addr, prefix := sharedRedis(t)
	a := startBackend(t, "a")
	opts := proxyOpts{extra: map[string]any{
		"rate_limit": rateLimitConfig(addr, prefix, map[string]any{
			"name": "api", "path_prefix": "/", "limit": 6, "window": "1m",
		}),
	}}
	p1, p2 := startProxy(t, opts, a), startProxy(t, opts, a)

	allowed := 0
	for i := 0; i < 12; i++ {
		ep := []*edgeProxy{p1, p2}[i%2]
		code, _, err := ep.do(http.MethodGet, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if code == http.StatusOK {
			allowed++
		}
	}
	// Each proxy alone would allow 6, i.e. 12 in total.
	if allowed != 6 {
		t.Fatalf("two proxies together allowed %d of 12 requests, want the shared limit of 6", allowed)
	}
}

func TestRateLimitFallsBackToLocalWhenRedisIsDown(t *testing.T) {
	mr := miniredis.RunT(t) // always miniredis: this test has to stop it
	a := startBackend(t, "a")
	ep := startProxy(t, proxyOpts{extra: map[string]any{
		"rate_limit": rateLimitConfig(mr.Addr(), "e2e:", map[string]any{
			"name": "api", "path_prefix": "/", "limit": 3, "window": "1m",
		}),
	}}, a)
	mr.Close()

	want := []int{200, 200, 200, 429}
	for i, w := range want {
		code, _, err := ep.do(http.MethodGet, "/", nil)
		if err != nil || code != w {
			t.Fatalf("request %d with Redis down: %d, %v; want %d (traffic keeps flowing, limited per instance)", i, code, err, w)
		}
	}
	if got := ep.metric(t, "edgeproxy_rate_limit_fallbacks_total"); got < 4 {
		t.Errorf("edgeproxy_rate_limit_fallbacks_total = %v, want at least 4", got)
	}
}
