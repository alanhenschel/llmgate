// Package router runs requests through an ordered provider chain with
// per-provider circuit breakers and fallback.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/alanhenschel/llmgate/internal/config"
	"github.com/alanhenschel/llmgate/internal/metrics"
	"github.com/alanhenschel/llmgate/internal/provider"
)

// Sentinel errors.
var (
	ErrUnknownRoute = errors.New("unknown route")
	// ErrAllFailed matches a *Failure produced because every provider in the
	// chain failed or was skipped (as opposed to a non-retryable client error).
	ErrAllFailed = errors.New("all providers failed")
	errAllOpen   = errors.New("all providers unavailable (circuit open)")
)

// Failure is the router's error. Err is the last underlying error.
type Failure struct {
	Attempts  int  // upstream calls actually made (skipped hops excluded)
	Timeout   bool // last failure was a timeout
	Exhausted bool // chain exhausted (vs. non-retryable error or cancel)
	Err       error
}

func (f *Failure) Error() string {
	return fmt.Sprintf("after %d attempt(s): %v", f.Attempts, f.Err)
}
func (f *Failure) Unwrap() error { return f.Err }
func (f *Failure) Is(target error) bool {
	return target == ErrAllFailed && f.Exhausted
}

// Result is a successful call. Exactly one of Response and Stream is set.
type Result struct {
	Provider string
	Model    string
	Attempts int
	Response *provider.Response
	Stream   provider.Stream // first chunk already received; caller must Close
}

// Router holds routes, providers and breakers. It is safe for concurrent use.
type Router struct {
	routes    map[string][]config.Target
	providers map[string]provider.Provider
	breakers  map[string]*Breaker
	m         *metrics.Metrics
	timeout   time.Duration
	log       *slog.Logger
}

// New wires a router. Every provider referenced by a route must be present.
func New(cfg *config.Config, providers map[string]provider.Provider, m *metrics.Metrics, log *slog.Logger) (*Router, error) {
	r := &Router{
		routes: cfg.Routes, providers: providers, m: m, log: log,
		timeout:  cfg.RequestTimeout,
		breakers: make(map[string]*Breaker, len(providers)),
	}
	for name := range providers {
		g := m.CircuitState.WithLabelValues(name)
		g.Set(float64(Closed))
		r.breakers[name] = NewBreaker(cfg.CircuitBreaker.FailureThreshold, cfg.CircuitBreaker.OpenFor, func(s State) {
			g.Set(float64(s))
			log.Warn("circuit state changed", "provider", name, "state", s.String())
		})
	}
	for route, chain := range cfg.Routes {
		for _, t := range chain {
			if _, ok := providers[t.Provider]; !ok {
				return nil, fmt.Errorf("route %q: provider %q not registered", route, t.Provider)
			}
		}
	}
	return r, nil
}

