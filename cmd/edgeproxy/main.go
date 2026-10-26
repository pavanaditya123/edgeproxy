// Command edgeproxy is a Layer-7 HTTP load balancer.
package main

import (
	"context"
	"crypto/sha256"
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

	"edgeproxy/internal/config"
	"edgeproxy/internal/metrics"
	"edgeproxy/internal/observe"
	"edgeproxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "configs/local.json", "path to the JSON config file")
	check := flag.Bool("check", false, "validate the config file and exit (like nginx -t)")
	flag.Parse()

	if *check {
		if _, err := config.Load(*configPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s: config ok\n", *configPath)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*configPath, logger); err != nil {
		logger.Error("edgeproxy exited", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}

	m := metrics.New()
	px := proxy.New(proxy.Config{
		Transport: proxy.NewTransport(proxy.TransportConfig{
			DialTimeout:           cfg.Timeouts.Dial.Std(),
			ResponseHeaderTimeout: cfg.Timeouts.ResponseHeader.Std(),
			IdleConnTimeout:       cfg.Timeouts.IdleConn.Std(),
		}),
		RetryBudget: proxy.NewRetryBudget(cfg.RetryBudget.Ratio, cfg.RetryBudget.MinRetriesPerSecond),
		Metrics:     m,
		Logger:      logger,
	})
	m.WatchBackends(px.Backends)
	limiter, closeRedis := newLimiter(cfg, m, logger)
	defer closeRedis()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if tr := cfg.Tracing; tr.Enabled {
		shutdownTracing, err := observe.SetupTracing(ctx, observe.TracingConfig{
			Endpoint: tr.Endpoint, Insecure: tr.Insecure, SampleRatio: tr.SampleRatio, ServiceName: tr.ServiceName,
		})
		if err != nil {
			return fmt.Errorf("tracing: %w", err)
		}
		defer func() {
			// Flush spans still buffered, but don't hang shutdown on a dead collector.
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTracing(flushCtx)
		}()
		logger.Info("tracing enabled", "endpoint", tr.Endpoint, "sample_ratio", tr.SampleRatio)
	}

	srv := &server{configPath: configPath, boot: cfg, px: px, limiter: limiter, metrics: m, logger: logger, lastConfigSum: sha256.Sum256(data)}
	// The first apply probes every backend before the listener opens, so the
	// first requests don't hit dead backends.
	if err := srv.apply(ctx, cfg); err != nil {
		return err
	}
	go srv.watch(ctx, cfg.ReloadInterval.Std(), cfg.Discovery.Interval.Std())

	var draining atomic.Bool
	admin := newAdminServer(cfg.AdminListen, m, px.Backends, &draining, func() error {
		return srv.reload(ctx, "admin API")
	})
	// Outermost first: every request gets an ID, then a trace span, then an
	// access-log line, so even rate-limited (429) requests are logged and traced.
	var handler http.Handler = limiter.Middleware(px)
	if cfg.AccessLog {
		handler = observe.AccessLog(logger)(handler)
	}
	if cfg.Tracing.Enabled {
		handler = observe.Tracing(handler)
	}
	handler = observe.RequestID(handler)

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
		"listen", cfg.Listen, "admin", cfg.AdminListen, "strategy", cfg.Strategy, "backends", len(px.Backends()))

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
