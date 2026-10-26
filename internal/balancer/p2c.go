package balancer

import (
	"math/rand/v2"
	"net/http"

	"edgeproxy/internal/backend"
)

// P2C ("power of two choices") samples two random eligible backends and sends
// the request to the less loaded one. It gets nearly all of least-conn's
// benefit without scanning every backend, and avoids least-conn's herding
// problem: when many proxies see the same "least loaded" backend they all pile
// onto it at once, whereas random sampling spreads their choices out.
type P2C struct {
	backends []*backend.Backend
}

func NewP2C(backends []*backend.Backend) *P2C {
	return &P2C{backends: backends}
}

func (p *P2C) Next(_ *http.Request, eligible Eligible) *backend.Backend {
	n := len(p.backends)
	switch n {
	case 0:
		return nil
	case 1:
		if eligible(p.backends[0]) {
			return p.backends[0]
		}
		return nil
	}

	// Fast path: two distinct random backends, both eligible.
	i := rand.IntN(n)
	j := rand.IntN(n - 1)
	if j >= i {
		j++
	}
	if a, b := p.backends[i], p.backends[j]; eligible(a) && eligible(b) {
		return lessLoaded(a, b)
	}

	// Slow path (some backends are down or already tried): take the first two
	// eligible backends from a random starting point.
	var first, second *backend.Backend
	offset := rand.IntN(n)
	for k := 0; k < n && second == nil; k++ {
		b := p.backends[(offset+k)%n]
		if !eligible(b) {
			continue
		}
		if first == nil {
			first = b
		} else {
			second = b
		}
	}
	if second == nil {
		return first
	}
	return lessLoaded(first, second)
}

func lessLoaded(a, b *backend.Backend) *backend.Backend {
	if b.Inflight() < a.Inflight() {
		return b
	}
	return a
}
