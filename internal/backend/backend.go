// Package backend models the upstream servers that edgeproxy forwards traffic to.
package backend

import (
	"fmt"
	"net/url"
	"sync/atomic"

	"edgeproxy/internal/outlier"
)

// Backend is a single upstream server. The health checker, the proxy and
// config reloads all touch it concurrently, so every mutable field is atomic.
type Backend struct {
	URL *url.URL

	// Outlier is the passive-health circuit breaker, driven by an outlier.Detector.
	Outlier outlier.State

	alive       atomic.Bool
	inflight    atomic.Int64
	weight      atomic.Int64
	maxInflight atomic.Int64
}

// New parses rawURL into a Backend with weight 1 and no in-flight cap. A
// backend starts unhealthy and only receives traffic once a health check passes.
func New(rawURL string) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse backend url %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("backend url %q: scheme must be http or https", rawURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("backend url %q: missing host", rawURL)
	}
	b := &Backend{URL: u}
	b.weight.Store(1)
	return b, nil
}

// Name is the label used for this backend in logs and metrics.
func (b *Backend) Name() string { return b.URL.Host }

func (b *Backend) Alive() bool { return b.alive.Load() }

// SetAlive records the backend's health and reports whether it changed.
func (b *Backend) SetAlive(alive bool) bool { return b.alive.Swap(alive) != alive }

// Inflight is the number of requests currently being proxied to this backend.
func (b *Backend) Inflight() int64 { return b.inflight.Load() }

func (b *Backend) Acquire() { b.inflight.Add(1) }
func (b *Backend) Release() { b.inflight.Add(-1) }

// Weight is the backend's relative share of traffic under weighted strategies.
func (b *Backend) Weight() int { return int(b.weight.Load()) }

func (b *Backend) SetWeight(w int) {
	if w < 1 {
		w = 1
	}
	b.weight.Store(int64(w))
}

// SetMaxInflight caps concurrent requests to this backend; 0 removes the cap.
func (b *Backend) SetMaxInflight(n int) { b.maxInflight.Store(int64(n)) }

// HasCapacity reports whether the backend is below its in-flight cap. The
// check and the later Acquire aren't one atomic step, so under a burst the cap
// can be exceeded by a few requests; it's a load-shedding guard, not a hard limit.
func (b *Backend) HasCapacity() bool {
	limit := b.maxInflight.Load()
	return limit <= 0 || b.inflight.Load() < limit
}
