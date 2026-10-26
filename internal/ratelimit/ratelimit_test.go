package ratelimit

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"edgeproxy/internal/metrics"
	"edgeproxy/internal/reqkey"
)

var ctx = context.Background()

func TestLocalAllowsLimitThenRefills(t *testing.T) {
	l := NewLocal(100)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		d, _ := l.Allow(ctx, "k", 5, 10*time.Second)
		if !d.Allowed || d.Remaining != 4-i {
			t.Fatalf("request %d: %+v, want allowed with %d remaining", i, d, 4-i)
		}
	}
	d, _ := l.Allow(ctx, "k", 5, 10*time.Second)
	if d.Allowed {
		t.Fatal("6th request in the window should be rejected")
	}
	if d.RetryAfter != 2*time.Second {
		t.Errorf("RetryAfter = %v, want 2s (one token every 10s/5)", d.RetryAfter)
	}

	now = now.Add(2 * time.Second) // exactly one token refilled
	if d, _ := l.Allow(ctx, "k", 5, 10*time.Second); !d.Allowed {
		t.Fatal("a token should have refilled after 2s")
	}
	if d, _ := l.Allow(ctx, "k", 5, 10*time.Second); d.Allowed {
		t.Fatal("only one token should have refilled")
	}
}

func TestLocalKeysAreIndependent(t *testing.T) {
	l := NewLocal(100)
	for i := 0; i < 3; i++ {
		l.Allow(ctx, "alice", 3, time.Minute)
	}
	if d, _ := l.Allow(ctx, "alice", 3, time.Minute); d.Allowed {
		t.Fatal("alice should be limited")
	}
	if d, _ := l.Allow(ctx, "bob", 3, time.Minute); !d.Allowed {
		t.Fatal("bob has his own bucket")
	}
}

func TestLocalMemoryIsBounded(t *testing.T) {
	l := NewLocal(160) // 10 per shard
	for i := 0; i < 10_000; i++ {
		l.Allow(ctx, "client-"+strconv.Itoa(i), 5, time.Minute)
	}
	if n := l.size(); n > 160 {
		t.Fatalf("tracking %d keys, want at most 160", n)
	}
}

func newRedisStore(t *testing.T) (*Redis, *miniredis.Miniredis, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { client.Close() })
	s := NewRedis(client, "test:", time.Second)
	now := time.UnixMilli(1_000_000_000) // the start of a 10s window
	s.now = func() time.Time { return now }
	return s, mr, &now
}

func TestRedisAllowsLimitPerWindow(t *testing.T) {
	s, _, _ := newRedisStore(t)
	for i := 0; i < 4; i++ {
		d, err := s.Allow(ctx, "k", 4, 10*time.Second)
		if err != nil || !d.Allowed || d.Remaining != 3-i {
			t.Fatalf("request %d: %+v, %v; want allowed with %d remaining", i, d, err, 3-i)
		}
	}
	d, err := s.Allow(ctx, "k", 4, 10*time.Second)
	if err != nil || d.Allowed {
		t.Fatalf("5th request: %+v, %v; want rejected", d, err)
	}
	if d.RetryAfter != 10*time.Second {
		t.Errorf("RetryAfter = %v, want the 10s until the next window", d.RetryAfter)
	}
}

func TestRedisSlidingWindowWeighsThePreviousWindow(t *testing.T) {
	s, _, now := newRedisStore(t)
	for i := 0; i < 10; i++ {
		s.Allow(ctx, "k", 10, 10*time.Second)
	}
	// 7.5s into the next window, a quarter of the full previous window still
	// overlaps: 10 * 0.25 = 2.5 requests count against the limit, leaving 7.
	*now = now.Add(17500 * time.Millisecond)
	allowed := 0
	for i := 0; i < 20; i++ {
		if d, _ := s.Allow(ctx, "k", 10, 10*time.Second); d.Allowed {
			allowed++
		}
	}
	if allowed != 7 {
		t.Fatalf("allowed %d requests, want 7", allowed)
	}
}

func TestRedisIsSharedAcrossInstances(t *testing.T) {
	a, mr, _ := newRedisStore(t)
	other := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer other.Close()
	b := NewRedis(other, "test:", time.Second)
	b.now = a.now

	allowed := 0
	for i := 0; i < 10; i++ {
		store := []*Redis{a, b}[i%2]
		if d, _ := store.Allow(ctx, "k", 6, time.Minute); d.Allowed {
			allowed++
		}
	}
	if allowed != 6 {
		t.Fatalf("two instances together allowed %d requests, want the shared limit of 6", allowed)
	}
}

