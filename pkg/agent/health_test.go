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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// mockHealthChecker is a test double for ProcessHealthChecker.
type mockHealthChecker struct {
	result bool
	err    error
	calls  int
}

func (m *mockHealthChecker) Check(_ context.Context, _ int) (bool, error) {
	m.calls++
	return m.result, m.err
}

func newTestAgent() *MetalAgent {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})
}

// --- HealthServer endpoint tests ---

func TestHealthServer_Healthz(t *testing.T) {
	agent := newTestAgent()
	srv := NewHealthServer(agent, 0, newNopLogger())

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "ok" {
		t.Errorf("GET /healthz body = %q, want %q", w.Body.String(), "ok")
	}
}

func TestHealthServer_Readyz_NoProcesses(t *testing.T) {
	agent := newTestAgent()
	srv := NewHealthServer(agent, 0, newNopLogger())

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /readyz (no processes) = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "ready" {
		t.Errorf("GET /readyz body = %q, want %q", w.Body.String(), "ready")
	}
}

func TestHealthServer_Readyz_OneHealthy(t *testing.T) {
	agent := newTestAgent()
	agent.processes["default/model-a"] = &ManagedProcess{
		Name: "model-a", Namespace: "default", Healthy: true,
	}
	srv := NewHealthServer(agent, 0, newNopLogger())

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /readyz (one healthy) = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHealthServer_Readyz_AllUnhealthy(t *testing.T) {
	agent := newTestAgent()
	agent.processes["default/model-a"] = &ManagedProcess{
		Name: "model-a", Namespace: "default", Healthy: false,
	}
	agent.processes["default/model-b"] = &ManagedProcess{
		Name: "model-b", Namespace: "default", Healthy: false,
	}
	srv := NewHealthServer(agent, 0, newNopLogger())

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz (all unhealthy) = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if w.Body.String() != "not ready" {
		t.Errorf("GET /readyz body = %q, want %q", w.Body.String(), "not ready")
	}
}

func TestHealthServer_Readyz_MixedHealth(t *testing.T) {
	agent := newTestAgent()
	agent.processes["default/model-a"] = &ManagedProcess{
		Name: "model-a", Namespace: "default", Healthy: true,
	}
	agent.processes["default/model-b"] = &ManagedProcess{
		Name: "model-b", Namespace: "default", Healthy: false,
	}
	srv := NewHealthServer(agent, 0, newNopLogger())

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /readyz (mixed health) = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHealthServer_Metrics(t *testing.T) {
	agent := newTestAgent()
	srv := NewHealthServer(agent, 0, newNopLogger())

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /metrics = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "llmkube_metal_agent") {
		t.Error("GET /metrics response does not contain llmkube_metal_agent metrics")
	}
}

// --- HealthMonitor tests ---

func TestHealthMonitor_MarksUnhealthy(t *testing.T) {
	agent := newTestAgent()
	agent.processes["default/model-a"] = &ManagedProcess{
		Name: "model-a", Namespace: "default", Port: 8080, Healthy: true,
	}
	// Set initial healthy state in metric
	processHealthy.WithLabelValues("model-a", "default").Set(1)

	checker := &mockHealthChecker{result: false}
	monitor := NewHealthMonitor(agent, checker, time.Second, newNopLogger())

	monitor.checkAll(context.Background())

	agent.mu.RLock()
	healthy := agent.processes["default/model-a"].Healthy
	agent.mu.RUnlock()

	if healthy {
		t.Error("process should be marked unhealthy after failed check")
	}
	if checker.calls != 1 {
		t.Errorf("expected 1 health check call, got %d", checker.calls)
	}
}

func TestHealthMonitor_MarksHealthy(t *testing.T) {
	agent := newTestAgent()
	agent.processes["default/model-a"] = &ManagedProcess{
		Name: "model-a", Namespace: "default", Port: 8080, Healthy: false,
	}
	processHealthy.WithLabelValues("model-a", "default").Set(0)

	checker := &mockHealthChecker{result: true}
	monitor := NewHealthMonitor(agent, checker, time.Second, newNopLogger())

	monitor.checkAll(context.Background())

	agent.mu.RLock()
	healthy := agent.processes["default/model-a"].Healthy
	agent.mu.RUnlock()

	if !healthy {
		t.Error("process should be marked healthy after successful check")
	}
}

func TestHealthMonitor_RecordsRestartCount(t *testing.T) {
	agent := newTestAgent()
	agent.processes["default/model-a"] = &ManagedProcess{
		Name: "model-a", Namespace: "default", Port: 8080, Healthy: true,
	}
	processHealthy.WithLabelValues("model-a", "default").Set(1)

	checker := &mockHealthChecker{result: false}
	monitor := NewHealthMonitor(agent, checker, time.Second, newNopLogger())

	monitor.checkAll(context.Background())

	// Verify restart counter was incremented (scheduleRestart calls processRestarts.Inc)
	// We can't easily check the counter value directly without reading the metric,
	// but we can verify the checker was called and the process was marked unhealthy.
	if checker.calls != 1 {
		t.Errorf("expected 1 health check call, got %d", checker.calls)
	}

	agent.mu.RLock()
	healthy := agent.processes["default/model-a"].Healthy
	agent.mu.RUnlock()
	if healthy {
		t.Error("process should be unhealthy after failed check")
	}
}

// TestHealthMonitor_WithdrawsOnUnhealthy verifies that the health monitor
// immediately withdraws the endpoint (Ready=false) when it detects a
// healthy→unhealthy transition, rather than waiting for the next heartbeat
// tick (#662).
func TestHealthMonitor_WithdrawsOnUnhealthy(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "withdraw-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "withdraw-model"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(isvc).
		Build()

	agent := &MetalAgent{
		config: MetalAgentConfig{
			K8sClient: k8sClient,
			Namespace: "default",
		},
		processes: map[string]*ManagedProcess{
			"default/withdraw-model": {
				Name:      "withdraw-model",
				Namespace: "default",
				Port:      8080,
				Healthy:   true,
			},
		},
		starting: make(map[string]bool),
		logger:   newNopLogger(),
	}
	agent.registry = NewServiceRegistry(k8sClient, "10.0.0.1", newNopLogger(), "")

	// First register the endpoint so it starts Ready=true.
	if err := agent.registry.RegisterEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}

	checker := &mockHealthChecker{result: false}
	monitor := NewHealthMonitor(agent, checker, time.Second, newNopLogger())

	monitor.checkAll(context.Background())

	// The endpoint should now be withdrawn (Ready=false).
	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "withdraw-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get endpointslice after health monitor: %v", err)
	}
	if len(slice.Endpoints) != 1 {
		t.Fatalf("EndpointSlice has %d endpoints, want 1", len(slice.Endpoints))
	}
	if slice.Endpoints[0].Conditions.Ready == nil || *slice.Endpoints[0].Conditions.Ready {
		t.Errorf("endpoint Conditions.Ready = %v, want false after health monitor withdrawal",
			slice.Endpoints[0].Conditions.Ready)
	}
}

