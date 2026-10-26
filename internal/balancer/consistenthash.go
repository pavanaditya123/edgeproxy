package balancer

import (
	"cmp"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"edgeproxy/internal/backend"
)

// DefaultReplicas is the number of virtual nodes per backend. More nodes give a
// more even spread of keys at the cost of a larger ring.
const DefaultReplicas = 128

// KeyFn extracts the value a request is hashed on.
type KeyFn func(*http.Request) string

type point struct {
	hash    uint64
	backend *backend.Backend
}

// ConsistentHash maps each key to a stable backend, so a user keeps hitting the
// same server (useful for caches and sticky sessions).
type ConsistentHash struct {
	ring  []point
	keyFn KeyFn
}

func NewConsistentHash(backends []*backend.Backend, replicas int, keyFn KeyFn) *ConsistentHash {
	ring := make([]point, 0, len(backends)*replicas)
	for _, b := range backends {
		for i := 0; i < replicas; i++ {
			ring = append(ring, point{hash: hashString(b.Name() + "#" + strconv.Itoa(i)), backend: b})
		}
	}
	slices.SortFunc(ring, func(a, b point) int { return cmp.Compare(a.hash, b.hash) })
	return &ConsistentHash{ring: ring, keyFn: keyFn}
}

// Next walks clockwise from the key's position on the ring to the first eligible
// backend. The ring itself never changes when a backend goes down: only that
// backend's keys spill over to their next neighbours, and every other key stays put.
func (c *ConsistentHash) Next(r *http.Request, eligible Eligible) *backend.Backend {
	n := len(c.ring)
	if n == 0 {
		return nil
	}
	h := hashString(c.keyFn(r))
	start, _ := slices.BinarySearchFunc(c.ring, h, func(p point, h uint64) int { return cmp.Compare(p.hash, h) })
	for i := 0; i < n; i++ {
		if p := c.ring[(start+i)%n]; eligible(p.backend) {
			return p.backend
		}
	}
	return nil
}

func hashString(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return mix64(h.Sum64())
}

// mix64 is the splitmix64 finalizer. FNV alone leaves similar strings such as
// "host#1" and "host#2" close together on the ring; mixing spreads them evenly.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// KeyFunc builds the hash-key extractor for a hash_key config value:
// "client_ip" hashes on the TCP peer address, and "header:<Name>" hashes on a
// request header, falling back to the client IP when the header is absent.
func KeyFunc(spec string) (KeyFn, error) {
	switch {
	case spec == "" || spec == "client_ip":
		return clientIP, nil
	case strings.HasPrefix(spec, "header:"):
		name := strings.TrimSpace(strings.TrimPrefix(spec, "header:"))
		if name == "" {
			return nil, errors.New(`hash_key "header:" needs a header name`)
		}
		return func(r *http.Request) string {
			if v := r.Header.Get(name); v != "" {
				return v
			}
			return clientIP(r)
		}, nil
	}
	return nil, fmt.Errorf("unknown hash_key %q", spec)
}

// clientIP returns the TCP peer's IP. It deliberately ignores X-Forwarded-For,
// which any client can forge.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
