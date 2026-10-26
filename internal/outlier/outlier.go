// Package outlier implements passive health checking. It watches the results
// of real requests and temporarily ejects a backend that keeps failing, without
// waiting for the next active health probe. Each backend behaves like a
// circuit breaker:
//
//	closed    (normal)    -- N failures in a row -->  open (ejected)
//	open      (ejected)   -- ejection time passes -->  half-open (on trial)
//	half-open (on trial)  -- one success          -->  closed
//	half-open (on trial)  -- one failure          -->  open again, for twice as long
package outlier

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	modeClosed int32 = iota
	modeOpen
	modeHalfOpen
)

type Policy struct {
	// ConsecutiveFailures ejects a backend after this many failed requests in a
	// row. 0 disables outlier detection.
	ConsecutiveFailures int
	// BaseEjection is how long the first ejection lasts; each repeat doubles it.
	BaseEjection time.Duration
	// MaxEjection caps the ejection time. A backend that stays healthy this
	// long after recovering starts again from BaseEjection.
	MaxEjection time.Duration
	// MaxEjectionPercent is the most of the pool that may be ejected at once,
	// so a shared problem (say, a bad deploy everywhere) can't eject everything.
	MaxEjectionPercent int
}

// State is one backend's breaker. Its zero value is a closed breaker.
type State struct {
	mode         atomic.Int32
	failures     atomic.Int32
	ejections    atomic.Int32
	ejectedUntil atomic.Int64 // unix nanoseconds
	closedAt     atomic.Int64 // unix nanoseconds of the last half-open -> closed
}

// Ejected reports whether the backend is ejected right now. Once the ejection
// time has passed it moves the breaker to half-open and returns false, letting
// traffic back in on trial.
func (s *State) Ejected(now time.Time) bool {
	if s.mode.Load() != modeOpen {
		return false
	}
	if now.UnixNano() < s.ejectedUntil.Load() {
		return true
	}
	s.mode.CompareAndSwap(modeOpen, modeHalfOpen)
	return false
}

// Mode describes the breaker for humans: "ok", "ejected" or "half_open".
func (s *State) Mode(now time.Time) string {
	s.Ejected(now)
	switch s.mode.Load() {
	case modeOpen:
		return "ejected"
	case modeHalfOpen:
		return "half_open"
	}
	return "ok"
}

// Detector applies a Policy across one pool of backends.
type Detector struct {
	policy Policy
	states []*State
	mu     sync.Mutex // serialises ejections so MaxEjectionPercent holds
}

func NewDetector(policy Policy, states []*State) *Detector {
	return &Detector{policy: policy, states: states}
}

// Report records the outcome of one request to the backend owning s. It
// returns how long the backend was ejected for, or 0 if it wasn't ejected.
// A nil Detector, or one with detection disabled, ignores every report.
func (d *Detector) Report(s *State, failed bool, now time.Time) time.Duration {
	if d == nil || d.policy.ConsecutiveFailures <= 0 {
		return 0
	}
	if !failed {
		s.failures.Store(0)
		if s.mode.CompareAndSwap(modeHalfOpen, modeClosed) {
			s.closedAt.Store(now.UnixNano())
		}
		return 0
	}
	switch s.mode.Load() {
	case modeOpen:
		return 0 // a request that was already in flight when the backend was ejected
	case modeHalfOpen:
		return d.eject(s, now) // the trial failed
	}
	if s.failures.Add(1) < int32(d.policy.ConsecutiveFailures) {
		return 0
	}
	return d.eject(s, now)
}

func (d *Detector) eject(s *State, now time.Time) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s.mode.Load() == modeOpen {
		return 0
	}
	ejected := 0
	for _, o := range d.states {
		if o != s && o.Ejected(now) {
			ejected++
		}
	}
	if (ejected+1)*100 > d.policy.MaxEjectionPercent*len(d.states) {
		// Ejecting this one too would take out too much of the pool; keep serving.
		s.failures.Store(0)
		return 0
	}

	if closedAt := s.closedAt.Load(); closedAt != 0 && now.UnixNano()-closedAt > int64(d.policy.MaxEjection) {
		s.ejections.Store(0)
	}
	n := s.ejections.Add(1)
	dur := d.policy.BaseEjection
	for i := int32(1); i < n && dur < d.policy.MaxEjection; i++ {
		dur *= 2
	}
	dur = min(dur, d.policy.MaxEjection)

	s.ejectedUntil.Store(now.Add(dur).UnixNano())
	s.failures.Store(0)
	s.mode.Store(modeOpen)
	return dur
}
