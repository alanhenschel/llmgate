package budget

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newStore(t *testing.T, now time.Time) (*Store, *miniredis.Miniredis, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cur := now
	s := NewStore(rdb)
	s.now = func() time.Time { return cur }
	return s, mr, &cur
}

func TestRateLimitSlidingWindow(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 10, 0, time.UTC) // 10s into the minute
	s, _, clock := newStore(t, start)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.AllowRequest(ctx, "k", 3); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	err := s.AllowRequest(ctx, "k", 3)
	var le *LimitError
	if !errors.As(err, &le) || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want rate limited, got %v", err)
	}
	if le.RetryAfter <= 0 || le.RetryAfter > 2*time.Minute {
		t.Fatalf("bad retry after %v", le.RetryAfter)
	}
	// Another key is independent.
	if err := s.AllowRequest(ctx, "other", 3); err != nil {
		t.Fatalf("other key: %v", err)
	}
	// Advance past the retry hint: allowed again.
	*clock = clock.Add(le.RetryAfter + time.Second)
	if err := s.AllowRequest(ctx, "k", 3); err != nil {
		t.Fatalf("after retry-after: %v", err)
	}
}

func TestRateLimitPreviousWindowWeight(t *testing.T) {
	// 4 requests at the end of a minute; at 50% into the next minute the
	// estimate is 4*0.5 = 2, so with limit 4 two more fit and a third does not.
	start := time.Date(2026, 1, 1, 12, 0, 59, 0, time.UTC)
	s, _, clock := newStore(t, start)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := s.AllowRequest(ctx, "k", 4); err != nil {
			t.Fatal(err)
		}
	}
	*clock = time.Date(2026, 1, 1, 12, 1, 30, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := s.AllowRequest(ctx, "k", 4); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if err := s.AllowRequest(ctx, "k", 4); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want rate limited, got %v", err)
	}
}

func TestRateLimitConcurrentAtomic(t *testing.T) {
	s, _, _ := newStore(t, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.AllowRequest(context.Background(), "k", 10) == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 10 {
		t.Fatalf("allowed %d, want exactly 10", ok.Load())
	}
}

func TestRateLimitUnlimited(t *testing.T) {
	s, _, _ := newStore(t, time.Now())
	for i := 0; i < 20; i++ {
		if err := s.AllowRequest(context.Background(), "k", 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBudget(t *testing.T) {
	now := time.Date(2026, 3, 5, 22, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		limits     Limits
		tokens     int64
		usdMicros  int64
		wantReason string
	}{
		{"under both", Limits{TokensPerDay: 1000, USDPerDay: 1}, 999, 999_999, ""},
		{"tokens exceeded", Limits{TokensPerDay: 1000, USDPerDay: 1}, 1000, 10, "tokens"},
		{"usd exceeded", Limits{TokensPerDay: 1000, USDPerDay: 1}, 10, 1_000_000, "usd"},
		{"unlimited", Limits{}, 1 << 40, 1 << 40, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := newStore(t, now)
			ctx := context.Background()
			if err := s.CheckBudget(ctx, "k", tt.limits); err != nil {
				t.Fatalf("fresh key rejected: %v", err)
			}
			if err := s.Record(ctx, "k", tt.tokens, tt.usdMicros); err != nil {
				t.Fatal(err)
			}
			err := s.CheckBudget(ctx, "k", tt.limits)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			var le *LimitError
			if !errors.As(err, &le) || !errors.Is(err, ErrBudgetExceeded) || le.Reason != tt.wantReason {
				t.Fatalf("want %s exceeded, got %v", tt.wantReason, err)
			}
			if le.RetryAfter != 2*time.Hour {
				t.Fatalf("retry after = %v, want 2h until UTC midnight", le.RetryAfter)
			}
		})
	}
}

func TestBudgetKeysExpireAndRollOver(t *testing.T) {
	s, mr, clock := newStore(t, time.Date(2026, 3, 5, 23, 0, 0, 0, time.UTC))
	ctx := context.Background()
	if err := s.Record(ctx, "k", 100, 5); err != nil {
		t.Fatal(err)
	}
	tk, _ := dayKeys("k", *clock)
	if ttl := mr.TTL(tk); ttl <= 0 || ttl > dayKeyTTL {
		t.Fatalf("ttl = %v, want (0, %v]", ttl, dayKeyTTL)
	}
	l := Limits{TokensPerDay: 100}
	if err := s.CheckBudget(ctx, "k", l); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("want exceeded, got %v", err)
	}
	*clock = clock.Add(2 * time.Hour) // next UTC day
	if err := s.CheckBudget(ctx, "k", l); err != nil {
		t.Fatalf("new day should reset: %v", err)
	}
}

func TestPing(t *testing.T) {
	s, mr, _ := newStore(t, time.Now())
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	mr.Close()
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("want error with redis down")
	}
}
