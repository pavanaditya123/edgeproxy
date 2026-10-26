package proxy

import (
	"sync"
	"time"
)

const budgetWindow = 10 // seconds

// RetryBudget caps retries at a fraction of recent traffic. Without one, a
// failing backend makes every request try 1+max_retries backends, multiplying
// the load on the healthy ones exactly when they can least afford it (a
// "retry storm"). Over the last 10 seconds, retries may be at most Ratio of
// requests, or MinPerSecond per second if that's larger, so quiet services can
// still retry.
//
// A nil *RetryBudget allows every retry.
type RetryBudget struct {
	ratio      float64
	minRetries int // per window

	mu      sync.Mutex
	buckets [budgetWindow]budgetBucket
	now     func() time.Time
}

type budgetBucket struct {
	second   int64
	requests int
	retries  int
}

func NewRetryBudget(ratio float64, minPerSecond int) *RetryBudget {
	return &RetryBudget{ratio: ratio, minRetries: minPerSecond * budgetWindow, now: time.Now}
}

// RecordRequest counts one client request towards the budget.
func (b *RetryBudget) RecordRequest() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.bucket(b.now().Unix()).requests++
	b.mu.Unlock()
}

// TryRetry reports whether one more retry fits in the budget, and if so spends it.
func (b *RetryBudget) TryRetry() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now().Unix()
	requests, retries := 0, 0
	for _, bk := range b.buckets {
		if bk.second > now-budgetWindow {
			requests += bk.requests
			retries += bk.retries
		}
	}
	allowed := max(float64(b.minRetries), b.ratio*float64(requests))
	if float64(retries) >= allowed {
		return false
	}
	b.bucket(now).retries++
	return true
}

func (b *RetryBudget) bucket(second int64) *budgetBucket {
	bk := &b.buckets[second%budgetWindow]
	if bk.second != second {
		*bk = budgetBucket{second: second}
	}
	return bk
}
