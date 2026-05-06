/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// ClientProxy exposes a stable host-side HTTP listener that forwards
// OpenAI-compatible requests to whichever inference child the metal-agent
// is currently managing. Replaces the ad-hoc external Python script
// (vllm-swift-proxy.py) by letting the agent — which already owns the
// dynamic port — also own the user-facing endpoint.
//
// See issue defilantech/LLMKube#406 for the design rationale.
//
// Concurrency model:
//   - Each incoming request looks up the current child port via a single
//     RLock on the agent's process map. No long-held locks, no race against
//     the reconciler swapping processes mid-flight.
//   - Streaming responses (OpenAI chat completions with stream:true) flow
//     through httputil.ReverseProxy with FlushInterval=-1 so each chunk is
//     forwarded immediately rather than buffered.
//
// Out of scope (per #406):
//   - Multi-process routing by `model:` field. Single-host metal-agent
//     deployments today run one inference child at a time; if a future
//     spec creates a second, the proxy picks the first one returned by
//     the agent's lookup. Multi-routing is a follow-up.
//   - TLS, auth, rate limiting. Local host only.
type ClientProxy struct {
	agent  *MetalAgent
	port   int
	logger *zap.SugaredLogger
	server *http.Server

	// requestsTotal counts proxied requests by upstream HTTP status. Allows
	// Grafana to alert when 503 (no child) or 5xx upstream-failure rates
	// climb during a deploy/restart window.
	requestsTotal *prometheus.CounterVec

	// started is set once Start() has bound the listener; prevents the
	// shutdown path from calling Server.Shutdown on a never-started server.
	started atomic.Bool
}

// NewClientProxy constructs a ClientProxy. Call Start to bind the listener.
//
// `port` is the host-side TCP port the proxy listens on. 0 disables the
// proxy entirely (Start returns nil immediately) so users who don't need a
// stable host-side endpoint can opt out without a flag dance.
func NewClientProxy(metalAgent *MetalAgent, port int, logger *zap.SugaredLogger) *ClientProxy {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_client_proxy_requests_total",
			Help: "Total HTTP requests handled by the metal-agent client proxy, labeled by upstream response code.",
		},
		[]string{"status"},
	)
	// Best-effort registration. Duplicate registration (e.g. unit tests that
	// instantiate multiple proxies) is treated as recoverable: the existing
	// collector is reused.
	if err := AgentRegistry.Register(requests); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			requests = are.ExistingCollector.(*prometheus.CounterVec)
		}
	}

	return &ClientProxy{
		agent:         metalAgent,
		port:          port,
		logger:        logger.With("subsystem", "client-proxy"),
		requestsTotal: requests,
	}
}

// Start binds the listener and serves until ctx is canceled. Returns nil
// when port == 0 (proxy disabled). Returns the http.Server's listen error
// otherwise. Safe to call exactly once per ClientProxy.
func (p *ClientProxy) Start(ctx context.Context) error {
	if p.port == 0 {
		p.logger.Infow("client proxy disabled (port=0)")
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/", p.handler())

	p.server = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", p.port),
		Handler: mux,
		// Generous timeouts because OpenAI streaming chat completions can
		// legitimately stay open for minutes during long agentic loops.
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout: streaming responses set their own deadline via
		// the request context (typically minutes).
	}
	p.started.Store(true)

	// Hook ctx → graceful shutdown.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.server.Shutdown(shutdownCtx); err != nil {
			p.logger.Warnw("shutdown returned error", "error", err)
		}
	}()

	p.logger.Infow("client proxy listening", "addr", p.server.Addr)
	if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("client proxy listen: %w", err)
	}
	return nil
}

// handler returns the http.Handler that backs the proxy. Pulled out for
// testing without binding a real listener.
func (p *ClientProxy) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port, ok := p.currentInferencePort()
		if !ok {
			p.write503(w)
			return
		}

		target := &url.URL{
			Scheme: "http",
			Host:   "127.0.0.1:" + strconv.Itoa(port),
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		// FlushInterval=-1 forces immediate flush of every Write call from
		// the upstream. Required so OpenAI-style server-sent-events arrive
		// at the client byte-by-byte instead of being buffered. Without it,
		// streaming chat completions look like 30-second hangs followed by
		// a single dump.
		proxy.FlushInterval = -1
		// Capture upstream status for metrics. ErrorHandler fires when the
		// upstream is unreachable (e.g., child crashed mid-request); we
		// translate that to 502 so clients get a normal HTTP error, not a
		// hang or a connection reset.
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			p.logger.Warnw("upstream error", "target", target.Host, "error", err)
			p.requestsTotal.WithLabelValues("502").Inc()
			http.Error(w, "upstream inference process unreachable", http.StatusBadGateway)
		}
		proxy.ModifyResponse = func(resp *http.Response) error {
			p.requestsTotal.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()
			return nil
		}

		proxy.ServeHTTP(w, r)
	})
}

// write503 emits a JSON error body when no inference child is currently
// running. Mirrors the pattern the Python script used so existing client
// integrations (opencode, aider) can detect the condition the same way.
func (p *ClientProxy) write503(w http.ResponseWriter) {
	p.requestsTotal.WithLabelValues("503").Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "no inference process available; deploy an InferenceService first",
	})
}

// currentInferencePort returns the port of any currently managed inference
// process. Returns (0, false) when no process is running. The agent is
// expected to manage a single child at a time on Apple Silicon hosts;
// when there are multiple, this returns one deterministic by map iteration
// order — which is fine for the v1 single-child case but is the moral
// equivalent of a TODO for multi-routing.
func (p *ClientProxy) currentInferencePort() (int, bool) {
	if p.agent == nil {
		return 0, false
	}
	p.agent.mu.RLock()
	defer p.agent.mu.RUnlock()
	for _, proc := range p.agent.processes {
		if proc != nil && proc.Healthy && proc.Port > 0 {
			return proc.Port, true
		}
	}
	return 0, false
}
