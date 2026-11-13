package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/alanhenschel/llmgate/internal/budget"
	"github.com/alanhenschel/llmgate/internal/config"
	"github.com/alanhenschel/llmgate/internal/metrics"
	"github.com/alanhenschel/llmgate/internal/pricing"
	"github.com/alanhenschel/llmgate/internal/provider"
	"github.com/alanhenschel/llmgate/internal/router"
)

const testKey = "secret"

type env struct {
	t   *testing.T
	ts  *httptest.Server
	mr  *miniredis.Miniredis
	m   *metrics.Metrics
	cfg *config.Config
}

type option func(*config.Config)

func withKey(k config.Key) option { return func(c *config.Config) { c.Keys = []config.Key{k} } }
func withTimeout(d time.Duration) option {
	return func(c *config.Config) { c.RequestTimeout = d }
}
func withAdmin(c *config.Config) { c.Admin = true }

// newEnv wires a full gateway on miniredis. providers maps instance name to
// config; chain is the "r" route as provider names (model = "m-<name>").
func newEnv(t *testing.T, providers map[string]config.Provider, chain []string, opts ...option) *env {
	t.Helper()
	cfg := &config.Config{
		RequestTimeout: 2 * time.Second,
		CircuitBreaker: config.CircuitBreaker{FailureThreshold: 100, OpenFor: time.Minute},
		Providers:      providers,
		Routes:         map[string][]config.Target{},
		Keys:           []config.Key{{Name: "demo", Key: testKey}},
		Pricing:        map[string]config.ModelCost{},
	}
	for _, n := range chain {
		cfg.Routes["r"] = append(cfg.Routes["r"], config.Target{Provider: n, Model: "m-" + n})
	}
	for _, o := range opts {
		o(cfg)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New()
	provs := map[string]provider.Provider{}
	mocks := map[string]*provider.Mock{}
	for name, pc := range providers {
		p, err := provider.New(name, pc, nil)
		if err != nil {
			t.Fatal(err)
		}
		provs[name] = p
		if mk, ok := p.(*provider.Mock); ok {
			mocks[name] = mk
		}
	}
	rt, err := router.New(cfg, provs, m, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{Config: cfg, Router: rt, Limiter: budget.NewStore(rdb), Metrics: m,
		Prices: pricing.New(map[string]pricing.Price{"m-cost": {InputPer1M: 1000, OutputPer1M: 2000}}), Mocks: mocks, Log: log})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{t: t, ts: ts, mr: mr, m: m, cfg: cfg}
}

func mockProv() config.Provider { return config.Provider{Type: config.TypeMock} }

func upstream(typ, url string) config.Provider {
	return config.Provider{Type: typ, BaseURL: url, APIKey: "k"}
}

func (e *env) do(method, path, auth, body string) *http.Response {
	e.t.Helper()
	return e.doCtx(context.Background(), method, path, auth, body)
}

func (e *env) doCtx(ctx context.Context, method, path, auth, body string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) chat(body string) *http.Response {
	e.t.Helper()
	return e.do("POST", "/v1/chat/completions", testKey, body)
}

const chatBody = `{"model":"r","messages":[{"role":"user","content":"hello"}]}`
const streamBody = `{"model":"r","stream":true,"messages":[{"role":"user","content":"hello"}]}`

func readAll(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type apiError struct {
	Error struct{ Message, Type, Code string }
}

func decodeErr(t *testing.T, r *http.Response) apiError {
	t.Helper()
	var e apiError
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// metricValue sums a metric over series whose labels contain all of want.
func (e *env) metricValue(name string, want map[string]string) float64 {
	e.t.Helper()
	mfs, err := e.m.Registry.Gather()
	if err != nil {
		e.t.Fatal(err)
	}
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			have := map[string]string{}
			for _, l := range m.GetLabel() {
				have[l.GetName()] = l.GetValue()
			}
			for k, v := range want {
				if have[k] != v {
					continue next
				}
			}
			switch {
			case m.Counter != nil:
				sum += m.Counter.GetValue()
			case m.Gauge != nil:
				sum += m.Gauge.GetValue()
			case m.Histogram != nil:
				sum += float64(m.Histogram.GetSampleCount())
			}
		}
	}
	return sum
}

// openAIServer replies with a fixed status/body and counts hits.
func openAIServer(t *testing.T, status int, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

const okCompletion = `{"id":"c1","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`

func TestAuth(t *testing.T) {
	e := newEnv(t, map[string]config.Provider{"mock": mockProv()}, []string{"mock"})
	tests := []struct{ name, auth string }{
		{"no header", ""},
		{"wrong key", "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := e.do("POST", "/v1/chat/completions", tt.auth, chatBody)
			if resp.StatusCode != 401 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if got := decodeErr(t, resp).Error; got.Code != "invalid_api_key" || got.Type == "" || got.Message == "" {
				t.Fatalf("error shape: %+v", got)
			}
		})
	}
	if resp := e.do("GET", "/v1/models", "", ""); resp.StatusCode != 401 {
		t.Fatalf("models without auth: %d", resp.StatusCode)
	}
	if e.metricValue("llmgate_requests_total", map[string]string{"status": "client_error"}) < 2 {
		t.Fatal("401s not counted")
	}
}

func TestChatSuccessAccounting(t *testing.T) {
	up := openAIServer(t, 200, okCompletion, nil)
	e := newEnv(t, map[string]config.Provider{"cost": upstream("openai", up.URL)}, []string{"cost"})
	// Price table has "m-cost" at $1000/$2000 per 1M tokens.
	resp := e.chat(chatBody)
	if resp.StatusCode != 200 || resp.Header.Get("X-LLMGate-Provider") != "cost" ||
		resp.Header.Get("X-LLMGate-Attempts") != "1" || resp.Header.Get("X-Request-ID") == "" {
		t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
	if body := readAll(t, resp); !strings.Contains(body, "hi there") {
		t.Fatalf("body %s", body)
	}
	// cost = 100*1000/1e6 + 50*2000/1e6 = 0.2 USD
	if got := e.metricValue("llmgate_cost_usd_total", map[string]string{"model": "m-cost"}); got < 0.1999 || got > 0.2001 {
		t.Fatalf("cost = %v, want 0.2", got)
	}
	in := e.metricValue("llmgate_tokens_total", map[string]string{"direction": "input"})
	out := e.metricValue("llmgate_tokens_total", map[string]string{"direction": "output"})
	if in != 100 || out != 50 {
		t.Fatalf("tokens in=%v out=%v", in, out)
	}
	keys := e.mr.Keys()
	var tokKey, usdKey string
	for _, k := range keys {
		if strings.HasSuffix(k, ":tokens") {
			tokKey = k
		}
		if strings.HasSuffix(k, ":usd_micros") {
			usdKey = k
		}
	}
	if v, _ := e.mr.Get(tokKey); v != "150" {
		t.Fatalf("redis tokens = %q (keys %v)", v, keys)
	}
	if v, _ := e.mr.Get(usdKey); v != "200000" {
		t.Fatalf("redis usd micros = %q", v)
	}
	if e.metricValue("llmgate_requests_total", map[string]string{"status": "ok", "route": "r", "key": "demo"}) != 1 {
		t.Fatal("requests_total ok missing")
	}
}

func TestFallbackBehaviour(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantStatus   int
		wantProvider string // "" = error expected
		wantAttempts string
		secondHits   int32
	}{
		{"500 falls back", 500, `{"error":{"message":"boom"}}`, 200, "second", "2", 1},
		{"503 falls back", 503, ``, 200, "second", "2", 1},
		{"429 falls back", 429, `{"error":{"message":"slow"}}`, 200, "second", "2", 1},
		{"400 is returned, no fallback", 400, `{"error":{"message":"bad temperature"}}`, 400, "", "1", 0},
		{"404 is returned, no fallback", 404, `{"error":{"message":"no such model"}}`, 404, "", "1", 0},
		{"upstream 401 becomes 502, no fallback", 401, `{"error":{"message":"bad key"}}`, 502, "", "1", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var first, second atomic.Int32
			s1 := openAIServer(t, tt.status, tt.body, &first)
			s2 := openAIServer(t, 200, okCompletion, &second)
			e := newEnv(t, map[string]config.Provider{
				"first": upstream("openai", s1.URL), "second": upstream("openai", s2.URL),
			}, []string{"first", "second"})
			resp := e.chat(chatBody)
			if resp.StatusCode != tt.wantStatus || resp.Header.Get("X-LLMGate-Attempts") != tt.wantAttempts {
				t.Fatalf("status=%d attempts=%s body=%s", resp.StatusCode, resp.Header.Get("X-LLMGate-Attempts"), readAll(t, resp))
			}
			if tt.wantProvider != "" && resp.Header.Get("X-LLMGate-Provider") != tt.wantProvider {
				t.Fatalf("provider = %s", resp.Header.Get("X-LLMGate-Provider"))
			}
			if tt.wantProvider == "" {
				if msg := decodeErr(t, resp).Error.Message; msg == "" {
					t.Fatal("empty error message")
				}
			}
			if second.Load() != tt.secondHits {
				t.Fatalf("second hits = %d, want %d", second.Load(), tt.secondHits)
			}
		})
	}
}

