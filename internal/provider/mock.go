package provider

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Mock is a deterministic echo provider with configurable latency and failure
// rate. It lets the whole gateway be demoed, tested and benchmarked without
// API keys.
type Mock struct {
	name       string
	latency    time.Duration
	chunkDelay time.Duration
	failRate   atomic.Uint64 // math.Float64bits; mutable at runtime via admin
}

func NewMock(name string, latency, chunkDelay time.Duration, failRate float64) *Mock {
	m := &Mock{name: name, latency: latency, chunkDelay: chunkDelay}
	m.SetFailRate(failRate)
	return m
}

func (m *Mock) Name() string { return m.name }
func (m *Mock) Type() string { return "mock" }

// SetFailRate sets the probability in [0,1] that a call fails with 503.
func (m *Mock) SetFailRate(r float64) { m.failRate.Store(math.Float64bits(r)) }

// FailRate returns the current failure probability.
func (m *Mock) FailRate() float64 { return math.Float64frombits(m.failRate.Load()) }

func (m *Mock) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Mock) maybeFail() error {
	if r := m.FailRate(); r > 0 && rand.Float64() < r {
		return &StatusError{Status: http.StatusServiceUnavailable, Message: "mock failure injected by " + m.name}
	}
	return nil
}

func (m *Mock) reply(req *Request) string {
	last := ""
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			last = msg.Text()
		}
	}
	return fmt.Sprintf("[%s] echo: %s", m.name, last)
}

func (m *Mock) Chat(ctx context.Context, req *Request) (*Response, error) {
	if err := m.sleep(ctx, m.latency); err != nil {
		return nil, err
	}
	if err := m.maybeFail(); err != nil {
		return nil, err
	}
	text := m.reply(req)
	u := Usage{PromptTokens: EstimateTokens(req.PromptChars()), CompletionTokens: EstimateTokens(len(text))}
	return &Response{Body: completionBody("chatcmpl-mock", req.Model, text, finishStop, u), Usage: u}, nil
}

func (m *Mock) Stream(ctx context.Context, req *Request) (Stream, error) {
	if err := m.sleep(ctx, m.latency); err != nil {
		return nil, err
	}
	if err := m.maybeFail(); err != nil {
		return nil, err
	}
	text := m.reply(req)
	u := Usage{PromptTokens: EstimateTokens(req.PromptChars()), CompletionTokens: EstimateTokens(len(text))}
	cb := newChunkBuilder("chatcmpl-mock", req.Model)
	words := strings.SplitAfter(text, " ")
	chunks := []Chunk{cb.role()}
	for _, w := range words {
		chunks = append(chunks, cb.content(w))
	}
	chunks = append(chunks, cb.finish(finishStop), cb.usage(u))
	return &mockStream{ctx: ctx, m: m, chunks: chunks, usage: u}, nil
}

type mockStream struct {
	ctx    context.Context
	m      *Mock
	chunks []Chunk
	i      int
	usage  Usage
}

func (s *mockStream) Recv() (Chunk, error) {
	if s.i >= len(s.chunks) {
		return Chunk{}, io.EOF
	}
	if s.i > 0 {
		if err := s.m.sleep(s.ctx, s.m.chunkDelay); err != nil {
			return Chunk{}, err
		}
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *mockStream) Usage() Usage { return s.usage }
func (s *mockStream) Close() error { return nil }
