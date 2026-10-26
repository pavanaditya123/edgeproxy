// Package health actively probes backends and marks them up or down.
package health

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"edgeproxy/internal/backend"
)

type Config struct {
	Path               string
	Interval           time.Duration
	Timeout            time.Duration
	HealthyThreshold   int
	UnhealthyThreshold int
}

// Checker probes every backend on a fixed interval. A backend only changes
// state after HealthyThreshold or UnhealthyThreshold consecutive results agree,
// so a single slow probe doesn't pull a server out of rotation (flapping).
type Checker struct {
	cfg      Config
	backends []*backend.Backend
	states   []probeState
	client   *http.Client
	logger   *slog.Logger
}

// probeState is only touched by the goroutine probing that backend.
type probeState struct {
	probed    bool
	successes int
	failures  int
}

func New(cfg Config, backends []*backend.Backend, logger *slog.Logger) *Checker {
	return &Checker{
		cfg:      cfg,
		backends: backends,
		states:   make([]probeState, len(backends)),
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:             nil,
				DisableKeepAlives: true, // each probe exercises a fresh connect, like a real new client would
			},
			// A redirect is not a passing health check.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		logger: logger,
	}
}

// Run probes all backends every Interval until ctx is done.
func (c *Checker) Run(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CheckAll(ctx)
		}
	}
}

// CheckAll probes every backend concurrently and waits for the results. It
// must not be called concurrently with itself or Run.
func (c *Checker) CheckAll(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range c.backends {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.record(i, c.probe(ctx, c.backends[i]))
		}(i)
	}
	wg.Wait()
}

func (c *Checker) probe(ctx context.Context, b *backend.Backend) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.JoinPath(c.cfg.Path).String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}
	return nil
}

func (c *Checker) record(i int, err error) {
	b, s := c.backends[i], &c.states[i]
	if err == nil {
		s.successes, s.failures = s.successes+1, 0
	} else {
		s.failures, s.successes = s.failures+1, 0
	}

	alive := b.Alive()
	switch {
	case !s.probed:
		// The first result applies immediately so startup doesn't wait for a full threshold.
		alive = err == nil
	case err == nil && s.successes >= c.cfg.HealthyThreshold:
		alive = true
	case err != nil && s.failures >= c.cfg.UnhealthyThreshold:
		alive = false
	}
	first := !s.probed
	s.probed = true

	changed := b.SetAlive(alive)
	switch {
	case changed && alive:
		c.logger.Info("backend is up", "backend", b.Name())
	case changed || (first && !alive):
		c.logger.Warn("backend is down", "backend", b.Name(), "error", err)
	}
}