func TestAllProvidersFailAndTimeout(t *testing.T) {
	t.Run("502", func(t *testing.T) {
		s := openAIServer(t, 500, "x", nil)
		e := newEnv(t, map[string]config.Provider{"a": upstream("openai", s.URL), "b": upstream("openai", s.URL)}, []string{"a", "b"})
		resp := e.chat(chatBody)
		if resp.StatusCode != 502 || resp.Header.Get("X-LLMGate-Attempts") != "2" {
			t.Fatalf("status %d attempts %s", resp.StatusCode, resp.Header.Get("X-LLMGate-Attempts"))
		}
		if decodeErr(t, resp).Error.Code != "all_providers_failed" {
			t.Fatal("wrong code")
		}
		if e.metricValue("llmgate_requests_total", map[string]string{"status": "upstream_error"}) != 1 {
			t.Fatal("upstream_error not counted")
		}
	})
	t.Run("504", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body) // reading the body arms disconnect detection
			<-r.Context().Done()
		}))
		defer slow.Close()
		e := newEnv(t, map[string]config.Provider{"a": upstream("openai", slow.URL)}, []string{"a"}, withTimeout(50*time.Millisecond))
		resp := e.chat(chatBody)
		if resp.StatusCode != 504 || decodeErr(t, resp).Error.Type != "timeout" {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}

