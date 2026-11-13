// Package api implements the HTTP surface of the gateway.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alanhenschel/llmgate/internal/budget"
	"github.com/alanhenschel/llmgate/internal/config"
	"github.com/alanhenschel/llmgate/internal/metrics"
	"github.com/alanhenschel/llmgate/internal/pricing"
	"github.com/alanhenschel/llmgate/internal/provider"
	"github.com/alanhenschel/llmgate/internal/router"
)

const (
	maxBodyBytes = 8 << 20
	hdrProvider  = "X-LLMGate-Provider"
	hdrAttempts  = "X-LLMGate-Attempts"
	hdrRequestID = "X-Request-ID"
)

// Limiter is the quota backend (Redis in production).
type Limiter interface {
	AllowRequest(ctx context.Context, key string, rpm int) error
	CheckBudget(ctx context.Context, key string, l budget.Limits) error
	Record(ctx context.Context, key string, tokens, usdMicros int64) error
	Ping(ctx context.Context) error
}

// Deps are the collaborators of a Server.
type Deps struct {
	Config  *config.Config
	Router  *router.Router
	Limiter Limiter
	Metrics *metrics.Metrics
	Prices  *pricing.Table
	Mocks   map[string]*provider.Mock // by instance name, for the admin toggle
	Log     *slog.Logger
}

// Server is the gateway HTTP handler.
type Server struct {
	Deps
	keys []keyEntry
}

type keyEntry struct {
	hash [sha256.Size]byte
	key  config.Key
}

// New builds a Server.
func New(d Deps) *Server {
	s := &Server{Deps: d}
	for _, k := range d.Config.Keys {
		s.keys = append(s.keys, keyEntry{hash: sha256.Sum256([]byte(k.Key)), key: k})
	}
	return s
}

// Handler returns the routed handler with request-id and panic-recovery middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /metrics", s.Metrics.Handler())
	if s.Config.Admin {
		mux.HandleFunc("POST /admin/mock/fail", s.handleMockFail)
	}
	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(hdrRequestID)
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set(hdrRequestID, id)
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic in handler", "request_id", id, "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// authenticate resolves the bearer token against the configured keys in
// constant time.
func (s *Server) authenticate(r *http.Request) (*config.Key, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, false
	}
	h := sha256.Sum256([]byte(tok))
	var found *config.Key
	for i := range s.keys {
		if subtle.ConstantTimeCompare(h[:], s.keys[i].hash[:]) == 1 {
			found = &s.keys[i].key
		}
	}
	return found, found != nil
}

func (s *Server) unauthorized(w http.ResponseWriter) {
	s.Metrics.Requests.WithLabelValues("unknown", "", metrics.StatusClientError).Inc()
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "missing or invalid API key")
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(r); !ok {
		s.unauthorized(w)
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	routes := s.Router.Routes()
	data := make([]model, 0, len(routes))
	for _, name := range routes {
		data = append(data, model{ID: name, Object: "model", OwnedBy: "llmgate"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Limiter.Ping(ctx); err != nil {
		s.Log.Warn("readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "redis unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMockFail implements POST /admin/mock/fail?provider=<name>&rate=<0..1>.
// Without provider every mock instance is affected.
func (s *Server) handleMockFail(w http.ResponseWriter, r *http.Request) {
	rate, err := strconv.ParseFloat(r.URL.Query().Get("rate"), 64)
	if err != nil || rate < 0 || rate > 1 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_rate", "rate must be a number in [0,1]")
		return
	}
	name := r.URL.Query().Get("provider")
	targets := map[string]*provider.Mock{}
	if name == "" {
		targets = s.Mocks
	} else if m, ok := s.Mocks[name]; ok {
		targets[name] = m
	} else {
		writeError(w, http.StatusNotFound, "invalid_request_error", "unknown_provider", "no mock provider named "+name)
		return
	}
	updated := map[string]float64{}
	for n, m := range targets {
		m.SetFailRate(rate)
		updated[n] = rate
	}
	s.Log.Warn("mock fail rate changed", "providers", updated)
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
}

// chatRequest is the parsed client request.
type chatRequest struct {
	Model        string
	Stream       bool
	IncludeUsage bool
	Req          *provider.Request
}

func parseChatRequest(r *http.Request, w http.ResponseWriter) (*chatRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&raw); err != nil {
		return nil, errors.New("request body is not valid JSON: " + err.Error())
	}
	var c chatRequest
	if err := json.Unmarshal(raw["model"], &c.Model); err != nil || c.Model == "" {
		return nil, errors.New("'model' is required")
	}
	var msgs []provider.Message
	if err := json.Unmarshal(raw["messages"], &msgs); err != nil || len(msgs) == 0 {
		return nil, errors.New("'messages' must be a non-empty array")
	}
	if v, ok := raw["stream"]; ok {
		if err := json.Unmarshal(v, &c.Stream); err != nil {
			return nil, errors.New("'stream' must be a boolean")
		}
	}
	var so struct {
		IncludeUsage bool `json:"include_usage"`
	}
	if v, ok := raw["stream_options"]; ok {
		_ = json.Unmarshal(v, &so)
	}
	c.IncludeUsage = so.IncludeUsage
	c.Req = &provider.Request{Model: c.Model, Stream: c.Stream, Messages: msgs, Raw: raw}
	return &c, nil
}
