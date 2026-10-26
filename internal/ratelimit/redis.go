package ratelimit

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindow is a sliding-window counter (the approach Cloudflare
// describes). Time is cut into fixed windows, each with its own counter; the
// count "in the last window" is estimated as the current window's count plus
// the previous window's count weighted by how much of it still overlaps.
// Running it as one Lua script makes read-check-increment atomic, so two
// proxies racing on the same key can't both squeeze in the last request.
//
// KEYS[1] current window counter, KEYS[2] previous window counter
// ARGV[1] limit, ARGV[2] window length (ms), ARGV[3] ms elapsed in the current window
// Returns {allowed (1/0), remaining}.
var slidingWindow = redis.NewScript(`
local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local elapsed = tonumber(ARGV[3])
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local previous = tonumber(redis.call('GET', KEYS[2]) or '0')
local used = previous * (window - elapsed) / window + current
if used + 1 > limit then
  return {0, math.max(0, math.floor(limit - used))}
end
current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('PEXPIRE', KEYS[1], window * 2)
end
return {1, math.max(0, math.floor(limit - used - 1))}
`)

// Redis is a Store shared by every proxy instance pointing at the same Redis,
// so a limit applies across the whole fleet instead of per instance.
type Redis struct {
	client  redis.Scripter
	prefix  string
	timeout time.Duration
	now     func() time.Time
}

// NewRedis returns a store that gives up on Redis after timeout, so a slow
// Redis adds at most that much latency before the limiter falls back.
func NewRedis(client redis.Scripter, prefix string, timeout time.Duration) *Redis {
	return &Redis{client: client, prefix: prefix, timeout: timeout, now: time.Now}
}

func (s *Redis) Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	windowMs := window.Milliseconds()
	nowMs := s.now().UnixMilli()
	index, elapsed := nowMs/windowMs, nowMs%windowMs
	// The {braces} are a Redis Cluster hash tag: both windows of a key land on
	// the same shard, which a multi-key script requires.
	base := s.prefix + "{" + key + "}:"
	keys := []string{base + strconv.FormatInt(index, 10), base + strconv.FormatInt(index-1, 10)}

	res, err := slidingWindow.Run(ctx, s.client, keys, limit, windowMs, elapsed).Int64Slice()
	if err != nil {
		return Decision{}, err
	}
	untilNextWindow := time.Duration(windowMs-elapsed) * time.Millisecond
	d := Decision{Allowed: res[0] == 1, Limit: limit, Remaining: int(res[1]), Reset: untilNextWindow}
	if !d.Allowed {
		d.RetryAfter = untilNextWindow
	}
	return d, nil
}
