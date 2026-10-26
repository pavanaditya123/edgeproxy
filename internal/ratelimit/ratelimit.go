// Package ratelimit rejects clients that send too many requests, with 429 Too
// Many Requests. Limits are counted in Redis when one is configured, so every
// proxy instance shares them, or in memory otherwise.
package ratelimit

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"edgeproxy/internal/metrics"
	"edgeproxy/internal/reqkey"
)

// Decision is a store's answer for one request.
type Decision struct {
	Allowed   bool
	Limit     int
	Remaining int
	// Reset is how long until the quota is fully available again.
	Reset time.Duration
	// RetryAfter is how long a rejected client should wait before trying again.
	RetryAfter time.Duration
}

// Store decides whether one more request under key fits in limit per window,
// and counts it if so.
type Store interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error)
}

// Rule limits requests whose path starts with PathPrefix (and whose method is
// in Methods, if set) to Limit per Window for each distinct Key.
type Rule struct {
	Name       string
	PathPrefix string
	Methods    []string
	Limit      int
	Window     time.Duration
	Key        reqkey.Func
}

func (r *Rule) matches(req *http.Request) bool {
	if !strings.HasPrefix(req.URL.Path, r.PathPrefix) {
		return false
	}
	return len(r.Methods) == 0 || slices.Contains(r.Methods, req.Method)
}

// OnError says what to do when the shared store (Redis) fails.
type OnError string

const (
	// FallbackLocal enforces the limit per instance from memory: the safe middle ground.
	FallbackLocal OnError = "local"
	// FailOpen lets every request through: availability over protection.
	FailOpen OnError = "allow"
	// FailClosed rejects every rate-limited request: protection over availability.
	FailClosed OnError = "deny"
)

type Options struct {
	// Shared is the fleet-wide store (Redis); nil means count in memory only.
	Shared  Store
	OnError OnError
	// MaxKeys bounds how many clients the in-memory store tracks.
	MaxKeys int
	Metrics *metrics.Metrics
	Logger  *slog.Logger
}

type Limiter struct {
	rules   atomic.Pointer[[]Rule]
	shared  Store
	local   *Local
	onError OnError
	metrics *metrics.Metrics
	logger  *slog.Logger
	lastLog atomic.Int64 // unix nanos of the last "Redis failed" log line
}

func New(opts Options) *Limiter {
	l := &Limiter{
		shared:  opts.Shared,
		local:   NewLocal(opts.MaxKeys),
		onError: opts.OnError,
		metrics: opts.Metrics,
		logger:  opts.Logger,
	}
	l.SetRules(nil)
	return l
}

// SetRules atomically replaces the rules; the first matching rule applies.
func (l *Limiter) SetRules(rules []Rule) { l.rules.Store(&rules) }

// Middleware rejects over-limit requests with 429 and passes the rest to next.
// Every limited response carries RateLimit-Limit, RateLimit-Remaining and
// RateLimit-Reset headers so well-behaved clients can slow down on their own.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rule := l.match(r)
		if rule == nil {
			next.ServeHTTP(w, r)
			return
		}
		d := l.decide(r.Context(), rule, rule.Key(r))
		if d.Limit > 0 {
			h := w.Header()
			h.Set("RateLimit-Limit", strconv.Itoa(d.Limit))
			h.Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
			h.Set("RateLimit-Reset", ceilSeconds(d.Reset))
		}
		if !d.Allowed {
			w.Header().Set("Retry-After", ceilSeconds(d.RetryAfter))
			l.metrics.RateLimited.WithLabelValues(rule.Name).Inc()
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) match(r *http.Request) *Rule {
	rules := *l.rules.Load()
	for i := range rules {
		if rules[i].matches(r) {
			return &rules[i]
		}
	}
	return nil
}

func (l *Limiter) decide(ctx context.Context, rule *Rule, key string) Decision {
	storeKey := rule.Name + ":" + key
	if l.shared == nil {
		d, _ := l.local.Allow(ctx, storeKey, rule.Limit, rule.Window)
		return d
	}
	d, err := l.shared.Allow(ctx, storeKey, rule.Limit, rule.Window)
	if err == nil {
		return d
	}

	l.metrics.RateLimitFallbacks.Inc()
	// Log at most every 10s: during an outage this fires on every request.
	if now := time.Now().UnixNano(); now-l.lastLog.Load() > int64(10*time.Second) {
		l.lastLog.Store(now)
		l.logger.Warn("rate limit store failed; applying on_redis_error policy", "policy", l.onError, "error", err)
	}
	switch l.onError {
	case FailOpen:
		return Decision{Allowed: true}
	case FailClosed:
		return Decision{Allowed: false, Limit: rule.Limit, RetryAfter: time.Second}
	default:
		d, _ := l.local.Allow(ctx, storeKey, rule.Limit, rule.Window)
		return d
	}
}

func ceilSeconds(d time.Duration) string {
	return strconv.Itoa(int(math.Ceil(d.Seconds())))
}
