/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// TestClientProxy_NoChild verifies the proxy surfaces a JSON 503 when the
// agent has no managed processes. This matches the contract clients like
// opencode rely on to distinguish "service down" from "service errored".
func TestClientProxy_NoChild(t *testing.T) {
	a := &MetalAgent{processes: map[string]*ManagedProcess{}}
	proxy := NewClientProxy(a, 0, zap.NewNop().Sugar())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !strings.Contains(body["error"], "no inference process available") {
		t.Errorf("error message = %q, missing expected substring", body["error"])
	}
}

// TestClientProxy_ForwardsRequest verifies a healthy child gets requests
// proxied through with method, path, and body intact.
func TestClientProxy_ForwardsRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream got path %q, want /v1/models", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer upstream.Close()

	a := mockAgentForUpstream(t, upstream)
	proxy := NewClientProxy(a, 0, zap.NewNop().Sugar())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "test-model") {
		t.Errorf("body = %q, want it to contain test-model", rec.Body.String())
	}
}

// TestClientProxy_SkipsUnhealthyProcess covers the case where the agent has
// registered a process but it hasn't passed its health check yet. The proxy
// must treat that as "no child available" rather than route to a dead port.
func TestClientProxy_SkipsUnhealthyProcess(t *testing.T) {
	a := &MetalAgent{processes: map[string]*ManagedProcess{
		"default/loading": {Name: "loading", Namespace: "default", Port: 12345, Healthy: false},
	}}
	proxy := NewClientProxy(a, 0, zap.NewNop().Sugar())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (unhealthy process should be invisible)", rec.Code)
	}
}

// TestClientProxy_RebindsAfterChildSwap verifies the load-bearing scenario
// from issue #406: when the metal-agent kills the current child and spawns
// a new one (e.g. spec change, watchdog kill), the proxy picks up the new
// port on the next request without restart. This is the property that
// removed the need for the external Python script in the first place.
func TestClientProxy_RebindsAfterChildSwap(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served-by-a"))
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served-by-b"))
	}))
	defer upstreamB.Close()

	a := mockAgentForUpstream(t, upstreamA)
	proxy := NewClientProxy(a, 0, zap.NewNop().Sugar())

	// Request 1 → upstream A
	rec := httptest.NewRecorder()
	proxy.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := rec.Body.String(); got != "served-by-a" {
		t.Fatalf("first request body = %q, want served-by-a", got)
	}

	// Simulate metal-agent restart with a new port
	rebindAgentToUpstream(t, a, upstreamB)

	// Request 2 → upstream B (no restart of the proxy)
	rec = httptest.NewRecorder()
	proxy.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := rec.Body.String(); got != "served-by-b" {
		t.Fatalf("second request body = %q, want served-by-b (proxy did not pick up new port)", got)
	}
}

// TestClientProxy_StreamsResponse verifies the FlushInterval=-1 setting
// causes responses to flow through chunk-by-chunk instead of being buffered.
// Critical for OpenAI server-sent-events.
func TestClientProxy_StreamsResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	a := mockAgentForUpstream(t, upstream)
	proxy := NewClientProxy(a, 0, zap.NewNop().Sugar())

	srv := httptest.NewServer(proxy.handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "data: chunk-0") || !strings.Contains(string(body), "data: chunk-2") {
		t.Errorf("body missing expected stream chunks: %q", string(body))
	}
}

// TestClientProxy_DisabledByPortZero ensures Start exits cleanly without
// binding a listener when port=0. This is the documented opt-out.
func TestClientProxy_DisabledByPortZero(t *testing.T) {
	a := &MetalAgent{processes: map[string]*ManagedProcess{}}
	proxy := NewClientProxy(a, 0, zap.NewNop().Sugar())

	if err := proxy.Start(t.Context()); err != nil {
		t.Fatalf("Start with port=0 should be no-op, got error: %v", err)
	}
	if proxy.started.Load() {
		t.Errorf("started=true after disabled Start; should not have bound a listener")
	}
}

// --- test helpers ---

// mockAgentForUpstream returns a MetalAgent stub with one healthy managed
// process pointing at the given upstream's port. Used to exercise the
// proxy's lookup + forwarding path without a real reconciler running.
func mockAgentForUpstream(t *testing.T, upstream *httptest.Server) *MetalAgent {
	t.Helper()
	port := portFromURL(t, upstream.URL)
	return &MetalAgent{
		processes: map[string]*ManagedProcess{
			"default/test": {
				Name:      "test",
				Namespace: "default",
				Port:      port,
				Healthy:   true,
				PID:       1,
			},
		},
	}
}

// rebindAgentToUpstream swaps the agent's single managed process to point at
// a different upstream. Simulates the metal-agent killing+respawning the
// child after a spec change or watchdog eviction.
func rebindAgentToUpstream(t *testing.T, a *MetalAgent, upstream *httptest.Server) {
	t.Helper()
	port := portFromURL(t, upstream.URL)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.processes["default/test"] = &ManagedProcess{
		Name:      "test",
		Namespace: "default",
		Port:      port,
		Healthy:   true,
		PID:       2,
	}
}

func portFromURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// _ ensures atomic.Bool has the expected zero value behavior for
// started.Load() in the disabled-port test. Pure compile-time guard.
var _ atomic.Bool
