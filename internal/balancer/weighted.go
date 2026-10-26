package balancer

import (
	"net/http"
	"sync"

	"edgeproxy/internal/backend"
)

// WeightedRoundRobin is nginx's smooth weighted round robin. A backend with
// weight 3 gets three times the traffic of one with weight 1, and the picks
// are interleaved (a a b a, not a a a b) so no backend gets a burst.
//
// Each pick, every eligible backend's score grows by its weight; the highest
// score wins and is lowered by the total weight. Over a full cycle every
// backend is picked exactly weight times.
type WeightedRoundRobin struct {
	mu       sync.Mutex
	backends []*backend.Backend
	scores   []int
}

func NewWeightedRoundRobin(backends []*backend.Backend) *WeightedRoundRobin {
	return &WeightedRoundRobin{backends: backends, scores: make([]int, len(backends))}
}

func (w *WeightedRoundRobin) Next(_ *http.Request, eligible Eligible) *backend.Backend {
	w.mu.Lock()
	defer w.mu.Unlock()
	total, best := 0, -1
	for i, b := range w.backends {
		if !eligible(b) {
			continue
		}
		weight := b.Weight()
		w.scores[i] += weight
		total += weight
		if best < 0 || w.scores[i] > w.scores[best] {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	w.scores[best] -= total
	return w.backends[best]
}
