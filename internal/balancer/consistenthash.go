package balancer

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"

	"edgeproxy/internal/backend"
	"edgeproxy/internal/reqkey"
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

// KeyFunc builds the hash-key extractor for a hash_key config value; see reqkey.Parse.
func KeyFunc(spec string) (KeyFn, error) {
	fn, err := reqkey.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("hash_key: %w", err)
	}
	return KeyFn(fn), nil
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
