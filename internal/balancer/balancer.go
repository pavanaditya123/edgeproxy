// Package balancer chooses which backend serves each request.
package balancer

import (
	"fmt"
	"net/http"

	"edgeproxy/internal/backend"
)

// Eligible reports whether a backend may be picked for the current attempt.
// The proxy uses it to skip unhealthy backends and ones a retry already tried.
type Eligible func(*backend.Backend) bool

type Balancer interface {
	// Next returns an eligible backend for r, or nil if none is eligible.
	// It must be safe for concurrent use.
	Next(r *http.Request, eligible Eligible) *backend.Backend
}

// New builds the balancer for a strategy name from the config.
func New(strategy, hashKey string, backends []*backend.Backend) (Balancer, error) {
	switch strategy {
	case "round_robin":
		return NewRoundRobin(backends), nil
	case "least_conn":
		return NewLeastConn(backends), nil
	case "consistent_hash":
		keyFn, err := KeyFunc(hashKey)
		if err != nil {
			return nil, err
		}
		return NewConsistentHash(backends, DefaultReplicas, keyFn), nil
	}
	return nil, fmt.Errorf("unknown strategy %q", strategy)
}
