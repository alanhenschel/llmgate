package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/alanhenschel/llmgate/internal/budget"
	"github.com/alanhenschel/llmgate/internal/config"
	"github.com/alanhenschel/llmgate/internal/metrics"
	"github.com/alanhenschel/llmgate/internal/pricing"
	"github.com/alanhenschel/llmgate/internal/provider"
	"github.com/alanhenschel/llmgate/internal/router"
)

// call collects what is needed to emit metrics and the access log once.
type call struct {
	key        string
	route      string
	stream     bool
	status     string // metrics status label
	httpStatus int
	provider   string
	model      string
	attempts   int
	usage      provider.Usage
	costUSD    float64
	err        error
	start      time.Time
}

func (s *Server) finish(w http.ResponseWriter, c *call) {
	route := c.route
	if route != "" && !s.Router.HasRoute(route) {
		route = "unknown" // keep label cardinality bounded
	}
	s.Metrics.Requests.WithLabelValues(c.key, route, c.status).Inc()
	s.Metrics.RequestDuration.WithLabelValues(route, strconv.FormatBool(c.stream)).Observe(time.Since(c.start).Seconds())
	attrs := []any{
		"request_id", w.Header().Get(hdrRequestID), "key", c.key, "route", c.route, "stream", c.stream,
		"status", c.status, "http_status", c.httpStatus, "provider", c.provider, "model", c.model,
		"attempts", c.attempts, "input_tokens", c.usage.PromptTokens, "output_tokens", c.usage.CompletionTokens,
		"cost_usd", c.costUSD, "duration_ms", time.Since(c.start).Milliseconds(),
	}
	if c.err != nil {
		attrs = append(attrs, "error", c.err.Error())
		s.Log.Warn("request", attrs...)
		return
	}
	s.Log.Info("request", attrs...)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	c := &call{start: time.Now(), key: "unknown", status: metrics.StatusClientError}
	key, ok := s.authenticate(r)
	if !ok {
		s.unauthorized(w)
		return
	}
	c.key = key.Name
	defer s.finish(w, c)

	cr, err := parseChatRequest(r, w)
	if err != nil {
		c.httpStatus, c.err = http.StatusBadRequest, err
		writeError(w, c.httpStatus, "invalid_request_error", "invalid_request", err.Error())
		return
	}
	c.route, c.stream = cr.Model, cr.Stream

	if err := s.Limiter.AllowRequest(r.Context(), key.Name, key.RPM); err != nil {
		if s.limited(w, c, err) {
			return
		}
		s.Log.Error("rate limiter unavailable, failing open", "key", key.Name, "error", err)
	}
	limits := budget.Limits{TokensPerDay: key.TokensPerDay, USDPerDay: key.USDPerDay, RPM: key.RPM}
	if err := s.Limiter.CheckBudget(r.Context(), key.Name, limits); err != nil {
		if s.limited(w, c, err) {
			return
		}
		s.Log.Error("budget store unavailable, failing open", "key", key.Name, "error", err)
	}
	if !s.Router.HasRoute(cr.Model) {
		c.httpStatus, c.err = http.StatusNotFound, router.ErrUnknownRoute
		writeError(w, c.httpStatus, "invalid_request_error", "model_not_found", "unknown model/route "+strconv.Quote(cr.Model))
		return
	}

	if cr.Stream {
		s.serveStream(w, r, key, cr, c)
	} else {
		s.serveChat(w, r, key, cr, c)
	}
}

// limited writes a 429 if err is a quota rejection and reports whether it did.
func (s *Server) limited(w http.ResponseWriter, c *call, err error) bool {
	var le *budget.LimitError
	if !errors.As(err, &le) {
		return false
	}
	typ, code, status := "rate_limited", "rate_limit_exceeded", metrics.StatusRateLimited
	if errors.Is(err, budget.ErrBudgetExceeded) {
		typ, code, status = "budget_exceeded", "budget_exceeded", metrics.StatusBudgetExceeded
	}
	c.status, c.httpStatus, c.err = status, http.StatusTooManyRequests, err
	setRetryAfter(w, le.RetryAfter)
	writeError(w, c.httpStatus, typ, code, err.Error())
	return true
}

