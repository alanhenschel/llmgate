// Package metrics defines the Prometheus collectors exposed on /metrics.
// Metric names are a contract with the Grafana dashboard in deploy/.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Request statuses for llmgate_requests_total.
const (
	StatusOK             = "ok"
	StatusClientError    = "client_error"
	StatusBudgetExceeded = "budget_exceeded"
	StatusRateLimited    = "rate_limited"
	StatusUpstreamError  = "upstream_error"
)

// Upstream outcomes for llmgate_upstream_requests_total.
const (
	OutcomeOK          = "ok"
	OutcomeError       = "error"
	OutcomeTimeout     = "timeout"
	OutcomeSkippedOpen = "skipped_open"
)

// Metrics bundles all collectors on a private registry.
type Metrics struct {
	Registry *prometheus.Registry

	Requests         *prometheus.CounterVec
	UpstreamRequests *prometheus.CounterVec
	RequestDuration  *prometheus.HistogramVec
	UpstreamDuration *prometheus.HistogramVec
	TTFB             *prometheus.HistogramVec
	Tokens           *prometheus.CounterVec
	CostUSD          *prometheus.CounterVec
	Fallbacks        *prometheus.CounterVec
	CircuitState     *prometheus.GaugeVec
}

// New registers all collectors on a fresh registry.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_requests_total", Help: "Gateway requests by final status.",
		}, []string{"key", "route", "status"}),
		UpstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_upstream_requests_total", Help: "Upstream attempts by outcome.",
		}, []string{"provider", "model", "outcome"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgate_request_duration_seconds", Help: "Total request duration including fallbacks.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
		}, []string{"route", "stream"}),
		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgate_upstream_duration_seconds", Help: "Duration of a single upstream attempt.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
		}, []string{"provider", "model"}),
		TTFB: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgate_time_to_first_byte_seconds", Help: "Time to first streamed chunk from upstream.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
		}, []string{"provider"}),
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_tokens_total", Help: "Tokens processed.",
		}, []string{"key", "provider", "model", "direction"}),
		CostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_cost_usd_total", Help: "Accumulated cost in USD.",
		}, []string{"key", "provider", "model"}),
		Fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_fallbacks_total", Help: "Fallback hops between providers.",
		}, []string{"from_provider", "to_provider"}),
		CircuitState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llmgate_circuit_state", Help: "Circuit breaker state: 0 closed, 1 half-open, 2 open.",
		}, []string{"provider"}),
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.Requests, m.UpstreamRequests, m.RequestDuration, m.UpstreamDuration, m.TTFB,
		m.Tokens, m.CostUSD, m.Fallbacks, m.CircuitState,
	)
	return m
}

// Handler serves the registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