func TestBudgetExceeded(t *testing.T) {
	e := newEnv(t, map[string]config.Provider{"mock": mockProv()}, []string{"mock"},
		withKey(config.Key{Name: "demo", Key: testKey, TokensPerDay: 5}))
	if resp := e.chat(chatBody); resp.StatusCode != 200 {
		t.Fatalf("first call: %d", resp.StatusCode)
	}
	resp := e.chat(chatBody)
	if resp.StatusCode != 429 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	var secs int
	if _, err := fmt.Sscan(ra, &secs); err != nil || secs < 1 || secs > 86400 {
		t.Fatalf("Retry-After = %q", ra)
	}
	if got := decodeErr(t, resp).Error; got.Type != "budget_exceeded" {
		t.Fatalf("error %+v", got)
	}
	if e.metricValue("llmgate_requests_total", map[string]string{"status": "budget_exceeded"}) != 1 {
		t.Fatal("budget_exceeded not counted")
	}
}

func TestUSDBudgetExceeded(t *testing.T) {
	up := openAIServer(t, 200, okCompletion, nil) // costs 0.2 USD on m-cost
	e := newEnv(t, map[string]config.Provider{"cost": upstream("openai", up.URL)}, []string{"cost"},
		withKey(config.Key{Name: "demo", Key: testKey, USDPerDay: 0.1}))
	if resp := e.chat(chatBody); resp.StatusCode != 200 {
		t.Fatalf("first call: %d", resp.StatusCode)
	}
	resp := e.chat(chatBody)
	if resp.StatusCode != 429 || decodeErr(t, resp).Error.Type != "budget_exceeded" {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, map[string]config.Provider{"mock": mockProv()}, []string{"mock"},
		withKey(config.Key{Name: "demo", Key: testKey, RPM: 2}))
	for i := 0; i < 2; i++ {
		if resp := e.chat(chatBody); resp.StatusCode != 200 {
			t.Fatalf("call %d: %d", i, resp.StatusCode)
		}
	}
	resp := e.chat(chatBody)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if got := decodeErr(t, resp).Error; got.Type != "rate_limited" {
		t.Fatalf("error %+v", got)
	}
	if e.metricValue("llmgate_requests_total", map[string]string{"status": "rate_limited"}) != 1 {
		t.Fatal("rate_limited not counted")
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t, map[string]config.Provider{"mock": mockProv()}, []string{"mock"})
	tests := []struct {
		name, body string
		want       int
		code       string
	}{
		{"invalid json", `{`, 400, "invalid_request"},
		{"missing model", `{"messages":[{"role":"user","content":"x"}]}`, 400, "invalid_request"},
		{"empty messages", `{"model":"r","messages":[]}`, 400, "invalid_request"},
		{"unknown model", `{"model":"zzz","messages":[{"role":"user","content":"x"}]}`, 404, "model_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := e.chat(tt.body)
			if resp.StatusCode != tt.want || decodeErr(t, resp).Error.Code != tt.code {
				t.Fatalf("status %d", resp.StatusCode)
			}
		})
	}
}

