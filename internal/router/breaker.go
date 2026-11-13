package router

import (
	"sync"
	"time"
)

// State is a circuit breaker state. Values match the llmgate_circuit_state gauge.
type State int

const (
	Closed   State = 0
	HalfOpen State = 1
	Open     State = 2
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half-open"
	case Open:
		return "open"
	}
	return "unknown"
}

// Breaker is a consecutive-failure circuit breaker.
//
//	closed    --threshold consecutive failures--> open
//	open      --open_for elapsed (on next Allow)--> half-open
//	half-open --probe succeeds--> closed
//	half-open --probe fails--> open
//
// In half-open exactly one probe request is admitted; everything else is
// rejected until the probe reports back. All methods are safe for concurrent
// use.
type Breaker struct {
	threshold int
	openFor   time.Duration
	now       func() time.Time
	onChange  func(State)

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	probing  bool
}

// NewBreaker creates a closed breaker. onChange (optional) is invoked with the
// breaker lock held on every transition: it must be quick and must not call
// back into the breaker.
func NewBreaker(threshold int, openFor time.Duration, onChange func(State)) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	return &Breaker{threshold: threshold, openFor: openFor, now: time.Now, onChange: onChange}
}

func (b *Breaker) set(s State) {
	if b.state == s {
		return
	}
	b.state = s
	if b.onChange != nil {
		b.onChange(s)
	}
}

// Allow reports whether a call may proceed. A true result must be followed by
// exactly one of Success, Failure or Abort.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if b.now().Sub(b.openedAt) < b.openFor {
			return false
		}
		b.set(HalfOpen)
		b.probing = true
		return true
	default: // HalfOpen
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
}

// Success records a healthy answer.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		b.failures = 0
	case HalfOpen:
		b.failures = 0
		b.probing = false
		b.set(Closed)
	}
}

// Failure records a failed call.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		b.failures++
		if b.failures >= b.threshold {
			b.trip()
		}
	case HalfOpen:
		b.probing = false
		b.trip()
	}
}

// Abort releases a permit without judging the provider (e.g. the client hung
// up). In half-open it frees the probe slot.
func (b *Breaker) Abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == HalfOpen {
		b.probing = false
	}
}

func (b *Breaker) trip() {
	b.openedAt = b.now()
	b.failures = 0
	b.set(Open)
}

// State returns the current state without side effects.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
