package ratelimit

import (
	"container/list"
	"context"
	"hash/fnv"
	"sync"
	"time"
)

const localShards = 16

// Local is an in-memory token-bucket store. Each key gets a bucket holding up
// to limit tokens that refills at limit/window per second; a request spends
// one token. Buckets are split across shards with their own locks so
// concurrent requests rarely contend, and each shard evicts its least
// recently used key once it holds maxKeys/16, which bounds memory no matter
// how many distinct clients show up. (An evicted key simply starts again
// with a full bucket.)
type Local struct {
	shards      [localShards]localShard
	maxPerShard int
	now         func() time.Time
}

type localShard struct {
	mu    sync.Mutex
	items map[string]*list.Element
	lru   *list.List // front = most recently used
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

func NewLocal(maxKeys int) *Local {
	l := &Local{maxPerShard: max(1, maxKeys/localShards), now: time.Now}
	for i := range l.shards {
		l.shards[i] = localShard{items: make(map[string]*list.Element), lru: list.New()}
	}
	return l
}

func (l *Local) Allow(_ context.Context, key string, limit int, window time.Duration) (Decision, error) {
	h := fnv.New32a()
	h.Write([]byte(key))
	sh := &l.shards[h.Sum32()%localShards]
	now := l.now()
	capacity := float64(limit)
	rate := capacity / window.Seconds() // tokens per second

	sh.mu.Lock()
	defer sh.mu.Unlock()
	var b *bucket
	if el, ok := sh.items[key]; ok {
		sh.lru.MoveToFront(el)
		b = el.Value.(*bucket)
		b.tokens = min(capacity, b.tokens+now.Sub(b.last).Seconds()*rate)
		b.last = now
	} else {
		b = &bucket{key: key, tokens: capacity, last: now}
		sh.items[key] = sh.lru.PushFront(b)
		if sh.lru.Len() > l.maxPerShard {
			oldest := sh.lru.Back()
			sh.lru.Remove(oldest)
			delete(sh.items, oldest.Value.(*bucket).key)
		}
	}

	d := Decision{Limit: limit}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
	} else {
		d.RetryAfter = seconds((1 - b.tokens) / rate)
	}
	d.Remaining = int(b.tokens)
	d.Reset = seconds((capacity - b.tokens) / rate)
	return d, nil
}

// size is the number of keys being tracked, for tests.
func (l *Local) size() int {
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += l.shards[i].lru.Len()
		l.shards[i].mu.Unlock()
	}
	return n
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
