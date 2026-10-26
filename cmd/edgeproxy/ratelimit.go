package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"edgeproxy/internal/config"
	"edgeproxy/internal/metrics"
	"edgeproxy/internal/ratelimit"
	"edgeproxy/internal/reqkey"
)

// newLimiter builds the rate limiter, backed by Redis when one is configured.
// The returned function closes the Redis connection pool.
func newLimiter(cfg *config.Config, m *metrics.Metrics, logger *slog.Logger) (*ratelimit.Limiter, func() error) {
	rl := cfg.RateLimit
	var shared ratelimit.Store
	closeFn := func() error { return nil }
	if rl.Redis.Addr != "" {
		timeout := rl.Redis.Timeout.Std()
		client := redis.NewClient(&redis.Options{
			Addr:         rl.Redis.Addr,
			Password:     os.Getenv(rl.Redis.PasswordEnv),
			DB:           rl.Redis.DB,
			DialTimeout:  time.Second,
			ReadTimeout:  timeout,
			WriteTimeout: timeout,
			// Fail fast and let on_redis_error decide, rather than retrying
			// while the client's request waits.
			MaxRetries:            -1,
			ContextTimeoutEnabled: true,
		})
		shared = ratelimit.NewRedis(client, rl.Redis.KeyPrefix, timeout)
		closeFn = client.Close
		logger.Info("rate limits are shared through Redis", "addr", rl.Redis.Addr, "on_redis_error", rl.OnRedisError)
	}
	return ratelimit.New(ratelimit.Options{
		Shared:  shared,
		OnError: ratelimit.OnError(rl.OnRedisError),
		MaxKeys: rl.MaxKeys,
		Metrics: m,
		Logger:  logger,
	}), closeFn
}

func buildRules(cfg *config.Config) ([]ratelimit.Rule, error) {
	rules := make([]ratelimit.Rule, 0, len(cfg.RateLimit.Rules))
	for _, r := range cfg.RateLimit.Rules {
		key, err := reqkey.Parse(r.Key)
		if err != nil {
			return nil, err
		}
		rules = append(rules, ratelimit.Rule{
			Name:       r.Name,
			PathPrefix: r.PathPrefix,
			Methods:    r.Methods,
			Limit:      r.Limit,
			Window:     r.Window.Std(),
			Key:        key,
		})
	}
	return rules, nil
}
