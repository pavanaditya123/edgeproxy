package balancer

import (
	"math/rand/v2"
	"net/http"

	"edgeproxy/internal/backend"
)

// LeastConn sends each request to the backend with the fewest in-flight
// requests, which adapts to slow backends far better than round robin.
type LeastConn struct {
	backends []*backend.Backend
}

func NewLeastConn(backends []*backend.Backend) *LeastConn {
	return &LeastConn{backends: backends}
}

func (lc *LeastConn) Next(_ *http.Request, eligible Eligible) *backend.Backend {
	n := len(lc.backends)
	if n == 0 {
		return nil
	}
	// Start the scan at a random offset so ties don't all land on the first backend.
	offset := rand.IntN(n)
	var best *backend.Backend
	var bestLoad int64
	for i := 0; i < n; i++ {
		b := lc.backends[(offset+i)%n]
		if !eligible(b) {
			continue
		}
		if load := b.Inflight(); best == nil || load < bestLoad {
			best, bestLoad = b, load
		}
	}
	return best
}
