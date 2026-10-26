package proxy

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestBudget(ratio float64, minPerSecond int) (*RetryBudget, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	b := NewRetryBudget(ratio, minPerSecond)
	b.now = clock.now
	return b, clock
}

func spend(b *RetryBudget) int {
	n := 0
	for b.TryRetry() {
		n++
	}
	return n
}

func TestRetryBudgetAllowsRatioOfRequests(t *testing.T) {
	b, _ := newTestBudget(0.2, 0)
	for i := 0; i < 100; i++ {
		b.RecordRequest()
	}
	if got := spend(b); got != 20 {
		t.Fatalf("allowed %d retries for 100 requests, want 20%% = 20", got)
	}
}

func TestRetryBudgetFloorForQuietTraffic(t *testing.T) {
	b, _ := newTestBudget(0.2, 1)
	if got := spend(b); got != 10 {
		t.Fatalf("allowed %d retries with no traffic, want the floor of 1/s over 10s = 10", got)
	}
}

func TestRetryBudgetForgetsOldTraffic(t *testing.T) {
	b, clock := newTestBudget(0.5, 0)
	for i := 0; i < 10; i++ {
		b.RecordRequest()
	}
	spend(b)
	if b.TryRetry() {
		t.Fatal("budget should be empty")
	}

	clock.t = clock.t.Add(11 * time.Second)
	if b.TryRetry() {
		t.Fatal("requests from over 10s ago must not fund new retries")
	}
	for i := 0; i < 10; i++ {
		b.RecordRequest()
	}
	if got := spend(b); got != 5 {
		t.Fatalf("allowed %d retries after the window moved on, want 5", got)
	}
}

func TestNilRetryBudgetAllowsEverything(t *testing.T) {
	var b *RetryBudget
	b.RecordRequest()
	for i := 0; i < 1000; i++ {
		if !b.TryRetry() {
			t.Fatal("a nil budget must not limit retries")
		}
	}
}
