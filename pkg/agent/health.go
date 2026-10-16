/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// ProcessHealthChecker checks whether an engine is healthy by probing path on
// its loopback port.
type ProcessHealthChecker interface {
	Check(ctx context.Context, port int, path string) (bool, error)
}

// healthProbePath returns the path the health monitor probes for a runtime.
// Every engine the agent starts serves /health except Ollama, which has no
// such route and answers it with 404; OllamaExecutor.isHealthy checks GET /
// for the same reason.
func healthProbePath(runtime string) string {
	if runtime == runtimeOllama {
		return "/"
	}
	return "/health"
}

// DefaultProcessHealthChecker probes an engine's health endpoint over HTTP.
type DefaultProcessHealthChecker struct {
	client http.Client
}

// NewDefaultProcessHealthChecker creates a health checker with the given timeout.
func NewDefaultProcessHealthChecker(timeout time.Duration) *DefaultProcessHealthChecker {
	return &DefaultProcessHealthChecker{
		client: http.Client{Timeout: timeout},
	}
}

// Check sends a GET request to path on 127.0.0.1:port and reports whether it
// answered 200.
func (c *DefaultProcessHealthChecker) Check(ctx context.Context, port int, path string) (bool, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		// Drain body to allow TCP connection reuse, capped at 64KB.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	return resp.StatusCode == http.StatusOK, nil
}

// HealthMonitor continuously polls managed processes for health.
type HealthMonitor struct {
	agent    *MetalAgent
	checker  ProcessHealthChecker
	interval time.Duration
	logger   *zap.SugaredLogger
}

// NewHealthMonitor creates a new health monitor.
func NewHealthMonitor(
	agent *MetalAgent, checker ProcessHealthChecker,
	interval time.Duration, logger *zap.SugaredLogger,
) *HealthMonitor {
	return &HealthMonitor{
		agent:    agent,
		checker:  checker,
		interval: interval,
		logger:   logger,
	}
}

// processSnapshot is a read-only copy of a managed process for health checking.
type processSnapshot struct {
	Key       string
	Name      string
	Namespace string
	Port      int
	Runtime   string
	// proc is the probed process itself, so a result that arrives after it
	// was replaced or stopped is not applied to whatever is tracked under
	// Key by then.
	proc *ManagedProcess
}

// Run starts the health monitor loop. It blocks until ctx is cancelled.
func (m *HealthMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

// checkAll performs one health check cycle across all managed processes.
func (m *HealthMonitor) checkAll(ctx context.Context) {
	// Snapshot processes under RLock
	m.agent.mu.RLock()
	snapshots := make([]processSnapshot, 0, len(m.agent.processes))
	for key, p := range m.agent.processes {
		snapshots = append(snapshots, processSnapshot{
			Key:       key,
			Name:      p.Name,
			Namespace: p.Namespace,
			Port:      p.Port,
			Runtime:   p.Runtime,
			proc:      p,
		})
	}
	m.agent.mu.RUnlock()

	// Update managed process count
	managedProcesses.Set(float64(len(snapshots)))

	threshold := m.agent.healthFailureThreshold()
	for _, snap := range snapshots {
		if ctx.Err() != nil {
			return
		}

		start := time.Now()
		healthy, err := m.checker.Check(ctx, snap.Port, healthProbePath(snap.Runtime))
		duration := time.Since(start).Seconds()
		healthCheckDuration.WithLabelValues(snap.Name, snap.Namespace).Observe(duration)
		if ctx.Err() != nil {
			// Cut short by shutdown: says nothing about the engine.
			return
		}

		if err != nil {
			healthy = false
			m.logger.Warnw("health check error", "name", snap.Name, "namespace", snap.Namespace, "error", err)
		}

		m.applyResult(ctx, snap, healthy, threshold)
	}
}

// applyResult records one probe result on the process it was taken from and
// acts on it: the first failure withdraws the endpoint, threshold consecutive
// failures stop the engine for a restart, and a success after failures
// re-registers the endpoint. An engine that is still loading is never probed:
// the agent tracks a process only after its executor's StartProcess returns,
// and every executor waits there until the engine is ready to serve.
func (m *HealthMonitor) applyResult(ctx context.Context, snap processSnapshot, healthy bool, threshold int) {
	a := m.agent
	a.mu.Lock()
	if a.processes[snap.Key] != snap.proc {
		// Replaced or stopped while the probe ran; the result says nothing
		// about the process tracked now.
		a.mu.Unlock()
		return
	}
	wasHealthy := snap.proc.Healthy
	snap.proc.Healthy = healthy
	if healthy {
		snap.proc.healthFailures = 0
		a.resetRestartBackoffLocked(snap.Key, snap.proc)
	} else {
		snap.proc.healthFailures++
	}
	failures := snap.proc.healthFailures
	a.mu.Unlock()

	switch {
	case wasHealthy && !healthy:
		m.logger.Warnw("process became unhealthy",
			"name", snap.Name, "namespace", snap.Namespace, "restartAfterFailures", threshold)
		processHealthy.WithLabelValues(snap.Name, snap.Namespace).Set(0)
		// Immediately withdraw the endpoint so kube-proxy stops DNATing
		// to the dead backend (#662).
		a.withdrawEndpoint(ctx, snap.Name, snap.Namespace)
	case !wasHealthy && healthy:
		m.logger.Infow("process recovered",
			"name", snap.Name, "namespace", snap.Namespace)
		processHealthy.WithLabelValues(snap.Name, snap.Namespace).Set(1)
		// Immediately re-register the endpoint so kube-proxy resumes
		// routing to the recovered backend (#662).
		a.registerEndpoint(ctx, snap.Key, snap.proc)
	}

	if !healthy && failures >= threshold {
		a.restartUnhealthy(ctx, snap.Key, snap.proc, failures)
	}
}

// HealthServer serves /healthz, /readyz, and /metrics for the Metal agent.
type HealthServer struct {
	agent  *MetalAgent
	port   int
	logger *zap.SugaredLogger
}

// NewHealthServer creates a new health server.
func NewHealthServer(agent *MetalAgent, port int, logger *zap.SugaredLogger) *HealthServer {
	return &HealthServer{
		agent:  agent,
		port:   port,
		logger: logger,
	}
}

// Handler returns the HTTP handler for the health server (useful for testing).
func (s *HealthServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(AgentRegistry, promhttp.HandlerOpts{}))
	return mux
}

// Run starts the HTTP server. It blocks until ctx is cancelled.
func (s *HealthServer) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 15, // 32KB
	}

	// G118 false positive on the shutdown goroutine below: the parent ctx
	// is already Done when the shutdown runs (we just waited on it), so we
	// cannot use it for the shutdown deadline. The independent 5s timeout
	// is the correct bound.
	go func() { //nolint:gosec // G118: see comment above
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Warnw("health server shutdown error", "error", err)
		}
	}()

	s.logger.Infow("starting health server", "port", s.port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("health server failed: %w", err)
	}
	return nil
}

// handleHealthz is a liveness probe — always returns 200.
func (s *HealthServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz is a readiness probe — returns 200 if no processes or at
// least one healthy process, 503 if all processes are unhealthy.
func (s *HealthServer) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	s.agent.mu.RLock()
	defer s.agent.mu.RUnlock()

	if len(s.agent.processes) == 0 {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
		return
	}

	for _, p := range s.agent.processes {
		if p.Healthy {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("not ready"))
}
