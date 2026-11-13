// Package budget enforces per-key rate limits and daily budgets in Redis.
//
// Every state change is a single Lua script, so concurrent gateway replicas
// sharing one Redis never lose updates or race between read and write.
//
// Rate limiting uses a sliding-window counter: the estimate is
// prev_minute_count * (1 - elapsed/60s) + current_minute_count. It smooths the
// 2x burst a plain fixed window allows at minute boundaries while needing only
// two small integer keys per API key (O(1) memory, unlike a sliding log).
//
// Budgets are daily counters (UTC) with a 48h TTL, so old days vanish on
// their own. Cost is stored as integer micro-dollars to keep INCRBY exact.
package budget

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	rateWindow = time.Minute
	dayKeyTTL  = 48 * time.Hour
)

// Sentinel errors; callers use errors.Is / errors.As.
var (
	ErrRateLimited    = errors.New("rate limit exceeded")
	ErrBudgetExceeded = errors.New("daily budget exceeded")
)

// LimitError carries the Retry-After hint and, for budgets, which limit tripped.
type LimitError struct {
	Kind       error  // ErrRateLimited or ErrBudgetExceeded
	Reason     string // "rpm", "tokens" or "usd"
	RetryAfter time.Duration
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%v (%s), retry after %s", e.Kind, e.Reason, e.RetryAfter.Round(time.Second))
}

func (e *LimitError) Unwrap() error { return e.Kind }

// Limits are the quotas of one API key. Zero means unlimited.
type Limits struct {
	TokensPerDay int64
	USDPerDay    float64
	RPM          int
}

// Store is the Redis-backed limiter.
type Store struct {
	rdb  redis.Scripter
	ping func(context.Context) error
	now  func() time.Time
}

// NewStore wraps a go-redis client.
func NewStore(rdb *redis.Client) *Store {
	return &Store{
		rdb:  rdb,
		ping: func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
		now:  time.Now,
	}
}

// Ping checks Redis reachability (used by /readyz).
func (s *Store) Ping(ctx context.Context) error { return s.ping(ctx) }

var rateScript = redis.NewScript(`
local limit = tonumber(ARGV[1])
local win = tonumber(ARGV[2])
local elapsed = tonumber(ARGV[3])
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local prev = tonumber(redis.call('GET', KEYS[2]) or '0')
local est = prev * (win - elapsed) / win + cur
if est + 1 <= limit then
  redis.call('INCR', KEYS[1])
  redis.call('PEXPIRE', KEYS[1], win * 2)
  return 0
end
local wait
if cur + 1 > limit then
  local frac = 0
  if cur > 0 then frac = 1 - (limit - 1) / cur end
  if frac < 0 then frac = 0 end
  wait = (win - elapsed) + win * frac
else
  wait = win * (1 - (limit - cur - 1) / prev) - elapsed
end
if wait < 1 then wait = 1 end
return math.ceil(wait)
`)

var checkScript = redis.NewScript(`
local tok = tonumber(redis.call('GET', KEYS[1]) or '0')
local usd = tonumber(redis.call('GET', KEYS[2]) or '0')
local tlim = tonumber(ARGV[1])
local ulim = tonumber(ARGV[2])
if tlim > 0 and tok >= tlim then return 1 end
if ulim > 0 and usd >= ulim then return 2 end
return 0
`)

var recordScript = redis.NewScript(`
redis.call('INCRBY', KEYS[1], ARGV[1])
redis.call('INCRBY', KEYS[2], ARGV[2])
redis.call('EXPIRE', KEYS[1], ARGV[3])
redis.call('EXPIRE', KEYS[2], ARGV[3])
return 1
`)

// AllowRequest counts one request against the key's RPM. It returns a
// *LimitError wrapping ErrRateLimited when the limit is hit.
func (s *Store) AllowRequest(ctx context.Context, key string, rpm int) error {
	if rpm <= 0 {
		return nil
	}
	now := s.now()
	win := rateWindow.Milliseconds()
	ms := now.UnixMilli()
	idx := ms / win
	keys := []string{rateKey(key, idx), rateKey(key, idx-1)}
	res, err := rateScript.Run(ctx, s.rdb, keys, rpm, win, ms%win).Int64()
	if err != nil {
		return fmt.Errorf("rate limit script: %w", err)
	}
	if res == 0 {
		return nil
	}
	return &LimitError{Kind: ErrRateLimited, Reason: "rpm", RetryAfter: time.Duration(res) * time.Millisecond}
}

// CheckBudget rejects when the key already consumed its daily allowance.
func (s *Store) CheckBudget(ctx context.Context, key string, l Limits) error {
	if l.TokensPerDay <= 0 && l.USDPerDay <= 0 {
		return nil
	}
	now := s.now()
	tk, uk := dayKeys(key, now)
	res, err := checkScript.Run(ctx, s.rdb, []string{tk, uk}, l.TokensPerDay, micros(l.USDPerDay)).Int64()
	if err != nil {
		return fmt.Errorf("budget check script: %w", err)
	}
	reasons := map[int64]string{1: "tokens", 2: "usd"}
	if r, ok := reasons[res]; ok {
		return &LimitError{Kind: ErrBudgetExceeded, Reason: r, RetryAfter: untilMidnight(now)}
	}
	return nil
}

// Record adds real usage after a call completed.
func (s *Store) Record(ctx context.Context, key string, tokens, usdMicros int64) error {
	if tokens == 0 && usdMicros == 0 {
		return nil
	}
	tk, uk := dayKeys(key, s.now())
	if err := recordScript.Run(ctx, s.rdb, []string{tk, uk}, tokens, usdMicros, int(dayKeyTTL.Seconds())).Err(); err != nil {
		return fmt.Errorf("budget record script: %w", err)
	}
	return nil
}

// {key} is a Redis Cluster hash tag so all keys of one API key share a slot.
func rateKey(key string, window int64) string {
	return fmt.Sprintf("llmgate:rl:{%s}:%d", key, window)
}

func dayKeys(key string, now time.Time) (tokens, usd string) {
	d := now.UTC().Format("20060102")
	return fmt.Sprintf("llmgate:budget:{%s}:%s:tokens", key, d),
		fmt.Sprintf("llmgate:budget:{%s}:%s:usd_micros", key, d)
}

func untilMidnight(now time.Time) time.Duration {
	n := now.UTC()
	next := time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(n)
}

func micros(usd float64) int64 { return int64(usd*1e6 + 0.5) }
