package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// The metrics endpoint must serve the registry the llmkube collectors are
// actually registered in.
//
// This is a regression test for a silent failure that shipped once already.
// promhttp.Handler() serves prometheus.DefaultGatherer, while internal/metrics
// registers every collector into controller-runtime's registry. Wiring the
// wrong one still returns HTTP 200, with roughly 38 families of Go runtime and
// process metrics, so the scrape succeeds, the PodMonitor reports a healthy
// target, and the dashboards are just empty. Nothing logs an error, which is
// why it survived review and a merge.
//
// Asserting on a real series rather than a synthetic collector is deliberate:
// it pins the contract (this endpoint exposes the llmkube collectors) rather
// than the mechanism, so the test still means something if registration is
// refactored.
func TestMetricsHandlerServesLLMKubeCollectors(t *testing.T) {
	// Load-bearing: a labelled collector emits no family until it has at least
	// one child series, so an idle scrape is byte-identical under either
	// registry. Without touching a counter first, this test passes on the bug.
	prommetrics.RouterRequestsTotal.WithLabelValues(
		"test-router", "test-rule", "test-backend", "test-class", "success").Inc()

	rec := httptest.NewRecorder()
	newMetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "llmkube_router_requests_total") {
		t.Errorf("metrics endpoint does not expose llmkube_router_requests_total; "+
			"it is serving the wrong registry. Got %d bytes beginning: %.200s",
			len(body), body)
	}
}
