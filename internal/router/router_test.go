package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/alanhenschel/llmgate/internal/config"
	"github.com/alanhenschel/llmgate/internal/metrics"
	"github.com/alanhenschel/llmgate/internal/provider"
)

// val reads the current value of a counter or gauge.
func val(c prometheus.Metric) float64 {
	var d dto.Metric
	if err := c.Write(&d); err != nil {
		panic(err)
	}
	if d.Counter != nil {
		return d.Counter.GetValue()
	}
	return d.Gauge.GetValue()
}

// fake is a scriptable provider.
type fake struct {
	name   string
	calls  atomic.Int32
	chat   func(ctx context.Context) (*provider.Response, error)
	stream func(ctx context.Context) (provider.Stream, error)
}

func (f *fake) Name() string { return f.name }
func (f *fake) Type() string { return "fake" }
func (f *fake) Chat(ctx context.Context, _ *provider.Request) (*provider.Response, error) {
	f.calls.Add(1)
	return f.chat(ctx)
}
func (f *fake) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	f.calls.Add(1)
	return f.stream(ctx)
}

func okChat(body string) func(context.Context) (*provider.Response, error) {
	return func(context.Context) (*provider.Response, error) {
		return &provider.Response{Body: []byte(body), Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1}}, nil
	}
}

func failChat(err error) func(context.Context) (*provider.Response, error) {
	return func(context.Context) (*provider.Response, error) { return nil, err }
}

