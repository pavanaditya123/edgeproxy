package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"net"
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
	"edgeproxy/internal/tlsutil"
)

// server owns the live config and swaps in a new backend pool on reload.
type server struct {
	configPath string
	boot       *config.Config // the config the process started with
	px         *proxy.Proxy
	limiter    *ratelimit.Limiter
	certs      *tlsutil.CertReloader // nil without TLS
	metrics    *metrics.Metrics
	logger     *slog.Logger

	// lookup resolves discovery names; nil means the system resolver.
	lookup lookupFunc

	mu            sync.Mutex // serialises reloads and discovery updates
	stopChecker   context.CancelFunc
	lastConfigSum [sha256.Size]byte
	cfg           *config.Config // the config currently applied
	discovered    discovered
	backendKey    string
}

// apply resolves cfg's backends (static plus DNS-discovered) and applies them.
func (s *server) apply(ctx context.Context, cfg *config.Config) error {
	specs, found := resolveBackends(ctx, cfg, s.discovered, s.resolver(), s.logger)
	if err := s.applySpecs(ctx, cfg, specs); err != nil {
		return err
	}
	s.discovered = found
	return nil
}

// rediscover re-resolves the discovery names and swaps in a new pool only if
// the set of backends changed.
func (s *server) rediscover(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil || len(s.cfg.Discovery.DNS) == 0 {
		return
	}
	specs, found := resolveBackends(ctx, s.cfg, s.discovered, s.resolver(), s.logger)
	s.discovered = found
	if backendSetKey(specs) == s.backendKey {
		return
	}
	if err := s.applySpecs(ctx, s.cfg, specs); err != nil {
		s.logger.Error("applying discovered backends failed", "error", err)
		return
	}
	s.logger.Info("discovered backends changed", "backends", len(specs))
}

func (s *server) resolver() lookupFunc {
	if s.lookup != nil {
		return s.lookup
	}
	return net.DefaultResolver.LookupHost
}

// applySpecs builds a backend pool from specs, probes it, swaps it in, and
// restarts health checking on it. Backends present in both the old and new
// pool keep their health and outlier state.
func (s *server) applySpecs(ctx context.Context, cfg *config.Config, specs []config.BackendSpec) error {
	backends, err := buildBackends(specs, cfg.MaxInflightPerBackend, s.px.Backends())
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

	s.cfg = cfg
	s.backendKey = backendSetKey(specs)
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

// watch reloads on SIGHUP and, if interval > 0, whenever the config file's
// contents change. Every discoveryInterval it re-resolves discovery names.
// It returns when ctx is done.
func (s *server) watch(ctx context.Context, interval, discoveryInterval time.Duration) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	discover := time.NewTicker(discoveryInterval)
	defer discover.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			_ = s.reload(ctx, "SIGHUP")
			s.reloadCerts()
		case <-tick:
			if s.fileChanged() {
				_ = s.reload(ctx, "file change")
			}
			s.reloadCerts()
		case <-discover.C:
			s.rediscover(ctx)
		}
	}
}

// reloadCerts picks up a renewed TLS certificate. A broken new certificate is
// logged and the current one keeps being served.
func (s *server) reloadCerts() {
	if s.certs == nil {
		return
	}
	changed, err := s.certs.Reload()
	switch {
	case err != nil:
		s.logger.Error("TLS certificate reload failed; still serving the previous certificate", "error", err)
	case changed:
		s.logger.Info("TLS certificate reloaded")
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
	check("discovery.interval", boot.Discovery.Interval, next.Discovery.Interval)
	check("tls", boot.TLS, next.TLS)
	return fields
}