func (s *Server) serveChat(w http.ResponseWriter, r *http.Request, key *config.Key, cr *chatRequest, c *call) {
	res, err := s.Router.Chat(r.Context(), cr.Model, cr.Req)
	if err != nil {
		s.upstreamFailed(w, c, err)
		return
	}
	c.status, c.httpStatus = metrics.StatusOK, http.StatusOK
	c.provider, c.model, c.attempts = res.Provider, res.Model, res.Attempts
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set(hdrProvider, res.Provider)
	h.Set(hdrAttempts, strconv.Itoa(res.Attempts))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Response.Body)
	s.account(r, key, c, res.Response.Usage, cr.Req)
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, key *config.Key, cr *chatRequest, c *call) {
	res, err := s.Router.Stream(r.Context(), cr.Model, cr.Req)
	if err != nil {
		s.upstreamFailed(w, c, err)
		return
	}
	defer res.Stream.Close()
	c.provider, c.model, c.attempts = res.Provider, res.Model, res.Attempts
	c.status, c.httpStatus = metrics.StatusOK, http.StatusOK

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set(hdrProvider, res.Provider)
	h.Set(hdrAttempts, strconv.Itoa(res.Attempts))
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	var buf []byte
	send := func(data []byte) error {
		buf = append(append(append(buf[:0], "data: "...), data...), '\n', '\n')
		if _, err := w.Write(buf); err != nil {
			return err
		}
		return rc.Flush()
	}

	for {
		chunk, err := res.Stream.Recv()
		if errors.Is(err, io.EOF) {
			if err := send([]byte("[DONE]")); err != nil {
				c.err = err
			}
			break
		}
		if err != nil {
			c.err = err
			if r.Context().Err() == nil { // upstream broke mid-stream: tell the client, then close
				c.status = metrics.StatusUpstreamError
				msg, _ := json.Marshal(errorBody{Error: errorDetail{Message: "upstream stream interrupted", Type: "upstream_error", Code: "stream_interrupted"}})
				_ = send(msg)
			}
			break
		}
		if chunk.UsageOnly && !cr.IncludeUsage {
			continue
		}
		if err := send(chunk.Data); err != nil { // client gone; deferred Close cancels upstream
			c.err = err
			break
		}
	}
	s.account(r, key, c, res.Stream.Usage(), cr.Req)
}

// upstreamFailed maps a router failure to an OpenAI-shaped HTTP error.
func (s *Server) upstreamFailed(w http.ResponseWriter, c *call, err error) {
	c.err = err
	var f *router.Failure
	if errors.As(err, &f) {
		c.attempts = f.Attempts
		w.Header().Set(hdrAttempts, strconv.Itoa(f.Attempts))
	}
	var se *provider.StatusError
	switch {
	case errors.Is(err, context.Canceled):
		c.status, c.httpStatus = metrics.StatusClientError, 499 // client closed request (nginx convention); nothing is written
	case !errors.Is(err, router.ErrAllFailed) && errors.As(err, &se):
		// Non-retryable upstream rejection (4xx): hand it to the client.
		status := se.Status
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			status = http.StatusBadGateway // gateway-side credential problem, not the client's
		}
		c.status, c.httpStatus = metrics.StatusClientError, status
		writeError(w, status, "invalid_request_error", "upstream_rejected", se.Message)
	case f != nil && f.Timeout:
		c.status, c.httpStatus = metrics.StatusUpstreamError, http.StatusGatewayTimeout
		writeError(w, c.httpStatus, "timeout", "upstream_timeout", "upstream provider timed out")
	default:
		c.status, c.httpStatus = metrics.StatusUpstreamError, http.StatusBadGateway
		writeError(w, c.httpStatus, "upstream_error", "all_providers_failed", "all upstream providers failed")
	}
}

// account records tokens, cost and metrics, and charges the key's budget.
// It uses a detached context so usage is still booked after a client hang-up.
func (s *Server) account(r *http.Request, key *config.Key, c *call, u provider.Usage, req *provider.Request) {
	if u.PromptTokens == 0 {
		u.PromptTokens = provider.EstimateTokens(req.PromptChars())
	}
	c.usage = u
	c.costUSD = s.Prices.Cost(c.model, u.PromptTokens, u.CompletionTokens)
	s.Metrics.Tokens.WithLabelValues(key.Name, c.provider, c.model, "input").Add(float64(u.PromptTokens))
	s.Metrics.Tokens.WithLabelValues(key.Name, c.provider, c.model, "output").Add(float64(u.CompletionTokens))
	s.Metrics.CostUSD.WithLabelValues(key.Name, c.provider, c.model).Add(c.costUSD)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	total := int64(u.PromptTokens + u.CompletionTokens)
	if err := s.Limiter.Record(ctx, key.Name, total, pricing.Micros(c.costUSD)); err != nil {
		s.Log.Error("failed to record usage", "key", key.Name, "error", err)
	}
}