func hangChat(ctx context.Context) (*provider.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// scriptedStream yields items in order; an error item is returned as-is.
type scriptedStream struct {
	items  []any // string chunk or error
	i      int
	closed atomic.Bool
}

func (s *scriptedStream) Recv() (provider.Chunk, error) {
	if s.i >= len(s.items) {
		return provider.Chunk{}, io.EOF
	}
	it := s.items[s.i]
	s.i++
	if err, ok := it.(error); ok {
		return provider.Chunk{}, err
	}
	return provider.Chunk{Data: []byte(it.(string))}, nil
}
func (s *scriptedStream) Usage() provider.Usage {
	return provider.Usage{PromptTokens: 2, CompletionTokens: 3}
}
func (s *scriptedStream) Close() error { s.closed.Store(true); return nil }

func streamOf(items ...any) func(context.Context) (provider.Stream, error) {
	return func(context.Context) (provider.Stream, error) { return &scriptedStream{items: items}, nil }
}

func streamErr(err error) func(context.Context) (provider.Stream, error) {
	return func(context.Context) (provider.Stream, error) { return nil, err }
}

type rig struct {
	r *Router
	m *metrics.Metrics
	p map[string]*fake
}

func newRig(t *testing.T, timeout time.Duration, threshold int, fakes ...*fake) *rig {
	t.Helper()
	cfg := &config.Config{
		RequestTimeout: timeout,
		CircuitBreaker: config.CircuitBreaker{FailureThreshold: threshold, OpenFor: time.Minute},
		Routes:         map[string][]config.Target{"r": nil},
	}
	provs := map[string]provider.Provider{}
	byName := map[string]*fake{}
	for _, f := range fakes {
		provs[f.name] = f
		byName[f.name] = f
		cfg.Routes["r"] = append(cfg.Routes["r"], config.Target{Provider: f.name, Model: "m-" + f.name})
	}
	m := metrics.New()
	r, err := New(cfg, provs, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &rig{r: r, m: m, p: byName}
}

func (g *rig) upstream(provider, model, outcome string) float64 {
	return val(g.m.UpstreamRequests.WithLabelValues(provider, model, outcome))
}

var req = &provider.Request{Model: "alias"}

func TestChatFallback(t *testing.T) {
	tests := []struct {
		name         string
		primaryErr   error
		hang         bool
		wantFallback bool
		wantAttempts int
	}{
		{"5xx falls back", &provider.StatusError{Status: 500, Message: "boom"}, false, true, 2},
		{"503 falls back", &provider.StatusError{Status: 503}, false, true, 2},
		{"429 falls back", &provider.StatusError{Status: 429}, false, true, 2},
		{"connection error falls back", errors.New("dial tcp: connection refused"), false, true, 2},
		{"timeout falls back", nil, true, true, 2},
		{"400 does not fall back", &provider.StatusError{Status: 400, Message: "bad"}, false, false, 1},
		{"404 does not fall back", &provider.StatusError{Status: 404}, false, false, 1},
		{"422 does not fall back", &provider.StatusError{Status: 422}, false, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary := &fake{name: "p1", chat: failChat(tt.primaryErr)}
			if tt.hang {
				primary.chat = hangChat
			}
			secondary := &fake{name: "p2", chat: okChat("ok")}
			g := newRig(t, 50*time.Millisecond, 100, primary, secondary)

			res, err := g.r.Chat(context.Background(), "r", req)
			if tt.wantFallback {
				if err != nil {
					t.Fatal(err)
				}
				if res.Provider != "p2" || res.Model != "m-p2" || res.Attempts != tt.wantAttempts || string(res.Response.Body) != "ok" {
					t.Fatalf("result %+v", res)
				}
				if got := val(g.m.Fallbacks.WithLabelValues("p1", "p2")); got != 1 {
					t.Fatalf("fallbacks_total = %v", got)
				}
				wantOutcome := metrics.OutcomeError
				if tt.hang {
					wantOutcome = metrics.OutcomeTimeout
				}
				if got := g.upstream("p1", "m-p1", wantOutcome); got != 1 {
					t.Fatalf("p1 outcome %s = %v", wantOutcome, got)
				}
				if got := g.upstream("p2", "m-p2", metrics.OutcomeOK); got != 1 {
					t.Fatalf("p2 ok = %v", got)
				}
				return
			}
			var se *provider.StatusError
			var f *Failure
			if !errors.As(err, &se) || !errors.As(err, &f) || errors.Is(err, ErrAllFailed) {
				t.Fatalf("want passthrough client error, got %v", err)
			}
			if f.Attempts != 1 || secondary.calls.Load() != 0 {
				t.Fatalf("attempts=%d secondary calls=%d, want no fallback", f.Attempts, secondary.calls.Load())
			}
		})
	}
}

func TestChatAllFailed(t *testing.T) {
	g := newRig(t, time.Second, 100,
		&fake{name: "p1", chat: failChat(&provider.StatusError{Status: 500})},
		&fake{name: "p2", chat: failChat(&provider.StatusError{Status: 502})})
	_, err := g.r.Chat(context.Background(), "r", req)
	var f *Failure
	if !errors.Is(err, ErrAllFailed) || !errors.As(err, &f) || f.Attempts != 2 || f.Timeout {
		t.Fatalf("got %v", err)
	}
}

func TestChatAllTimedOut(t *testing.T) {
	g := newRig(t, 20*time.Millisecond, 100, &fake{name: "p1", chat: hangChat}, &fake{name: "p2", chat: hangChat})
	_, err := g.r.Chat(context.Background(), "r", req)
	var f *Failure
	if !errors.As(err, &f) || !f.Timeout || !f.Exhausted || f.Attempts != 2 {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownRoute(t *testing.T) {
	g := newRig(t, time.Second, 5, &fake{name: "p1", chat: okChat("x")})
	if _, err := g.r.Chat(context.Background(), "nope", req); !errors.Is(err, ErrUnknownRoute) {
		t.Fatalf("got %v", err)
	}
}

func TestClientCancelDoesNotFallBackOrTripBreaker(t *testing.T) {
	p1 := &fake{name: "p1", chat: hangChat}
	p2 := &fake{name: "p2", chat: okChat("x")}
	g := newRig(t, time.Minute, 1, p1, p2)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := g.r.Chat(ctx, "r", req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if p2.calls.Load() != 0 || g.r.BreakerState("p1") != Closed {
		t.Fatalf("p2 calls=%d breaker=%v", p2.calls.Load(), g.r.BreakerState("p1"))
	}
}

func TestBreakerOpensAndSkips(t *testing.T) {
	p1 := &fake{name: "p1", chat: failChat(&provider.StatusError{Status: 500})}
	p2 := &fake{name: "p2", chat: okChat("ok")}
	g := newRig(t, time.Second, 3, p1, p2)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g.r.breakers["p1"].now = clk.Now

	for i := 0; i < 3; i++ {
		if _, err := g.r.Chat(context.Background(), "r", req); err != nil {
			t.Fatal(err)
		}
	}
	if g.r.BreakerState("p1") != Open {
		t.Fatalf("breaker = %v, want open", g.r.BreakerState("p1"))
	}
	if v := val(g.m.CircuitState.WithLabelValues("p1")); v != 2 {
		t.Fatalf("circuit gauge = %v, want 2", v)
	}

	res, err := g.r.Chat(context.Background(), "r", req)
	if err != nil || res.Provider != "p2" || res.Attempts != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if p1.calls.Load() != 3 {
		t.Fatalf("p1 called %d times, want 3 (4th skipped)", p1.calls.Load())
	}
	if g.upstream("p1", "m-p1", metrics.OutcomeSkippedOpen) != 1 {
		t.Fatal("skipped_open not recorded")
	}

	// Recovery: after open_for one probe is let through; success closes it.
	p1.chat = okChat("back")
	clk.Advance(2 * time.Minute)
	res, err = g.r.Chat(context.Background(), "r", req)
	if err != nil || res.Provider != "p1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if g.r.BreakerState("p1") != Closed {
		t.Fatalf("breaker = %v, want closed", g.r.BreakerState("p1"))
	}
}

func TestAllBreakersOpen(t *testing.T) {
	g := newRig(t, time.Second, 1, &fake{name: "p1", chat: failChat(&provider.StatusError{Status: 500})})
	_, _ = g.r.Chat(context.Background(), "r", req)
	_, err := g.r.Chat(context.Background(), "r", req)
	var f *Failure
	if !errors.Is(err, ErrAllFailed) || !errors.As(err, &f) || f.Attempts != 0 {
		t.Fatalf("got %v", err)
	}
}

func TestClientErrorDoesNotTripBreaker(t *testing.T) {
	g := newRig(t, time.Second, 2, &fake{name: "p1", chat: failChat(&provider.StatusError{Status: 400})})
	for i := 0; i < 5; i++ {
		_, _ = g.r.Chat(context.Background(), "r", req)
	}
	if g.r.BreakerState("p1") != Closed {
		t.Fatal("4xx must not open the breaker")
	}
}

func recvAll(t *testing.T, s provider.Stream) (got []string, err error) {
	t.Helper()
	for {
		c, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		got = append(got, string(c.Data))
	}
}

func TestStreamFallbackBeforeFirstChunk(t *testing.T) {
	tests := []struct {
		name    string
		primary *fake
	}{
		{"open fails 503", &fake{name: "p1", stream: streamErr(&provider.StatusError{Status: 503})}},
		{"open fails 429", &fake{name: "p1", stream: streamErr(&provider.StatusError{Status: 429})}},
		{"error before first chunk", &fake{name: "p1", stream: streamOf(&provider.StatusError{Status: 529})}},
		{"empty stream", &fake{name: "p1", stream: streamOf()}},
		{"hangs before first chunk", &fake{name: "p1", stream: func(ctx context.Context) (provider.Stream, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p2 := &fake{name: "p2", stream: streamOf("a", "b")}
			g := newRig(t, 50*time.Millisecond, 100, tt.primary, p2)
			res, err := g.r.Stream(context.Background(), "r", req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Stream.Close()
			got, err := recvAll(t, res.Stream)
			if err != nil || strings.Join(got, "") != "ab" || res.Provider != "p2" || res.Attempts != 2 {
				t.Fatalf("got=%v err=%v res=%+v", got, err, res)
			}
		})
	}
}

func TestStreamNoFallbackAfterFirstChunk(t *testing.T) {
	midErr := errors.New("connection reset")
	p1 := &fake{name: "p1", stream: streamOf("a", midErr)}
	p2 := &fake{name: "p2", stream: streamOf("x")}
	g := newRig(t, time.Second, 100, p1, p2)

	res, err := g.r.Stream(context.Background(), "r", req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := recvAll(t, res.Stream)
	if !errors.Is(err, midErr) || strings.Join(got, "") != "a" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	_ = res.Stream.Close()
	if p2.calls.Load() != 0 {
		t.Fatal("must not fall back after first chunk was delivered")
	}
	if g.upstream("p1", "m-p1", metrics.OutcomeError) != 1 {
		t.Fatal("mid-stream error not recorded")
	}
	if g.upstream("p1", "m-p1", metrics.OutcomeOK) != 0 {
		t.Fatal("must not also count ok")
	}
	var d dto.Metric
	if err := g.m.TTFB.WithLabelValues("p1").(prometheus.Metric).Write(&d); err != nil || d.Histogram.GetSampleCount() != 1 {
		t.Fatalf("ttfb samples = %v (%v)", d.Histogram.GetSampleCount(), err)
	}
}

func TestStreamTimerStopsAfterFirstChunk(t *testing.T) {
	// Attempt timeout (30ms) must only guard the first chunk, not the whole stream.
	slow := &slowStream{delay: 80 * time.Millisecond}
	p1 := &fake{name: "p1", stream: func(context.Context) (provider.Stream, error) { return slow, nil }}
	g := newRig(t, 30*time.Millisecond, 100, p1)
	res, err := g.r.Stream(context.Background(), "r", req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := recvAll(t, res.Stream)
	if err != nil || len(got) != 3 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	_ = res.Stream.Close()
	if g.upstream("p1", "m-p1", metrics.OutcomeOK) != 1 {
		t.Fatal("ok not recorded")
	}
}

type slowStream struct {
	delay time.Duration
	i     int
}

func (s *slowStream) Recv() (provider.Chunk, error) {
	if s.i >= 3 {
		return provider.Chunk{}, io.EOF
	}
	if s.i > 0 {
		time.Sleep(s.delay)
	}
	s.i++
	return provider.Chunk{Data: []byte("c")}, nil
}
func (s *slowStream) Usage() provider.Usage { return provider.Usage{} }
func (s *slowStream) Close() error          { return nil }

func TestStreamClientAbortNotCountedAsFailure(t *testing.T) {
	p1 := &fake{name: "p1", stream: streamOf("a", "b", "c")}
	g := newRig(t, time.Second, 1, p1)
	res, err := g.r.Stream(context.Background(), "r", req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Stream.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = res.Stream.Close() // client hung up after first chunk
	if g.r.BreakerState("p1") != Closed || g.upstream("p1", "m-p1", metrics.OutcomeError) != 0 {
		t.Fatal("client abort must not count against provider")
	}
}

func TestNewRejectsUnknownProvider(t *testing.T) {
	cfg := &config.Config{Routes: map[string][]config.Target{"r": {{Provider: "ghost", Model: "m"}}}}
	if _, err := New(cfg, nil, metrics.New(), slog.Default()); err == nil {
		t.Fatal("want error")
	}
}
