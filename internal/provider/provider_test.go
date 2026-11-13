package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func userReq(text string) *Request {
	c, _ := json.Marshal(text)
	return &Request{Model: "m", Messages: []Message{{Role: "user", Content: c}}, Raw: map[string]json.RawMessage{}}
}

func drain(t *testing.T, s Stream) (chunks []Chunk) {
	t.Helper()
	for {
		c, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return chunks
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		chunks = append(chunks, c)
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", &StatusError{Status: 429}, true},
		{"500", &StatusError{Status: 500}, true},
		{"529", &StatusError{Status: 529}, true},
		{"400", &StatusError{Status: 400}, false},
		{"404", &StatusError{Status: 404}, false},
		{"401", &StatusError{Status: 401}, false},
		{"wrapped 503", fmt.Errorf("x: %w", &StatusError{Status: 503}), true},
		{"conn error", errors.New("connection refused"), true},
		{"deadline", context.DeadlineExceeded, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Retryable(tt.err); got != tt.want {
				t.Fatalf("Retryable = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSSEReader(t *testing.T) {
	in := ": keepalive\n\nevent: a\ndata: one\n\ndata: two\ndata: three\r\n\r\ndata: tail"
	r := newSSEReader(strings.NewReader(in))
	want := []struct{ ev, data string }{{"a", "one"}, {"", "two\nthree"}, {"", "tail"}}
	for i, w := range want {
		ev, d, err := r.next()
		if err != nil || ev != w.ev || string(d) != w.data {
			t.Fatalf("#%d: got (%q,%q,%v), want (%q,%q)", i, ev, d, err, w.ev, w.data)
		}
	}
	if _, _, err := r.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestMessageText(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{"string", `"hi"`, "hi"},
		{"parts", `[{"type":"text","text":"a"},{"type":"image_url"},{"type":"text","text":"b"}]`, "a\nb"},
		{"null", `null`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Message{Content: json.RawMessage(tt.raw)}).Text(); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestBuildAnthropicRequest(t *testing.T) {
	body := `{"model":"x","max_tokens":77,"temperature":0.3,"stop":"END","messages":[
	 {"role":"system","content":"be brief"},
	 {"role":"user","content":"a"},{"role":"user","content":"b"},
	 {"role":"assistant","content":"c"},{"role":"tool","content":"d"}]}`
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(body), &raw)
	var msgs []Message
	_ = json.Unmarshal(raw["messages"], &msgs)
	got := buildAnthropicRequest(&Request{Model: "claude-x", Messages: msgs, Raw: raw, Stream: true})
	if got.System != "be brief" || got.MaxTokens != 77 || *got.Temperature != 0.3 || !got.Stream {
		t.Fatalf("scalars: %+v", got)
	}
	if len(got.StopSequences) != 1 || got.StopSequences[0] != "END" {
		t.Fatalf("stop: %v", got.StopSequences)
	}
	want := []anthropicMessage{{"user", "a\n\nb"}, {"assistant", "c"}, {"user", "d"}}
	if fmt.Sprint(got.Messages) != fmt.Sprint(want) {
		t.Fatalf("messages: %v want %v", got.Messages, want)
	}
	if def := buildAnthropicRequest(userReq("x")); def.MaxTokens != defaultMaxTokens {
		t.Fatalf("default max_tokens = %d", def.MaxTokens)
	}
}

func TestAnthropicChat(t *testing.T) {
	var gotHeaders http.Header
	var gotBody anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"id":"msg_1","model":"claude-x","stop_reason":"max_tokens","content":[{"type":"text","text":"Hel"},{"type":"text","text":"lo"}],"usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	defer srv.Close()
	p := NewAnthropic("anthropic", srv.URL, "k", nil)
	resp, err := p.Chat(context.Background(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if gotHeaders.Get("x-api-key") != "k" || gotHeaders.Get("anthropic-version") == "" || gotBody.Stream {
		t.Fatalf("headers/body: %v %+v", gotHeaders, gotBody)
	}
	if resp.Usage != (Usage{PromptTokens: 11, CompletionTokens: 7}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message      struct{ Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		}
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || out.Choices[0].Message.Content != "Hello" ||
		out.Choices[0].FinishReason != "length" || out.Usage.TotalTokens != 18 {
		t.Fatalf("translated: %s", resp.Body)
	}
}

const anthropicStreamBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_9","model":"claude-x","usage":{"input_tokens":25,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicStreamTranslation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b anthropicRequest
		_ = json.NewDecoder(r.Body).Decode(&b)
		if !b.Stream {
			t.Error("upstream request must have stream=true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicStreamBody)
	}))
	defer srv.Close()
	s, err := NewAnthropic("a", srv.URL, "k", nil).Stream(context.Background(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks := drain(t, s)

	type parsed struct {
		ID, Model, Object string
		Choices           []struct {
			Delta        struct{ Role, Content string }
			FinishReason *string `json:"finish_reason"`
		}
		Usage *struct{ PromptTokens, CompletionTokens, TotalTokens int }
	}
	var content strings.Builder
	var roles, finishes, usageChunks int
	for _, c := range chunks {
		var p parsed
		if err := json.Unmarshal(c.Data, &p); err != nil {
			t.Fatalf("bad chunk %s: %v", c.Data, err)
		}
		if p.Object != "chat.completion.chunk" || p.ID != "chatcmpl-msg_9" || p.Model != "claude-x" {
			t.Fatalf("envelope: %s", c.Data)
		}
		if c.UsageOnly {
			usageChunks++
			if len(p.Choices) != 0 || p.Usage == nil {
				t.Fatalf("usage chunk: %s", c.Data)
			}
			continue
		}
		ch := p.Choices[0]
		if ch.Delta.Role == "assistant" {
			roles++
		}
		content.WriteString(ch.Delta.Content)
		if ch.FinishReason != nil {
			finishes++
			if *ch.FinishReason != "stop" {
				t.Fatalf("finish_reason %q", *ch.FinishReason)
			}
		}
	}
	if content.String() != "Hello" || roles != 1 || finishes != 1 || usageChunks != 1 {
		t.Fatalf("content=%q roles=%d finishes=%d usage=%d", content.String(), roles, finishes, usageChunks)
	}
	// Role chunk must come first, usage last.
	if !strings.Contains(string(chunks[0].Data), `"role":"assistant"`) || !chunks[len(chunks)-1].UsageOnly {
		t.Fatalf("ordering wrong: %s ... %s", chunks[0].Data, chunks[len(chunks)-1].Data)
	}
	if u := s.Usage(); u != (Usage{PromptTokens: 25, CompletionTokens: 9}) {
		t.Fatalf("usage %+v", u)
	}
}

func TestAnthropicStreamErrors(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantEOF    bool
	}{
		{"error event before content", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n", 529, false},
		{"rate limit event", "data: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow\"}}\n\n", 429, false},
		{"truncated stream", "data: {\"type\":\"message_start\",\"message\":{\"id\":\"x\"}}\n\n", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tt.body) }))
			defer srv.Close()
			s, err := NewAnthropic("a", srv.URL, "k", nil).Stream(context.Background(), userReq("hi"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, err = s.Recv()
			if tt.wantEOF {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("want unexpected EOF, got %v", err)
				}
				return
			}
			var se *StatusError
			if !errors.As(err, &se) || se.Status != tt.wantStatus {
				t.Fatalf("want status %d, got %v", tt.wantStatus, err)
			}
		})
	}
}

func TestAnthropicNotConfigured(t *testing.T) {
	_, err := NewAnthropic("a", "http://unused", "", nil).Chat(context.Background(), userReq("x"))
	if !errors.Is(err, ErrNotConfigured) || !Retryable(err) {
		t.Fatalf("got %v", err)
	}
}

func TestUpstreamStatusErrors(t *testing.T) {
	tests := []struct {
		status    int
		body      string
		wantMsg   string
		retryable bool
	}{
		{400, `{"error":{"message":"bad param","type":"invalid_request_error"}}`, "bad param", false},
		{429, `{"error":{"message":"slow down"}}`, "slow down", true},
		{500, `oops`, "oops", true},
		{502, ``, "Bad Gateway", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			p := NewOpenAI("openai", "openai", srv.URL, "k", true, nil)
			_, err := p.Chat(context.Background(), userReq("x"))
			var se *StatusError
			if !errors.As(err, &se) || se.Status != tt.status || se.Message != tt.wantMsg {
				t.Fatalf("got %v", err)
			}
			if Retryable(err) != tt.retryable {
				t.Fatalf("retryable = %v", !tt.retryable)
			}
		})
	}
}

func TestOpenAIPassthrough(t *testing.T) {
	var gotAuth string
	var gotBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if string(gotBody["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"abcd\"}}],\"usage\":null}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"efgh\"}}],\"usage\":null}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `{"id":"1","choices":[{"message":{"content":"abcdefgh"}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
	}))
	defer srv.Close()

	p := NewOpenAI("openai", "openai", srv.URL, "sk", true, nil)
	req := userReq("hello")
	req.Raw["custom_field"] = json.RawMessage(`42`)
	req.Model = "gpt-test"

	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk" || string(gotBody["model"]) != `"gpt-test"` || string(gotBody["custom_field"]) != "42" {
		t.Fatalf("auth=%q body=%v", gotAuth, gotBody)
	}
	if resp.Usage != (Usage{PromptTokens: 4, CompletionTokens: 2}) || !strings.Contains(string(resp.Body), "abcdefgh") {
		t.Fatalf("resp %+v", resp)
	}

	s, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	chunks := drain(t, s)
	if len(chunks) != 3 || !chunks[2].UsageOnly || chunks[0].UsageOnly {
		t.Fatalf("chunks: %+v", chunks)
	}
	if string(gotBody["stream_options"]) != `{"include_usage":true}` {
		t.Fatalf("stream_options = %s", gotBody["stream_options"])
	}
	if u := s.Usage(); u != (Usage{PromptTokens: 5, CompletionTokens: 3}) {
		t.Fatalf("usage %+v", u)
	}
}

func TestOpenAIStreamEstimatesUsageAndDetectsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"12345678\"}}]}\n\n")
	}))
	defer srv.Close()
	s, err := NewOpenAI("ollama", "ollama", srv.URL, "", false, nil).Stream(context.Background(), userReq("12345678"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want unexpected EOF, got %v", err)
	}
	if u := s.Usage(); u != (Usage{PromptTokens: 2, CompletionTokens: 2, Estimated: true}) {
		t.Fatalf("usage %+v", u)
	}
}

