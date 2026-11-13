// Package provider defines the upstream abstraction and the OpenAI, Anthropic,
// Ollama and mock adapters. Every adapter speaks OpenAI chat-completion JSON
// towards the router, whatever the upstream wire format is.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Message is one OpenAI chat message. Content is kept raw because it may be a
// string or an array of content parts.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Text returns the plain text of the message, joining text parts.
func (m Message) Text() string {
	c := strings.TrimSpace(string(m.Content))
	switch {
	case strings.HasPrefix(c, `"`):
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return s
		}
	case strings.HasPrefix(c, "["):
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			var out []string
			for _, p := range parts {
				if p.Type == "text" || p.Type == "" {
					out = append(out, p.Text)
				}
			}
			return strings.Join(out, "\n")
		}
	}
	return ""
}

// Request is a chat-completion request in gateway-internal form. Raw keeps the
// client's original fields so OpenAI-compatible upstreams get a faithful
// passthrough.
type Request struct {
	Model    string // upstream model for this attempt
	Stream   bool
	Messages []Message
	Raw      map[string]json.RawMessage
}

// WithModel returns a shallow copy targeting another upstream model.
func (r *Request) WithModel(model string) *Request {
	c := *r
	c.Model = model
	return &c
}

// PromptChars is the character count of all message texts.
func (r *Request) PromptChars() int {
	n := 0
	for _, m := range r.Messages {
		n += len(m.Text())
	}
	return n
}

// Usage is a token count. Estimated is true when the upstream did not report
// real numbers and chars/4 was used instead.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	Estimated        bool
}

// EstimateTokens approximates tokens as chars/4, rounded up.
func EstimateTokens(chars int) int { return (chars + 3) / 4 }

// Response is a complete non-streaming result in OpenAI JSON.
type Response struct {
	Body  []byte
	Usage Usage
}

// Chunk is one chat.completion.chunk JSON payload (no "data: " framing).
// UsageOnly chunks carry just the usage block (empty choices); the API layer
// forwards them only when the client asked for stream_options.include_usage.
type Chunk struct {
	Data      []byte
	UsageOnly bool
}

// Stream yields OpenAI chunks. Recv returns io.EOF after the last one. Usage
// is final once Recv returned io.EOF; before that it is a best-effort partial.
type Stream interface {
	Recv() (Chunk, error)
	Usage() Usage
	Close() error
}

// Provider is one upstream instance.
type Provider interface {
	Name() string
	Type() string
	Chat(ctx context.Context, req *Request) (*Response, error)
	Stream(ctx context.Context, req *Request) (Stream, error)
}

// StatusError is a non-success answer from an upstream.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.Status, e.Message)
}

// Retryable reports whether the failure should trigger fallback: network
// errors, timeouts, HTTP 429 and 5xx. Other 4xx are the client's fault and
// would fail identically on the next provider.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusTooManyRequests || se.Status >= 500
	}
	return true
}

// IsTimeout reports whether err is a deadline or network timeout.
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ErrNotConfigured is returned by adapters that lack credentials. It is
// retryable so a chain can fall through to the next provider.
var ErrNotConfigured = &StatusError{Status: http.StatusServiceUnavailable, Message: "provider api key not configured"}
