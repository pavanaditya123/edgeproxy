package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"syscall"
	"time"

	"edgeproxy/internal/balancer"
	"edgeproxy/internal/config"
	"edgeproxy/internal/health"
	"edgeproxy/internal/metrics"
	"edgeproxy/internal/proxy"
	"edgeproxy/internal/ratelimit"
)

// server owns the live config and swaps in a new backend pool on reload.
type server struct {
	configPath string
	boot       *config.Config // the config the process started with
	px         *proxy.Proxy
	limiter    *ratelimit.Limiter
	metrics    *metrics.Metrics
	logger     *slog.Logger

	mu            sync.Mutex // serialises reloads
	stopChecker   context.CancelFunc
	lastConfigSum [sha256.Size]byte
}

// apply builds a backend pool from cfg, probes it, swaps it in, and restarts
// health checking on it. Backends that exist in both the old and new config
// keep their health and outlier state.
func (s *server) apply(ctx context.Context, cfg *config.Config) error {
	backends, err := buildBackends(cfg, s.px.Backends())
	if err != nil {
		return err
	}
	bal, err := balancer.New(cfg.Strategy, cfg.HashKey, backends)
	if err != nil {
		return err
	}
	rules, err := buildRules(cfg)
	if err != nil {
		return err
	}
	checker := health.New(health.Config{
		Path:               cfg.HealthCheck.Path,
		Interval:           cfg.HealthCheck.Interval.Std(),
		Timeout:            cfg.HealthCheck.Timeout.Std(),
		HealthyThreshold:   cfg.HealthCheck.HealthyThreshold,
		UnhealthyThreshold: cfg.HealthCheck.UnhealthyThreshold,
	}, backends, s.logger)

	// Probe before swapping in, so new backends don't take traffic before
	// anyone knows whether they're up.
	checker.CheckAll(ctx)
	s.px.SetPool(proxy.PoolConfig{
		Backends:   backends,
		Balancer:   bal,
		Outlier:    newDetector(cfg, backends),
		MaxRetries: cfg.MaxRetries,
	})
	s.limiter.SetRules(rules)

	if s.stopChecker != nil {
		s.stopChecker()
	}
	checkerCtx, cancel := context.WithCancel(ctx)
	s.stopChecker = cancel
	go checker.Run(checkerCtx)
	return nil
}

// reload re-reads the config file and applies it. An invalid file is
// rejected and the running config is kept.
func (s *server) reload(ctx context.Context, trigger string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.configPath)
	if err == nil {
		s.lastConfigSum = sha256.Sum256(data)
	}
	var cfg *config.Config
	if err == nil {
		cfg, err = config.Parse(data)
	}
	if err == nil {
		err = s.apply(ctx, cfg)
	}
	if err != nil {
		s.metrics.ConfigReloads.WithLabelValues("failure").Inc()
		s.logger.Error("config reload failed; keeping the running config", "trigger", trigger, "error", err)
		return err
	}

	s.metrics.ConfigReloads.WithLabelValues("success").Inc()
	if fields := restartOnlyChanges(s.boot, cfg); len(fields) > 0 {
		s.logger.Warn("config reloaded, but some changes only take effect after a restart",
			"trigger", trigger, "fields", fields)
	} else {
		s.logger.Info("config reloaded", "trigger", trigger, "backends", len(cfg.Backends), "strategy", cfg.Strategy)
	}
	return nil
}

// watch reloads on SIGHUP, and whenever the config file's contents change if
// interval > 0, until ctx is done.
func (s *server) watch(ctx context.Context, interval time.Duration) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			_ = s.reload(ctx, "SIGHUP")
		case <-tick:
			if s.fileChanged() {
				_ = s.reload(ctx, "file change")
			}
		}
	}
}

func (s *server) fileChanged() bool {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return false // e.g. mid-rename by an editor; try again next tick
	}
	sum := sha256.Sum256(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	return !bytes.Equal(sum[:], s.lastConfigSum[:])
}

// restartOnlyChanges names settings that differ from boot but can't be
// changed without restarting the process.
func restartOnlyChanges(boot, next *config.Config) []string {
	var fields []string
	check := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			fields = append(fields, name)
		}
	}
	check("listen", boot.Listen, next.Listen)
	check("admin_listen", boot.AdminListen, next.AdminListen)
	check("timeouts", boot.Timeouts, next.Timeouts)
	check("retry_budget", boot.RetryBudget, next.RetryBudget)
	check("reload_interval", boot.ReloadInterval, next.ReloadInterval)
	check("rate_limit.redis", boot.RateLimit.Redis, next.RateLimit.Redis)
	check("rate_limit.on_redis_error", boot.RateLimit.OnRedisError, next.RateLimit.OnRedisError)
	check("rate_limit.max_keys", boot.RateLimit.MaxKeys, next.RateLimit.MaxKeys)
	check("access_log", boot.AccessLog, next.AccessLog)
	check("tracing", boot.Tracing, next.Tracing)
	return fields
}