func TestMock(t *testing.T) {
	m := NewMock("mock-a", 0, 0, 0)
	resp, err := m.Chat(context.Background(), userReq("ping"))
	if err != nil || !strings.Contains(string(resp.Body), "[mock-a] echo: ping") {
		t.Fatalf("chat: %v %s", err, resp.Body)
	}
	s, err := m.Stream(context.Background(), userReq("one two"))
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, c := range drain(t, s) {
		var p struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		_ = json.Unmarshal(c.Data, &p)
		for _, ch := range p.Choices {
			sb.WriteString(ch.Delta.Content)
		}
	}
	if sb.String() != "[mock-a] echo: one two" {
		t.Fatalf("streamed %q", sb.String())
	}

	m.SetFailRate(1)
	for name, call := range map[string]func() error{
		"chat":   func() error { _, err := m.Chat(context.Background(), userReq("x")); return err },
		"stream": func() error { _, err := m.Stream(context.Background(), userReq("x")); return err },
	} {
		var se *StatusError
		if err := call(); !errors.As(err, &se) || se.Status != 503 {
			t.Fatalf("%s: want 503, got %v", name, err)
		}
	}
}

func TestMockLatencyHonorsContext(t *testing.T) {
	m := NewMock("slow", 5*time.Second, 0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := m.Chat(ctx, userReq("x"))
	if !errors.Is(err, context.DeadlineExceeded) || !IsTimeout(err) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}
