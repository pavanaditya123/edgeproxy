package balancer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"edgeproxy/internal/backend"
)

func newBackends(t *testing.T, n int) []*backend.Backend {
	t.Helper()
	bs := make([]*backend.Backend, n)
	for i := range bs {
		b, err := backend.New(fmt.Sprintf("http://10.0.0.%d:80", i+1))
		if err != nil {
			t.Fatal(err)
		}
		b.SetAlive(true)
		bs[i] = b
	}
	return bs
}

func alive(b *backend.Backend) bool { return b.Alive() }

func userRequest(id int) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-User-ID", "user-"+strconv.Itoa(id))
	return r
}

func newUserHash(t *testing.T, bs []*backend.Backend) *ConsistentHash {
	t.Helper()
	keyFn, err := KeyFunc("header:X-User-ID")
	if err != nil {
		t.Fatal(err)
	}
	return NewConsistentHash(bs, DefaultReplicas, keyFn)
}

func TestRoundRobinCyclesEvenly(t *testing.T) {
	bs := newBackends(t, 3)
	rr := NewRoundRobin(bs)
	counts := map[*backend.Backend]int{}
	for i := 0; i < 300; i++ {
		counts[rr.Next(nil, alive)]++
	}
	for _, b := range bs {
		if counts[b] != 100 {
			t.Errorf("%s got %d requests, want 100", b.Name(), counts[b])
		}
	}
}

func TestRoundRobinSkipsDeadBackends(t *testing.T) {
	bs := newBackends(t, 3)
	bs[1].SetAlive(false)
	rr := NewRoundRobin(bs)
	for i := 0; i < 30; i++ {
		if got := rr.Next(nil, alive); got == bs[1] {
			t.Fatalf("picked dead backend %s", got.Name())
		}
	}
}

func TestLeastConnPicksLeastLoaded(t *testing.T) {
	bs := newBackends(t, 3)
	bs[0].Acquire()
	bs[0].Acquire()
	bs[2].Acquire()
	lc := NewLeastConn(bs)
	for i := 0; i < 30; i++ {
		if got := lc.Next(nil, alive); got != bs[1] {
			t.Fatalf("picked %s, want the idle backend %s", got.Name(), bs[1].Name())
		}
	}
}

func TestConsistentHashIsSticky(t *testing.T) {
	ch := newUserHash(t, newBackends(t, 4))
	for i := 0; i < 200; i++ {
		first := ch.Next(userRequest(i), alive)
		for j := 0; j < 5; j++ {
			if got := ch.Next(userRequest(i), alive); got != first {
				t.Fatalf("user %d moved from %s to %s", i, first.Name(), got.Name())
			}
		}
	}
}

func TestConsistentHashSpreadsLoad(t *testing.T) {
	bs := newBackends(t, 4)
	ch := newUserHash(t, bs)
	const keys = 20000
	counts := map[*backend.Backend]int{}
	for i := 0; i < keys; i++ {
		counts[ch.Next(userRequest(i), alive)]++
	}
	for _, b := range bs {
		share := float64(counts[b]) / keys
		if share < 0.15 || share > 0.35 {
			t.Errorf("%s owns %.1f%% of keys, want roughly 25%%", b.Name(), share*100)
		}
	}
}

// When a backend fails, only its own keys should move; that is the whole
// point of consistent hashing over hash(key) % N.
func TestConsistentHashMovesOnlyFailedBackendsKeys(t *testing.T) {
	bs := newBackends(t, 4)
	ch := newUserHash(t, bs)
	const keys = 20000
	before := make([]*backend.Backend, keys)
	for i := range before {
		before[i] = ch.Next(userRequest(i), alive)
	}

	bs[2].SetAlive(false)
	moved := 0
	for i := 0; i < keys; i++ {
		after := ch.Next(userRequest(i), alive)
		if after == bs[2] {
			t.Fatalf("key %d still routed to the dead backend", i)
		}
		if after != before[i] {
			if before[i] != bs[2] {
				t.Fatalf("key %d moved off healthy backend %s", i, before[i].Name())
			}
			moved++
		}
	}
	if moved < keys/8 || moved > keys*3/8 {
		t.Errorf("%d of %d keys moved, want roughly a quarter", moved, keys)
	}
}

func TestBalancersReturnNilWhenNothingEligible(t *testing.T) {
	bs := newBackends(t, 3)
	none := func(*backend.Backend) bool { return false }
	bals := map[string]Balancer{
		"round_robin":     NewRoundRobin(bs),
		"least_conn":      NewLeastConn(bs),
		"consistent_hash": newUserHash(t, bs),
	}
	for name, bal := range bals {
		if got := bal.Next(userRequest(1), none); got != nil {
			t.Errorf("%s returned %s, want nil", name, got.Name())
		}
	}
}

func TestKeyFuncIgnoresForwardedFor(t *testing.T) {
	keyFn, err := KeyFunc("client_ip")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:51234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := keyFn(r); got != "203.0.113.7" {
		t.Errorf("key = %q, want the TCP peer IP 203.0.113.7", got)
	}
}
