package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The dashboard depends on these exact names.
func TestContractNamesExposed(t *testing.T) {
	m := New()
	m.Requests.WithLabelValues("k", "r", StatusOK).Inc()
	m.UpstreamRequests.WithLabelValues("p", "m", OutcomeOK).Inc()
	m.RequestDuration.WithLabelValues("r", "false").Observe(0.1)
	m.UpstreamDuration.WithLabelValues("p", "m").Observe(0.1)
	m.TTFB.WithLabelValues("p").Observe(0.1)
	m.Tokens.WithLabelValues("k", "p", "m", "input").Add(1)
	m.CostUSD.WithLabelValues("k", "p", "m").Add(0.1)
	m.Fallbacks.WithLabelValues("a", "b").Inc()
	m.CircuitState.WithLabelValues("p").Set(2)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, name := range []string{
		"llmgate_requests_total", "llmgate_upstream_requests_total", "llmgate_request_duration_seconds_bucket",
		"llmgate_upstream_duration_seconds_bucket", "llmgate_time_to_first_byte_seconds_bucket", "llmgate_tokens_total",
		"llmgate_cost_usd_total", "llmgate_fallbacks_total", "llmgate_circuit_state",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("missing %s", name)
		}
	}
}
