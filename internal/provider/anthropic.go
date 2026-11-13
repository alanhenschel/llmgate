package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	anthropicVersion    = "2023-06-01"
	defaultMaxTokens    = 1024
	anthropicIDPrefix   = "chatcmpl-"
	finishStop          = "stop"
	finishLength        = "length"
	finishToolCalls     = "tool_calls"
	finishContentFilter = "content_filter"
)

// Anthropic adapts the OpenAI chat API onto the Anthropic Messages API,
// including translation of the streaming event protocol.
//
// Supported: text messages, system/developer prompts, max_tokens,
// temperature, top_p, stop. Tools and images are not translated.
type Anthropic struct {
	name, baseURL, apiKey string
	client                *http.Client
}

func NewAnthropic(name, baseURL, apiKey string, client *http.Client) *Anthropic {
	if client == nil {
		client = DefaultClient()
	}
	return &Anthropic{name: name, baseURL: baseURL, apiKey: apiKey, client: client}
}

func (p *Anthropic) Name() string { return p.name }
func (p *Anthropic) Type() string { return "anthropic" }

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

// buildAnthropicRequest maps an OpenAI request to a Messages request.
// Anthropic requires strictly alternating user/assistant turns, so adjacent
// messages with the same role are merged.
func buildAnthropicRequest(req *Request) anthropicRequest {
	out := anthropicRequest{Model: req.Model, MaxTokens: defaultMaxTokens, Stream: req.Stream}
	var system []string
	for _, m := range req.Messages {
		text := m.Text()
		role := m.Role
		switch role {
		case "system", "developer":
			system = append(system, text)
			continue
		case "assistant":
		default: // user, tool, function
			role = "user"
		}
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content += "\n\n" + text
			continue
		}
		out.Messages = append(out.Messages, anthropicMessage{Role: role, Content: text})
	}
	out.System = strings.Join(system, "\n\n")

	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		var n int
		if v, ok := req.Raw[k]; ok && json.Unmarshal(v, &n) == nil && n > 0 {
			out.MaxTokens = n
			break
		}
	}
	if v, ok := req.Raw["temperature"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out.Temperature = &f
		}
	}
	if v, ok := req.Raw["top_p"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out.TopP = &f
		}
	}
	if v, ok := req.Raw["stop"]; ok {
		var one string
		var many []string
		if json.Unmarshal(v, &one) == nil && one != "" {
			out.StopSequences = []string{one}
		} else if json.Unmarshal(v, &many) == nil {
			out.StopSequences = many
		}
	}
	return out
}

func mapStopReason(r string) string {
	switch r {
	case "max_tokens":
		return finishLength
	case "tool_use":
		return finishToolCalls
	case "refusal":
		return finishContentFilter
	default: // end_turn, stop_sequence, pause_turn, ""
		return finishStop
	}
}

func (p *Anthropic) do(ctx context.Context, req *Request) (*http.Response, error) {
	if p.apiKey == "" {
		return nil, ErrNotConfigured
	}
	body, err := json.Marshal(buildAnthropicRequest(req))
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("x-api-key", p.apiKey)
	hr.Header.Set("anthropic-version", anthropicVersion)
	if req.Stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	resp, err := p.client.Do(hr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, statusError(resp)
	}
	return resp, nil
}

func (p *Anthropic) Chat(ctx context.Context, req *Request) (*Response, error) {
	r := *req
	r.Stream = false
	resp, err := p.do(ctx, &r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	var text strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	u := Usage{PromptTokens: m.Usage.InputTokens, CompletionTokens: m.Usage.OutputTokens}
	return &Response{Body: completionBody(anthropicIDPrefix+m.ID, m.Model, text.String(), mapStopReason(m.StopReason), u), Usage: u}, nil
}

func (p *Anthropic) Stream(ctx context.Context, req *Request) (Stream, error) {
	r := *req
	r.Stream = true
	resp, err := p.do(ctx, &r)
	if err != nil {
		return nil, err
	}
	return &anthropicStream{
		body: resp.Body, r: newSSEReader(resp.Body), model: req.Model, promptChars: req.PromptChars(),
	}, nil
}

// anthropicStream translates Messages streaming events to OpenAI chunks:
//
//	message_start        -> remembered (id, model, input tokens)
//	content_block_delta  -> delta.content chunk (text_delta only)
//	message_delta        -> finish_reason chunk, output tokens remembered
//	message_stop         -> usage-only chunk, then io.EOF
//	error                -> *StatusError
//
// The role chunk is emitted lazily right before the first content (or finish)
// chunk, so an upstream error that arrives before any content can still be
// handled by fallback.
type anthropicStream struct {
	body        io.ReadCloser
	r           *sseReader
	model       string
	promptChars int

	cb       chunkBuilder
	haveMeta bool
	started  bool
	stopped  bool
	pending  []Chunk
	in, out  int
	outChars int
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *anthropicStream) emit(c Chunk) {
	if !s.started {
		if !s.haveMeta {
			s.cb = newChunkBuilder(anthropicIDPrefix+"unknown", s.model)
		}
		s.started = true
		s.pending = append(s.pending, s.cb.role())
	}
	s.pending = append(s.pending, c)
}

func (s *anthropicStream) Recv() (Chunk, error) {
	for {
		if len(s.pending) > 0 {
			c := s.pending[0]
			s.pending = s.pending[1:]
			return c, nil
		}
		if s.stopped {
			return Chunk{}, io.EOF
		}
		_, data, err := s.r.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Chunk{}, io.ErrUnexpectedEOF
			}
			return Chunk{}, err
		}
		var ev anthropicEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue // tolerate junk lines
		}
		switch ev.Type {
		case "message_start":
			s.haveMeta = true
			model := ev.Message.Model
			if model == "" {
				model = s.model
			}
			s.cb = newChunkBuilder(anthropicIDPrefix+ev.Message.ID, model)
			s.in = ev.Message.Usage.InputTokens
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				s.outChars += len(ev.Delta.Text)
				s.emit(s.cb.content(ev.Delta.Text))
			}
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				s.out = ev.Usage.OutputTokens
			}
			s.emit(s.cb.finish(mapStopReason(ev.Delta.StopReason)))
		case "message_stop":
			s.stopped = true
			s.pending = append(s.pending, s.cb.usage(s.Usage()))
		case "error":
			return Chunk{}, anthropicStreamError(ev.Error.Type, ev.Error.Message)
		}
	}
}

func anthropicStreamError(typ, msg string) *StatusError {
	status := http.StatusInternalServerError
	switch typ {
	case "overloaded_error":
		status = 529
	case "rate_limit_error":
		status = http.StatusTooManyRequests
	case "invalid_request_error":
		status = http.StatusBadRequest
	}
	return &StatusError{Status: status, Message: msg}
}

func (s *anthropicStream) Usage() Usage {
	if s.out > 0 || s.in > 0 {
		in := s.in
		if in == 0 {
			in = EstimateTokens(s.promptChars)
		}
		out := s.out
		if out == 0 {
			out = EstimateTokens(s.outChars)
		}
		return Usage{PromptTokens: in, CompletionTokens: out, Estimated: s.out == 0}
	}
	return Usage{PromptTokens: EstimateTokens(s.promptChars), CompletionTokens: EstimateTokens(s.outChars), Estimated: true}
}

func (s *anthropicStream) Close() error { return s.body.Close() }
