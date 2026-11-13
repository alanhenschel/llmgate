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

// OpenAI talks to any OpenAI-compatible /v1/chat/completions endpoint. It
// serves both the "openai" and "ollama" provider types; the request and the
// SSE chunks pass through unchanged.
type OpenAI struct {
	name, typ, baseURL, apiKey string
	needsKey                   bool
	client                     *http.Client
}

// NewOpenAI builds an OpenAI-compatible adapter. needsKey makes calls fail
// with ErrNotConfigured when apiKey is empty.
func NewOpenAI(name, typ, baseURL, apiKey string, needsKey bool, client *http.Client) *OpenAI {
	if client == nil {
		client = DefaultClient()
	}
	return &OpenAI{name: name, typ: typ, baseURL: baseURL, apiKey: apiKey, needsKey: needsKey, client: client}
}

func (p *OpenAI) Name() string { return p.name }
func (p *OpenAI) Type() string { return p.typ }

func (p *OpenAI) body(req *Request) ([]byte, error) {
	raw := make(map[string]json.RawMessage, len(req.Raw)+2)
	for k, v := range req.Raw {
		raw[k] = v
	}
	raw["model"], _ = json.Marshal(req.Model)
	if req.Stream {
		raw["stream"] = json.RawMessage("true")
		// Ask for the final usage chunk so we can account real tokens.
		raw["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		raw["stream"] = json.RawMessage("false")
		delete(raw, "stream_options")
	}
	return json.Marshal(raw)
}

func (p *OpenAI) do(ctx context.Context, req *Request) (*http.Response, error) {
	if p.needsKey && p.apiKey == "" {
		return nil, ErrNotConfigured
	}
	body, err := p.body(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if req.Stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	if p.apiKey != "" {
		hr.Header.Set("Authorization", "Bearer "+p.apiKey)
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

func (p *OpenAI) Chat(ctx context.Context, req *Request) (*Response, error) {
	resp, err := p.do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var parsed struct {
		Usage   *wireUsage `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	u := Usage{}
	if parsed.Usage != nil && parsed.Usage.PromptTokens+parsed.Usage.CompletionTokens > 0 {
		u = Usage{PromptTokens: parsed.Usage.PromptTokens, CompletionTokens: parsed.Usage.CompletionTokens}
	} else {
		chars := 0
		if len(parsed.Choices) > 0 {
			chars = len(parsed.Choices[0].Message.Content)
		}
		u = Usage{PromptTokens: EstimateTokens(req.PromptChars()), CompletionTokens: EstimateTokens(chars), Estimated: true}
	}
	return &Response{Body: body, Usage: u}, nil
}

func (p *OpenAI) Stream(ctx context.Context, req *Request) (Stream, error) {
	r := *req
	r.Stream = true
	resp, err := p.do(ctx, &r)
	if err != nil {
		return nil, err
	}
	return &openaiStream{body: resp.Body, r: newSSEReader(resp.Body), promptChars: req.PromptChars()}, nil
}

type openaiStream struct {
	body        io.ReadCloser
	r           *sseReader
	reported    *Usage
	outChars    int
	promptChars int
}

func (s *openaiStream) Recv() (Chunk, error) {
	_, data, err := s.r.next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			// Upstream closed without [DONE]: the answer is truncated.
			return Chunk{}, io.ErrUnexpectedEOF
		}
		return Chunk{}, err
	}
	if string(bytes.TrimSpace(data)) == "[DONE]" {
		return Chunk{}, io.EOF
	}
	var info struct {
		Error   *struct{ Message string } `json:"error"`
		Usage   *wireUsage                `json:"usage"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &info) != nil {
		return Chunk{Data: data}, nil // not our job to validate; pass through
	}
	if info.Error != nil {
		return Chunk{}, &StatusError{Status: http.StatusBadGateway, Message: info.Error.Message}
	}
	if info.Usage != nil && info.Usage.PromptTokens+info.Usage.CompletionTokens > 0 {
		s.reported = &Usage{PromptTokens: info.Usage.PromptTokens, CompletionTokens: info.Usage.CompletionTokens}
	}
	for _, c := range info.Choices {
		s.outChars += len(c.Delta.Content)
	}
	return Chunk{Data: data, UsageOnly: len(info.Choices) == 0 && info.Usage != nil}, nil
}

func (s *openaiStream) Usage() Usage {
	if s.reported != nil {
		return *s.reported
	}
	return Usage{PromptTokens: EstimateTokens(s.promptChars), CompletionTokens: EstimateTokens(s.outChars), Estimated: true}
}

func (s *openaiStream) Close() error { return s.body.Close() }

const maxResponseBytes = 32 << 20

// statusError converts an error response into a StatusError, extracting the
// message from the usual {"error":{"message":...}} envelope when present.
func statusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &env) == nil && env.Error.Message != "" {
		msg = env.Error.Message
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &StatusError{Status: resp.StatusCode, Message: msg}
}

// DefaultClient is a pooled client without a global timeout: deadlines come
// from request contexts so long streams are not cut off.
func DefaultClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 256
	t.MaxIdleConnsPerHost = 128
	return &http.Client{Transport: t}
}
