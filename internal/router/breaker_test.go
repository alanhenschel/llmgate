package router

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestBreaker(threshold int, openFor time.Duration) (*Breaker, *fakeClock, *[]State) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var transitions []State
	b := NewBreaker(threshold, openFor, func(s State) { transitions = append(transitions, s) })
	b.now = clk.Now
	return b, clk, &transitions
}

func mustAllow(t *testing.T, b *Breaker, want bool) {
	t.Helper()
	if got := b.Allow(); got != want {
		t.Fatalf("Allow() = %v, want %v (state %v)", got, want, b.State())
	}
}

func TestBreakerTransitions(t *testing.T) {
	b, clk, tr := newTestBreaker(3, 10*time.Second)

	// closed: failures below threshold keep it closed; success resets the count.
	for i := 0; i < 2; i++ {
		mustAllow(t, b, true)
		b.Failure()
	}
	mustAllow(t, b, true)
	b.Success()
	for i := 0; i < 2; i++ {
		mustAllow(t, b, true)
		b.Failure()
	}
	if b.State() != Closed {
		t.Fatalf("state = %v, want closed (count was reset)", b.State())
	}

	// third consecutive failure trips it.
	mustAllow(t, b, true)
	b.Failure()
	if b.State() != Open {
		t.Fatalf("state = %v, want open", b.State())
	}
	mustAllow(t, b, false)
	clk.Advance(9 * time.Second)
	mustAllow(t, b, false)

	// after open_for the next Allow admits exactly one probe.
	clk.Advance(2 * time.Second)
	mustAllow(t, b, true)
	if b.State() != HalfOpen {
		t.Fatalf("state = %v, want half-open", b.State())
	}
	mustAllow(t, b, false)

	// probe fails: back to open with a fresh timer.
	b.Failure()
	if b.State() != Open {
		t.Fatalf("state = %v, want open", b.State())
	}
	mustAllow(t, b, false)
	clk.Advance(10 * time.Second)

	// probe succeeds: closed.
	mustAllow(t, b, true)
	b.Success()
	if b.State() != Closed {
		t.Fatalf("state = %v, want closed", b.State())
	}
	mustAllow(t, b, true)

	want := []State{Open, HalfOpen, Open, HalfOpen, Closed}
	if len(*tr) != len(want) {
		t.Fatalf("transitions = %v, want %v", *tr, want)
	}
	for i := range want {
		if (*tr)[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", *tr, want)
		}
	}
}

func TestBreakerAbortFreesProbe(t *testing.T) {
	b, clk, _ := newTestBreaker(1, time.Second)
	mustAllow(t, b, true)
	b.Failure()
	clk.Advance(time.Second)
	mustAllow(t, b, true) // probe
	mustAllow(t, b, false)
	b.Abort() // client hung up; probe slot released, state unchanged
	if b.State() != HalfOpen {
		t.Fatalf("state = %v, want half-open", b.State())
	}
	mustAllow(t, b, true)
}

func TestBreakerStateString(t *testing.T) {
	for s, want := range map[State]string{Closed: "closed", HalfOpen: "half-open", Open: "open", State(9): "unknown"} {
		if s.String() != want {
			t.Fatalf("%d -> %q, want %q", s, s.String(), want)
		}
	}
}

// After open_for, exactly one of many simultaneous callers becomes the probe.
func TestBreakerSingleProbeUnderContention(t *testing.T) {
	b, clk, _ := newTestBreaker(1, time.Second)
	mustAllow(t, b, true)
	b.Failure()
	clk.Advance(time.Second)

	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if b.Allow() {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("%d probes admitted, want exactly 1", admitted.Load())
	}
}

// Mixed traffic from many goroutines; meaningful under -race.
func TestBreakerRaceStress(t *testing.T) {
	b, clk, _ := newTestBreaker(2, time.Millisecond)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if i%50 == 0 {
					clk.Advance(2 * time.Millisecond)
				}
				if !b.Allow() {
					continue
				}
				switch (g + i) % 3 {
				case 0:
					b.Success()
				case 1:
					b.Failure()
				default:
					b.Abort()
				}
				_ = b.State()
			}
		}(g)
	}
	wg.Wait()
}
