package balancer

import (
	"net/http"
	"sync/atomic"

	"edgeproxy/internal/backend"
)

// RoundRobin hands requests to backends in turn. A lock-free counter keeps it
// cheap under heavy concurrency.
type RoundRobin struct {
	backends []*backend.Backend
	next     atomic.Uint64
}

func NewRoundRobin(backends []*backend.Backend) *RoundRobin {
	return &RoundRobin{backends: backends}
}

func (rr *RoundRobin) Next(_ *http.Request, eligible Eligible) *backend.Backend {
	n := uint64(len(rr.backends))
	if n == 0 {
		return nil
	}
	start := rr.next.Add(1) - 1
	for i := uint64(0); i < n; i++ {
		if b := rr.backends[(start+i)%n]; eligible(b) {
			return b
		}
	}
	return nil
}
