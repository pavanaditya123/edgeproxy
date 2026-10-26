// Command edgeproxy is a Layer-7 HTTP load balancer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"edgeproxy/internal/backend"
	"edgeproxy/internal/balancer"
	"edgeproxy/internal/config"
	"edgeproxy/internal/health"
	"edgeproxy/internal/metrics"
	"edgeproxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "configs/local.json", "path to the JSON config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*configPath, logger); err != nil {
		logger.Error("edgeproxy exited", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	backends := make([]*backend.Backend, 0, len(cfg.Backends))
	for _, raw := range cfg.Backends {
		b, err := backend.New(raw)
		if err != nil {
			return err
		}
		backends = append(backends, b)
	}
	bal, err := balancer.New(cfg.Strategy, cfg.HashKey, backends)
	if err != nil {
		return err
	}
	m := metrics.New(backends)
	checker := health.New(health.Config{
		Path:               cfg.HealthCheck.Path,
		Interval:           cfg.HealthCheck.Interval.Std(),
		Timeout:            cfg.HealthCheck.Timeout.Std(),
		HealthyThreshold:   cfg.HealthCheck.HealthyThreshold,
		UnhealthyThreshold: cfg.HealthCheck.UnhealthyThreshold,
	}, backends, logger)
	handler := proxy.New(proxy.Config{
		Backends: backends,
		Balancer: bal,
		Transport: proxy.NewTransport(proxy.TransportConfig{
			DialTimeout:           cfg.Timeouts.Dial.Std(),
			ResponseHeaderTimeout: cfg.Timeouts.ResponseHeader.Std(),
			IdleConnTimeout:       cfg.Timeouts.IdleConn.Std(),
		}),
		MaxRetries: cfg.MaxRetries,
		Metrics:    m,
		Logger:     logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Probe once before accepting traffic so the first requests don't hit dead backends.
	checker.CheckAll(ctx)
	go checker.Run(ctx)

	var draining atomic.Bool
	admin := newAdminServer(cfg.AdminListen, m, backends, &draining)
	public := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader.Std(),
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 2)
	go func() { errc <- serve(admin) }()
	go func() { errc <- serve(public) }()
	logger.Info("edgeproxy started",
		"listen", cfg.Listen, "admin", cfg.AdminListen, "strategy", cfg.Strategy, "backends", len(backends))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Fail readiness first and keep serving for DrainDelay, so whatever sits in
	// front of us (a cloud LB, a Kubernetes Service) stops routing new traffic
	// here before we close the listener.
	draining.Store(true)
	logger.Info("draining", "drain_delay", cfg.Timeouts.DrainDelay.Std().String())
	time.Sleep(cfg.Timeouts.DrainDelay.Std())

	logger.Info("shutting down", "grace", cfg.Timeouts.ShutdownGrace.Std().String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Timeouts.ShutdownGrace.Std())
	defer cancel()
	return errors.Join(public.Shutdown(shutdownCtx), admin.Shutdown(shutdownCtx))
}

func serve(s *http.Server) error {
	if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", s.Addr, err)
	}
	return nil
}