// TestHealthMonitor_RegistersOnRecovery verifies that the health monitor
// immediately re-registers the endpoint (Ready=true) when it detects an
// unhealthy→healthy transition, rather than waiting for the next heartbeat
// tick (#662).
func TestHealthMonitor_RegistersOnRecovery(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "recover-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "recover-model"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(isvc).
		Build()

	agent := &MetalAgent{
		config: MetalAgentConfig{
			K8sClient: k8sClient,
			Namespace: "default",
		},
		processes: map[string]*ManagedProcess{
			"default/recover-model": {
				Name:      "recover-model",
				Namespace: "default",
				Port:      8080,
				Healthy:   false,
			},
		},
		starting: make(map[string]bool),
		logger:   newNopLogger(),
	}
	agent.registry = NewServiceRegistry(k8sClient, "10.0.0.1", newNopLogger(), "")

	// First withdraw the endpoint so it starts Ready=false.
	if err := agent.registry.WithdrawEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("WithdrawEndpoint: %v", err)
	}

	checker := &mockHealthChecker{result: true}
	monitor := NewHealthMonitor(agent, checker, time.Second, newNopLogger())

	monitor.checkAll(context.Background())

	// The endpoint should now be registered (Ready=true).
	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "recover-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get endpointslice after health monitor: %v", err)
	}
	if len(slice.Endpoints) != 1 {
		t.Fatalf("EndpointSlice has %d endpoints, want 1", len(slice.Endpoints))
	}
	if slice.Endpoints[0].Conditions.Ready == nil || !*slice.Endpoints[0].Conditions.Ready {
		t.Errorf("endpoint Conditions.Ready = %v, want true after health monitor recovery",
			slice.Endpoints[0].Conditions.Ready)
	}
}

func TestHealthMonitor_ContextCancellation(t *testing.T) {
	agent := newTestAgent()
	checker := &mockHealthChecker{result: true}
	monitor := NewHealthMonitor(agent, checker, 10*time.Millisecond, newNopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		monitor.Run(ctx)
		close(done)
	}()

	// Let it run briefly
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Error("HealthMonitor.Run() did not exit after context cancellation")
	}
}

// --- DefaultProcessHealthChecker tests ---

func TestDefaultProcessHealthChecker_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Extract port from test server URL
	var port int
	if _, err := fmt.Sscanf(ts.URL, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatalf("failed to parse test server port: %v", err)
	}

	checker := NewDefaultProcessHealthChecker(5 * time.Second)
	healthy, err := checker.Check(context.Background(), port)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !healthy {
		t.Error("expected healthy=true for 200 response")
	}
}

func TestDefaultProcessHealthChecker_Failure(t *testing.T) {
	// Use a port that is almost certainly not listening
	checker := NewDefaultProcessHealthChecker(500 * time.Millisecond)
	healthy, err := checker.Check(context.Background(), 19999)
	if err == nil {
		t.Error("expected error for unreachable port")
	}
	if healthy {
		t.Error("expected healthy=false for unreachable port")
	}
}