// Routes lists route aliases, sorted.
func (r *Router) Routes() []string {
	out := make([]string, 0, len(r.routes))
	for k := range r.routes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HasRoute reports whether the alias exists.
func (r *Router) HasRoute(name string) bool { _, ok := r.routes[name]; return ok }

// BreakerState exposes a provider's breaker state (tests, diagnostics).
func (r *Router) BreakerState(provider string) State {
	if b, ok := r.breakers[provider]; ok {
		return b.State()
	}
	return Closed
}

// Chat runs a non-streaming request through the chain.
func (r *Router) Chat(ctx context.Context, route string, req *provider.Request) (*Result, error) {
	return r.execute(ctx, route, req, false)
}

// Stream opens a streaming request. Fallback happens only until the first
// chunk has been received from an upstream; the returned Result's Stream
// replays that chunk first, so once the caller has written anything, no
// further fallback is possible by construction.
func (r *Router) Stream(ctx context.Context, route string, req *provider.Request) (*Result, error) {
	return r.execute(ctx, route, req, true)
}

func (r *Router) execute(ctx context.Context, route string, req *provider.Request, stream bool) (*Result, error) {
	chain, ok := r.routes[route]
	if !ok {
		return nil, &Failure{Err: fmt.Errorf("%w: %q", ErrUnknownRoute, route)}
	}
	var (
		attempts int
		lastErr  error
		timeout  bool
		prev     string
	)
	for _, t := range chain {
		p, br := r.providers[t.Provider], r.breakers[t.Provider]
		if !br.Allow() {
			r.m.UpstreamRequests.WithLabelValues(t.Provider, t.Model, metrics.OutcomeSkippedOpen).Inc()
			prev = t.Provider
			continue
		}
		if prev != "" {
			r.m.Fallbacks.WithLabelValues(prev, t.Provider).Inc()
		}
		prev = t.Provider
		attempts++

		res, err := r.attempt(ctx, p, br, t, req, stream)
		if err == nil {
			res.Attempts = attempts
			return res, nil
		}
		if ctx.Err() != nil { // client went away
			return nil, &Failure{Attempts: attempts, Err: ctx.Err()}
		}
		lastErr = err
		timeout = provider.IsTimeout(err) || isAttemptTimeout(err)
		if !provider.Retryable(err) {
			return nil, &Failure{Attempts: attempts, Err: err}
		}
		r.log.Warn("upstream attempt failed, falling back", "route", route, "provider", t.Provider,
			"model", t.Model, "timeout", timeout, "error", err)
	}
	if lastErr == nil {
		lastErr = errAllOpen
	}
	return nil, &Failure{Attempts: attempts, Timeout: timeout, Exhausted: true, Err: lastErr}
}

// errAttemptTimeout marks an attempt cut short by our own per-attempt timer.
var errAttemptTimeout = fmt.Errorf("upstream attempt timed out: %w", context.DeadlineExceeded)

func isAttemptTimeout(err error) bool { return errors.Is(err, errAttemptTimeout) }

func (r *Router) attempt(ctx context.Context, p provider.Provider, br *Breaker, t config.Target, req *provider.Request, stream bool) (*Result, error) {
	actx, cancel := context.WithCancel(ctx)
	var timedOut atomic.Bool
	timer := time.AfterFunc(r.timeout, func() { timedOut.Store(true); cancel() })
	stop := func() { timer.Stop(); cancel() }

	start := time.Now()
	treq := req.WithModel(t.Model)
	var (
		resp   *provider.Response
		st     provider.Stream
		first  provider.Chunk
		err    error
		labels = []string{t.Provider, t.Model}
	)
	if stream {
		st, err = p.Stream(actx, treq)
		if err == nil {
			first, err = st.Recv()
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // empty stream is a broken upstream
			}
			if err != nil {
				_ = st.Close()
			}
		}
	} else {
		resp, err = p.Chat(actx, treq)
	}

	if err != nil {
		stop()
		r.m.UpstreamDuration.WithLabelValues(labels...).Observe(time.Since(start).Seconds())
		if ctx.Err() != nil {
			br.Abort()
			return nil, ctx.Err()
		}
		if timedOut.Load() {
			err = errAttemptTimeout
		}
		outcome := metrics.OutcomeError
		if isAttemptTimeout(err) || provider.IsTimeout(err) {
			outcome = metrics.OutcomeTimeout
		}
		r.m.UpstreamRequests.WithLabelValues(t.Provider, t.Model, outcome).Inc()
		if provider.Retryable(err) {
			br.Failure()
		} else {
			br.Success() // the provider answered; the request was at fault
		}
		return nil, err
	}

	if !stream {
		stop()
		r.m.UpstreamDuration.WithLabelValues(labels...).Observe(time.Since(start).Seconds())
		r.m.UpstreamRequests.WithLabelValues(t.Provider, t.Model, metrics.OutcomeOK).Inc()
		br.Success()
		return &Result{Provider: t.Provider, Model: t.Model, Response: resp}, nil
	}

	// First chunk arrived: from here on there is no fallback. The attempt
	// timer only guards time-to-first-chunk, so long streams are not cut.
	timer.Stop()
	r.m.TTFB.WithLabelValues(t.Provider).Observe(time.Since(start).Seconds())
	br.Success()
	return &Result{Provider: t.Provider, Model: t.Model, Stream: &liveStream{
		inner: st, first: &first, cancel: cancel, parent: ctx, r: r, br: br, t: t, start: start,
	}}, nil
}

// liveStream replays the already-received first chunk, then proxies the
// upstream stream while recording the final outcome exactly once.
type liveStream struct {
	inner  provider.Stream
	first  *provider.Chunk
	cancel context.CancelFunc
	parent context.Context
	r      *Router
	br     *Breaker
	t      config.Target
	start  time.Time
	done   bool
}

func (s *liveStream) Recv() (provider.Chunk, error) {
	if s.first != nil {
		c := *s.first
		s.first = nil
		return c, nil
	}
	c, err := s.inner.Recv()
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		s.finish(nil)
	case s.parent.Err() != nil:
		s.finish(nil) // client disconnected; not the provider's fault
	default:
		s.finish(err)
	}
	return c, err
}

func (s *liveStream) Usage() provider.Usage { return s.inner.Usage() }

func (s *liveStream) Close() error {
	s.finish(nil)
	return s.inner.Close()
}

func (s *liveStream) finish(err error) {
	if s.done {
		return
	}
	s.done = true
	s.cancel()
	outcome := metrics.OutcomeOK
	if err != nil {
		outcome = metrics.OutcomeError
		s.br.Failure()
		s.r.log.Warn("upstream stream failed mid-flight", "provider", s.t.Provider, "model", s.t.Model, "error", err)
	}
	s.r.m.UpstreamRequests.WithLabelValues(s.t.Provider, s.t.Model, outcome).Inc()
	s.r.m.UpstreamDuration.WithLabelValues(s.t.Provider, s.t.Model).Observe(time.Since(s.start).Seconds())
}
