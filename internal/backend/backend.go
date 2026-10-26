// Package backend models the upstream servers that edgeproxy forwards traffic to.
package backend

import (
	"fmt"
	"net/url"
	"sync/atomic"
)

// Backend is a single upstream server. Its health and in-flight request count
// are updated concurrently by the health checker and the proxy, so both are atomics.
type Backend struct {
	URL *url.URL

	alive    atomic.Bool
	inflight atomic.Int64
}

// New parses rawURL into a Backend. A backend starts unhealthy and only
// receives traffic once a health check has passed.
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
	return &Backend{URL: u}, nil
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