func TestModelsHealthReadyMetrics(t *testing.T) {
	e := newEnv(t, map[string]config.Provider{"mock": mockProv()}, []string{"mock"})
	if resp := e.do("GET", "/v1/models", testKey, ""); !strings.Contains(readAll(t, resp), `"id":"r"`) {
		t.Fatal("models missing route r")
	}
	if resp := e.do("GET", "/healthz", "", ""); resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	if resp := e.do("GET", "/readyz", "", ""); resp.StatusCode != 200 {
		t.Fatalf("readyz %d", resp.StatusCode)
	}
	e.chat(chatBody)
	body := readAll(t, e.do("GET", "/metrics", "", ""))
	for _, name := range []string{
		"llmgate_requests_total", "llmgate_upstream_requests_total", "llmgate_request_duration_seconds",
		"llmgate_upstream_duration_seconds", "llmgate_tokens_total", "llmgate_circuit_state",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics missing %s", name)
		}
	}
	e.mr.Close()
	if resp := e.do("GET", "/readyz", "", ""); resp.StatusCode != 503 {
		t.Fatalf("readyz with redis down = %d", resp.StatusCode)
	}
}

func TestAdminMockFailToggleDemoFallback(t *testing.T) {
	e := newEnv(t, map[string]config.Provider{"mock-primary": mockProv(), "mock-secondary": mockProv()},
		[]string{"mock-primary", "mock-secondary"}, withAdmin)
	if resp := e.chat(chatBody); resp.Header.Get("X-LLMGate-Provider") != "mock-primary" {
		t.Fatalf("provider = %s", resp.Header.Get("X-LLMGate-Provider"))
	}
	if resp := e.do("POST", "/admin/mock/fail?provider=mock-primary&rate=1.0", "", ""); resp.StatusCode != 200 {
		t.Fatalf("admin: %d", resp.StatusCode)
	}
	resp := e.chat(chatBody)
	if resp.StatusCode != 200 || resp.Header.Get("X-LLMGate-Provider") != "mock-secondary" || resp.Header.Get("X-LLMGate-Attempts") != "2" {
		t.Fatalf("status=%d provider=%s attempts=%s", resp.StatusCode, resp.Header.Get("X-LLMGate-Provider"), resp.Header.Get("X-LLMGate-Attempts"))
	}
	if e.metricValue("llmgate_fallbacks_total", map[string]string{"from_provider": "mock-primary", "to_provider": "mock-secondary"}) != 1 {
		t.Fatal("fallback not counted")
	}
	for _, tt := range []struct {
		path string
		want int
	}{
		{"/admin/mock/fail?provider=ghost&rate=1", 404},
		{"/admin/mock/fail?rate=abc", 400},
		{"/admin/mock/fail?rate=2", 400},
		{"/admin/mock/fail?rate=0", 200}, // all mocks
	} {
		if resp := e.do("POST", tt.path, "", ""); resp.StatusCode != tt.want {
			t.Errorf("%s: %d, want %d", tt.path, resp.StatusCode, tt.want)
		}
	}
	if resp := e.chat(chatBody); resp.Header.Get("X-LLMGate-Provider") != "mock-primary" {
		t.Fatal("primary should serve again after rate=0")
	}
}