func newLimiter(t *testing.T, shared Store, onError OnError, rules ...Rule) (*Limiter, *metrics.Metrics, http.Handler) {
	t.Helper()
	m := metrics.New()
	l := New(Options{Shared: shared, OnError: onError, MaxKeys: 1000, Metrics: m, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	l.SetRules(rules)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return l, m, l.Middleware(ok)
}

func send(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestMiddlewareRejectsWith429AndHeaders(t *testing.T) {
	_, m, h := newLimiter(t, nil, FallbackLocal,
		Rule{Name: "login", PathPrefix: "/login", Methods: []string{http.MethodPost}, Limit: 2, Window: time.Minute, Key: reqkey.ClientIP})

	for i := 0; i < 2; i++ {
		rec := send(h, http.MethodPost, "/login")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i, rec.Code)
		}
		if got, want := rec.Header().Get("RateLimit-Remaining"), strconv.Itoa(1-i); got != want {
			t.Errorf("request %d: RateLimit-Remaining = %q, want %q", i, got, want)
		}
	}
	rec := send(h, http.MethodPost, "/login")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "30" || rec.Header().Get("RateLimit-Limit") != "2" {
		t.Errorf("429 headers: Retry-After=%q RateLimit-Limit=%q, want 30 and 2",
			rec.Header().Get("Retry-After"), rec.Header().Get("RateLimit-Limit"))
	}
	if got := testutil.ToFloat64(m.RateLimited.WithLabelValues("login")); got != 1 {
		t.Errorf("rate limited counter = %v, want 1", got)
	}

	// The rule only covers POST /login.
	if rec := send(h, http.MethodGet, "/login"); rec.Code != http.StatusOK || rec.Header().Get("RateLimit-Limit") != "" {
		t.Errorf("GET /login should not be limited: %d %v", rec.Code, rec.Header())
	}
	if rec := send(h, http.MethodPost, "/orders"); rec.Code != http.StatusOK {
		t.Errorf("POST /orders should not be limited: %d", rec.Code)
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	_, _, h := newLimiter(t, nil, FallbackLocal,
		Rule{Name: "search", PathPrefix: "/api/search", Limit: 1, Window: time.Minute, Key: reqkey.ClientIP},
		Rule{Name: "api", PathPrefix: "/api", Limit: 100, Window: time.Minute, Key: reqkey.ClientIP})
	send(h, http.MethodGet, "/api/search")
	if rec := send(h, http.MethodGet, "/api/search"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("/api/search should hit its own limit of 1, got %d", rec.Code)
	}
	if rec := send(h, http.MethodGet, "/api/users"); rec.Code != http.StatusOK {
		t.Errorf("/api/users falls under the broader rule, got %d", rec.Code)
	}
}

func TestRedisFailureFallsBackPerPolicy(t *testing.T) {
	rule := Rule{Name: "api", PathPrefix: "/", Limit: 2, Window: time.Minute, Key: reqkey.ClientIP}
	for _, tc := range []struct {
		policy OnError
		want   []int
	}{
		{FallbackLocal, []int{200, 200, 429}},
		{FailOpen, []int{200, 200, 200}},
		{FailClosed, []int{429, 429, 429}},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			s, mr, _ := newRedisStore(t)
			mr.Close() // Redis is down
			_, m, h := newLimiter(t, s, tc.policy, rule)
			for i, want := range tc.want {
				if rec := send(h, http.MethodGet, "/"); rec.Code != want {
					t.Errorf("request %d: %d, want %d", i, rec.Code, want)
				}
			}
			if got := testutil.ToFloat64(m.RateLimitFallbacks); got != 3 {
				t.Errorf("fallbacks = %v, want 3", got)
			}
		})
	}
}

func TestSetRulesTakesEffectImmediately(t *testing.T) {
	l, _, h := newLimiter(t, nil, FallbackLocal)
	if rec := send(h, http.MethodGet, "/"); rec.Code != http.StatusOK {
		t.Fatal("no rules means no limiting")
	}
	l.SetRules([]Rule{{Name: "all", PathPrefix: "/", Limit: 1, Window: time.Minute, Key: reqkey.ClientIP}})
	send(h, http.MethodGet, "/")
	if rec := send(h, http.MethodGet, "/"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d after adding a limit of 1, want 429", rec.Code)
	}
}
