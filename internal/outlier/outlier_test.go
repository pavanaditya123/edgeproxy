package outlier

import (
	"testing"
	"time"
)

var policy = Policy{
	ConsecutiveFailures: 3,
	BaseEjection:        10 * time.Second,
	MaxEjection:         time.Minute,
	MaxEjectionPercent:  50,
}

func newPool(n int) (*Detector, []*State) {
	states := make([]*State, n)
	for i := range states {
		states[i] = &State{}
	}
	return NewDetector(policy, states), states
}

func failN(d *Detector, s *State, n int, now time.Time) time.Duration {
	var dur time.Duration
	for i := 0; i < n; i++ {
		dur = d.Report(s, true, now)
	}
	return dur
}

func TestEjectsAfterConsecutiveFailures(t *testing.T) {
	d, s := newPool(4)
	now := time.Unix(1000, 0)
	if dur := failN(d, s[0], 2, now); dur != 0 || s[0].Ejected(now) {
		t.Fatal("ejected before 3 consecutive failures")
	}
	if dur := d.Report(s[0], true, now); dur != 10*time.Second {
		t.Fatalf("third failure ejected for %v, want 10s", dur)
	}
	if !s[0].Ejected(now.Add(9 * time.Second)) {
		t.Error("should still be ejected after 9s")
	}
}

func TestSuccessResetsTheFailureCount(t *testing.T) {
	d, s := newPool(4)
	now := time.Unix(1000, 0)
	failN(d, s[0], 2, now)
	d.Report(s[0], false, now)
	if dur := failN(d, s[0], 2, now); dur != 0 {
		t.Fatal("failures either side of a success should not add up")
	}
}

func TestHalfOpenTrialDecidesTheOutcome(t *testing.T) {
	d, s := newPool(4)
	now := time.Unix(1000, 0)
	failN(d, s[0], 3, now)

	later := now.Add(11 * time.Second)
	if s[0].Ejected(later) {
		t.Fatal("ejection should have expired")
	}
	if got := s[0].Mode(later); got != "half_open" {
		t.Fatalf("mode = %q, want half_open", got)
	}
	// One failure on trial re-ejects immediately, for twice as long.
	if dur := d.Report(s[0], true, later); dur != 20*time.Second {
		t.Fatalf("failed trial ejected for %v, want 20s", dur)
	}

	evenLater := later.Add(21 * time.Second)
	s[0].Ejected(evenLater)
	d.Report(s[0], false, evenLater)
	if got := s[0].Mode(evenLater); got != "ok" {
		t.Fatalf("mode after a successful trial = %q, want ok", got)
	}
}

func TestEjectionTimeIsCapped(t *testing.T) {
	d, s := newPool(4)
	now := time.Unix(1000, 0)
	dur := failN(d, s[0], 3, now)
	for i := 0; i < 10; i++ {
		now = now.Add(dur + time.Second)
		s[0].Ejected(now)
		dur = d.Report(s[0], true, now)
	}
	if dur != time.Minute {
		t.Fatalf("ejection grew to %v, want the 1m cap", dur)
	}
}

func TestNeverEjectsMoreThanMaxPercent(t *testing.T) {
	d, s := newPool(4)
	now := time.Unix(1000, 0)
	for i := range s {
		failN(d, s[i], 3, now)
	}
	ejected := 0
	for i := range s {
		if s[i].Ejected(now) {
			ejected++
		}
	}
	if ejected != 2 {
		t.Fatalf("%d of 4 backends ejected, want at most 50%% = 2", ejected)
	}
}

func TestNeverEjectsTheOnlyBackend(t *testing.T) {
	d, s := newPool(1)
	if dur := failN(d, s[0], 10, time.Unix(1000, 0)); dur != 0 {
		t.Fatal("the only backend must never be ejected")
	}
}

func TestDisabledDetectorIgnoresFailures(t *testing.T) {
	s := &State{}
	d := NewDetector(Policy{}, []*State{s, {}})
	if dur := failN(d, s, 100, time.Unix(1000, 0)); dur != 0 {
		t.Fatal("ConsecutiveFailures = 0 should disable ejection")
	}
	var nilDetector *Detector
	if dur := nilDetector.Report(s, true, time.Now()); dur != 0 {
		t.Fatal("a nil detector should ignore reports")
	}
}