func TestAdminDisabledByDefault(t *testing.T) {
	e := newEnv(t, map[string]config.Provider{"mock": mockProv()}, []string{"mock"})
	if resp := e.do("POST", "/admin/mock/fail?rate=1", "", ""); resp.StatusCode != 404 {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

// ---- streaming ----

type sseClient struct {
	t  *testing.T
	sc *bufio.Scanner
}

func newSSE(t *testing.T, r *http.Response) *sseClient {
	return &sseClient{t: t, sc: bufio.NewScanner(r.Body)}
}

// next returns the next data payload or "" at end of stream.
func (c *sseClient) next() (string, bool) {
	for c.sc.Scan() {
		if d, ok := strings.CutPrefix(c.sc.Text(), "data: "); ok {
			return d, true
		}
	}
	return "", false
}

func (c *sseClient) all() []string {
	var out []string
	for {
		d, ok := c.next()
		if !ok {
			return out
		}
		out = append(out, d)
	}
}

func TestStreamMockEndToEnd(t *testing.T) {
	e := newEnv(t, map[string]config.Provider{"mock": mockProv()}, []string{"mock"})
	resp := e.chat(streamBody)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" ||
		resp.Header.Get("X-LLMGate-Provider") != "mock" {
		t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
	events := newSSE(t, resp).all()
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("stream must end with [DONE], got %v", events)
	}
	var text strings.Builder
	for _, ev := range events[:len(events)-1] {
		var c struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		if err := json.Unmarshal([]byte(ev), &c); err != nil {
			t.Fatalf("bad event %q", ev)
		}
		if len(c.Choices) == 0 {
			t.Fatalf("usage chunk leaked to client that did not ask for it: %s", ev)
		}
		text.WriteString(c.Choices[0].Delta.Content)
	}
	if text.String() != "[mock] echo: hello" {
		t.Fatalf("text %q", text.String())
	}
	if e.metricValue("llmgate_tokens_total", map[string]string{"direction": "output"}) == 0 {
		t.Fatal("stream tokens not accounted")
	}

	// include_usage: the usage chunk is delivered.
	resp = e.chat(`{"model":"r","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`)
	var sawUsage bool
	for _, ev := range newSSE(t, resp).all() {
		if strings.Contains(ev, `"usage":{`) {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatal("include_usage requested but no usage chunk")
	}
}

// The first chunk must reach the client while upstream is still producing:
// proves per-chunk flushing instead of buffering.
func TestStreamFlushesPerChunk(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	e := newEnv(t, map[string]config.Provider{"o": upstream("openai", up.URL)}, []string{"o"})
	resp := e.chat(streamBody)
	c := newSSE(t, resp)

	got := make(chan string, 1)
	go func() { d, _ := c.next(); got <- d }()
	select {
	case d := <-got:
		if !strings.Contains(d, "one") {
			t.Fatalf("first event %q", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk not delivered while upstream still open: response is buffered")
	}
	close(release)
	rest := c.all()
	if len(rest) != 2 || rest[1] != "[DONE]" {
		t.Fatalf("rest %v", rest)
	}
}

func TestStreamAnthropicTranslatedThroughGateway(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"type":"message_start","message":{"id":"m1","model":"claude","usage":{"input_tokens":10}}}`,
			`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`,
			`{"type":"content_block_delta","delta":{"type":"text_delta","text":" you"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`,
		} {
			fmt.Fprintf(w, "event: x\ndata: %s\n\n", ev)
			w.(http.Flusher).Flush()
		}
	}))
	defer up.Close()
	e := newEnv(t, map[string]config.Provider{"claude": upstream("anthropic", up.URL)}, []string{"claude"})
	events := newSSE(t, e.chat(streamBody)).all()
	var text strings.Builder
	var finish string
	for _, ev := range events[:len(events)-1] {
		var c struct {
			Object  string
			Choices []struct {
				Delta        struct{ Content string }
				FinishReason *string `json:"finish_reason"`
			}
		}
		_ = json.Unmarshal([]byte(ev), &c)
		if c.Object != "chat.completion.chunk" {
			t.Fatalf("event %s", ev)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if text.String() != "Hi you" || finish != "stop" || events[len(events)-1] != "[DONE]" {
		t.Fatalf("text=%q finish=%q events=%v", text.String(), finish, events)
	}
	if e.metricValue("llmgate_tokens_total", map[string]string{"direction": "input"}) != 10 ||
		e.metricValue("llmgate_tokens_total", map[string]string{"direction": "output"}) != 4 {
		t.Fatal("token accounting from anthropic usage events wrong")
	}
}

func TestStreamFallbackBeforeFirstByte(t *testing.T) {
	var hits atomic.Int32
	bad := openAIServer(t, 503, `{"error":{"message":"down"}}`, &hits)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer good.Close()
	e := newEnv(t, map[string]config.Provider{"bad": upstream("openai", bad.URL), "good": upstream("openai", good.URL)}, []string{"bad", "good"})
	resp := e.chat(streamBody)
	if resp.StatusCode != 200 || resp.Header.Get("X-LLMGate-Provider") != "good" || resp.Header.Get("X-LLMGate-Attempts") != "2" {
		t.Fatalf("status=%d provider=%s attempts=%s", resp.StatusCode, resp.Header.Get("X-LLMGate-Provider"), resp.Header.Get("X-LLMGate-Attempts"))
	}
	if ev := newSSE(t, resp).all(); len(ev) != 2 {
		t.Fatalf("events %v", ev)
	}
}

func TestStreamNoFallbackAfterFirstByte(t *testing.T) {
	var secondHits atomic.Int32
	// Sends one chunk, then closes without [DONE] (truncated stream).
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer broken.Close()
	second := openAIServer(t, 200, okCompletion, &secondHits)
	e := newEnv(t, map[string]config.Provider{"broken": upstream("openai", broken.URL), "second": upstream("openai", second.URL)}, []string{"broken", "second"})
	resp := e.chat(streamBody)
	if resp.StatusCode != 200 || resp.Header.Get("X-LLMGate-Provider") != "broken" {
		t.Fatalf("status=%d provider=%s", resp.StatusCode, resp.Header.Get("X-LLMGate-Provider"))
	}
	events := newSSE(t, resp).all()
	if len(events) != 2 || !strings.Contains(events[0], "partial") || !strings.Contains(events[1], "stream_interrupted") {
		t.Fatalf("events %v", events)
	}
	for _, ev := range events {
		if ev == "[DONE]" {
			t.Fatal("interrupted stream must not end with [DONE]")
		}
	}
	if secondHits.Load() != 0 {
		t.Fatal("fell back after first byte was sent")
	}
	if e.metricValue("llmgate_requests_total", map[string]string{"status": "upstream_error"}) != 1 {
		t.Fatal("mid-stream failure not recorded as upstream_error")
	}
}

func TestStreamClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer up.Close()
	e := newEnv(t, map[string]config.Provider{"o": upstream("openai", up.URL)}, []string{"o"})
	ctx, cancel := context.WithCancel(context.Background())
	resp := e.doCtx(ctx, "POST", "/v1/chat/completions", testKey, streamBody)
	if _, ok := newSSE(t, resp).next(); !ok {
		t.Fatal("no first event")
	}
	cancel() // client goes away mid-stream
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not cancelled after client disconnect")
	}
}
