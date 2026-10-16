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
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newPressureTestAgent(t *testing.T, isvcs ...*inferencev1alpha1.InferenceService) *MetalAgent {
	t.Helper()
	// We need corev1 and discovery/v1 in the scheme so deleteProcess ->
	// UnregisterEndpoint can issue Delete calls against Service/EndpointSlice
	// (the fake client returns "no kind registered" instead of NotFound
	// otherwise, and that error would be misinterpreted as a real failure).
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&inferencev1alpha1.InferenceService{})
	for _, isvc := range isvcs {
		builder = builder.WithObjects(isvc)
	}
	k8sClient := builder.Build()

	return NewMetalAgent(MetalAgentConfig{
		K8sClient: k8sClient,
		Logger:    newNopLogger(),
	})
}

// nolint:unparam // name is parameterized for clarity at call sites even though existing tests all use "svc-a"
func newPressureTestISvc(name, priority string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name, Priority: priority},
	}
}

// TestHandleMemoryPressure_ConditionPatchedOnTransition verifies that a
// transition from Normal to Warning patches the MemoryPressure condition on
// the running InferenceService so operators can see why their workload is
// degraded.
func TestHandleMemoryPressure_ConditionPatchedOnTransition(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "normal")
	a := newPressureTestAgent(t, isvc)

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "normal", StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureWarning, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 25 << 30,
	})

	got := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "svc-a"}, got); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryPressure)
	if cond == nil {
		t.Fatal("expected MemoryPressure condition to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("status = %v, want True", cond.Status)
	}
	if cond.Reason != ReasonMemoryWarning {
		t.Errorf("reason = %q, want %q", cond.Reason, ReasonMemoryWarning)
	}
}

// TestHandleMemoryPressure_NoEvictionAtWarning ensures that under Warning
// pressure we surface the condition but do NOT kill any processes — the user
// should get a warning before we start tearing things down.
func TestHandleMemoryPressure_NoEvictionAtWarning(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "low", StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureWarning, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 80 << 30,
	})

	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.pressureBlocked) != 0 {
		t.Errorf("Warning level must not block any process, got %d", len(a.pressureBlocked))
	}
	if _, still := a.processes["default/svc-a"]; !still {
		t.Error("process map should still contain svc-a; eviction is critical-only")
	}
}

// TestHandleMemoryPressure_EvictionBlockedBelowGuard verifies the
// friendly-fire guard: even under Critical pressure, we will not evict if
// LLMKube is using less than 50% of system RSS.
func TestHandleMemoryPressure_EvictionBlockedBelowGuard(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "low", StartedAt: time.Now(),
	}

	// 30% of total — pressure is from somewhere else, not us.
	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 30 << 30,
	})

	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.pressureBlocked) != 0 {
		t.Errorf("must not evict below 50%% RSS guard, got %d blocked", len(a.pressureBlocked))
	}
}

// stubExecutor lets handleMemoryPressure exercise the eviction path
// (deleteProcess -> StopProcess) without spawning a real llama-server. We
// only need StopProcess to be a no-op; StartProcess is unused in these tests.
type stubExecutor struct{}

func (stubExecutor) StartProcess(_ context.Context, _ ExecutorConfig) (*ManagedProcess, error) {
	return nil, nil
}
func (stubExecutor) StopProcess(_ int) error { return nil }

// TestHandleMemoryPressure_EvictsLowestPriorityWhenEnabledAndAboveGuard is the
// end-to-end test for the eviction path: guard satisfied, EvictionEnabled
// true, multiple priorities — verifies the lowest-priority process is the
// one removed from the managed map and the key lands in pressureBlocked so
// a subsequent ensureProcess will not respawn it, with its RSS recorded so a
// restart can wait for room to take that memory back.
func TestHandleMemoryPressure_EvictsLowestPriorityWhenEnabledAndAboveGuard(t *testing.T) {
	low := newPressureTestISvc("svc-low", "low")
	high := newPressureTestISvc("svc-high", "high")
	a := newPressureTestAgent(t, low, high)
	a.config.EvictionEnabled = true
	a.executors["llama-server"] = stubExecutor{}
	a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")

	a.processes["default/svc-low"] = &ManagedProcess{
		Name: "svc-low", Namespace: "default", Priority: "low",
		ModelPath: "/m.gguf", PID: 999990, StartedAt: time.Now(),
	}
	a.processes["default/svc-high"] = &ManagedProcess{
		Name: "svc-high", Namespace: "default", Priority: "high",
		ModelPath: "/m.gguf", PID: 999991, StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 80 << 30,
		ProcessRSS: map[string]uint64{"svc-low": 30 << 30, "svc-high": 50 << 30},
	})

	a.mu.RLock()
	defer a.mu.RUnlock()
	if _, stillRunning := a.processes["default/svc-low"]; stillRunning {
		t.Error("svc-low should have been evicted (lowest priority)")
	}
	if _, stillRunning := a.processes["default/svc-high"]; !stillRunning {
		t.Error("svc-high should still be running (only one eviction per tick)")
	}
	evicted, blocked := a.pressureBlocked["default/svc-low"]
	if !blocked {
		t.Error("evicted key must be in pressureBlocked to prevent respawn")
	}
	if evicted.rss != 30<<30 {
		t.Errorf("recorded RSS = %d, want %d (svc-low's RSS at eviction)", evicted.rss, uint64(30<<30))
	}
}

// TestHandleMemoryPressure_DisabledHonorsCLIFlag confirms the CLI opt-in:
// even at Critical pressure with the guard satisfied, EvictionEnabled=false
// must keep the process alive. Regression guard for the bug where the
// handler hardcoded evictionEnabled=true.
func TestHandleMemoryPressure_DisabledHonorsCLIFlag(t *testing.T) {
	isvc := newPressureTestISvc("svc-low", "low")
	a := newPressureTestAgent(t, isvc)
	a.config.EvictionEnabled = false // explicit
	a.executors["llama-server"] = stubExecutor{}
	a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")

	a.processes["default/svc-low"] = &ManagedProcess{
		Name: "svc-low", Namespace: "default", Priority: "low",
		ModelPath: "/m.gguf", PID: 999990, StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 80 << 30,
	})

	a.mu.RLock()
	defer a.mu.RUnlock()
	if _, stillRunning := a.processes["default/svc-low"]; !stillRunning {
		t.Error("EvictionEnabled=false must prevent eviction even at Critical+guard")
	}
	if _, blocked := a.pressureBlocked["default/svc-low"]; blocked {
		t.Error("no eviction means no pressureBlocked entry")
	}
}

// TestEnsureProcess_BlockedUnderCriticalSkipsRespawn verifies the back-pressure
// guard in ensureProcess: a key in pressureBlocked is a no-op while pressure
// is non-Normal, so the controller's UPDATED-event loop cannot defeat
// eviction by silently respawning.
func TestEnsureProcess_BlockedUnderCriticalSkipsRespawn(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)
	a.executors["llama-server"] = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())

	a.pressureBlocked["default/svc-a"] = evictedService{}
	a.lastPressureLevel = MemoryPressureCritical

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess returned error, expected silent skip: %v", err)
	}
	if _, exists := a.processes["default/svc-a"]; exists {
		t.Error("ensureProcess respawned despite pressureBlocked guard")
	}
}

// TestHandleMemoryPressure_PatchesConditionForLateSpawnedProcess locks down
// the PR-2B fix for the gap PR-2A had: a process that spawns AFTER a level
// transition would not get a MemoryPressure condition until the next
// transition. Now the handler patches any key not yet observed at the
// current level, so a late-arrival sees the condition on the very next
// watchdog tick.
func TestHandleMemoryPressure_PatchesConditionForLateSpawnedProcess(t *testing.T) {
	earlyISvc := newPressureTestISvc("svc-early", "normal")
	lateISvc := newPressureTestISvc("svc-late", "normal")
	a := newPressureTestAgent(t, earlyISvc, lateISvc)

	// Simulate the agent has already observed svc-early at Critical (the
	// transition tick fired before svc-late spawned).
	a.processes["default/svc-early"] = &ManagedProcess{
		Name: "svc-early", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "normal", StartedAt: time.Now(),
	}
	a.lastPressureLevel = MemoryPressureCritical
	a.pressureObserved["default/svc-early"] = MemoryPressureCritical

	// Now svc-late spawns mid-pressure, then the watchdog ticks again at
	// the same level (no transition).
	a.processes["default/svc-late"] = &ManagedProcess{
		Name: "svc-late", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "normal", StartedAt: time.Now(),
	}
	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 25 << 30,
	})

	got := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "svc-late"}, got); err != nil {
		t.Fatalf("get svc-late: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryPressure)
	if cond == nil {
		t.Fatal("late-spawned process must receive a MemoryPressure condition on the next tick")
	}
	if cond.Reason != ReasonMemoryCritical {
		t.Errorf("condition reason = %q, want %q", cond.Reason, ReasonMemoryCritical)
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.pressureObserved["default/svc-late"] != MemoryPressureCritical {
		t.Error("svc-late must be marked observed at Critical after the patch")
	}
}

// countingClient wraps the fake client to track how many Status().Update
// calls land on the apiserver. We use it to verify that the
// already-observed short-circuit prevents redundant patches.
type countingStatusClient struct {
	client.Client
	updates *int
}

func (c countingStatusClient) Status() client.SubResourceWriter {
	return countingStatusWriter{SubResourceWriter: c.Client.Status(), updates: c.updates}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	updates *int
}

func (w countingStatusWriter) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	*w.updates++
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// TestHandleMemoryPressure_DoesNotRePatchAlreadyObserved verifies the
// other side of the late-spawn fix: a key that has been observed at the
// current level must not generate another Status().Update on subsequent
// ticks at the same level. Without this, sustained Critical pressure would
// thrash the apiserver every tick.
func TestHandleMemoryPressure_DoesNotRePatchAlreadyObserved(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "normal")
	a := newPressureTestAgent(t, isvc)
	updates := 0
	a.config.K8sClient = countingStatusClient{Client: a.config.K8sClient, updates: &updates}

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "normal", StartedAt: time.Now(),
	}

	stats := MemoryStats{TotalMemory: 100 << 30, TotalRSS: 25 << 30}
	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, stats)
	if updates != 1 {
		t.Errorf("first tick at Critical should patch once, got %d", updates)
	}
	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, stats)
	if updates != 1 {
		t.Errorf("second tick at same Critical level must not re-patch, got %d total updates", updates)
	}
}

// TestHandleMemoryPressure_IncrementsSkippedCounter is the table-driven
// regression guard for the new evictions_skipped_total metric. Each
// scenario forces a different skip path and asserts the labelled counter
// ticks exactly once.
func TestHandleMemoryPressure_IncrementsSkippedCounter(t *testing.T) {
	tests := []struct {
		name           string
		evictionEnable bool
		level          MemoryPressureLevel
		processes      func(*MetalAgent)
		stats          MemoryStats
		wantReason     string
	}{
		{
			name:           "disabled at critical",
			evictionEnable: false,
			level:          MemoryPressureCritical,
			processes: func(a *MetalAgent) {
				a.processes["default/svc-a"] = &ManagedProcess{
					Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
					Priority: "low", StartedAt: time.Now(),
				}
			},
			stats:      MemoryStats{TotalMemory: 100 << 30, TotalRSS: 80 << 30},
			wantReason: "disabled",
		},
		{
			name:           "enabled but below guard at critical",
			evictionEnable: true,
			level:          MemoryPressureCritical,
			processes: func(a *MetalAgent) {
				a.processes["default/svc-a"] = &ManagedProcess{
					Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
					Priority: "low", StartedAt: time.Now(),
				}
				a.processes["default/svc-b"] = &ManagedProcess{
					Name: "svc-b", Namespace: "default", ModelPath: "/m.gguf",
					Priority: "low", StartedAt: time.Now(),
				}
			},
			stats:      MemoryStats{TotalMemory: 100 << 30, TotalRSS: 30 << 30},
			wantReason: "below_guard",
		},
		{
			name:           "floor with one process",
			evictionEnable: true,
			level:          MemoryPressureCritical,
			processes: func(a *MetalAgent) {
				a.processes["default/svc-only"] = &ManagedProcess{
					Name: "svc-only", Namespace: "default", ModelPath: "/m.gguf",
					Priority: "low", StartedAt: time.Now(),
				}
			},
			stats:      MemoryStats{TotalMemory: 100 << 30, TotalRSS: 80 << 30},
			wantReason: SkipReasonFloor,
		},
		{
			name:           "all protected",
			evictionEnable: true,
			level:          MemoryPressureCritical,
			processes: func(a *MetalAgent) {
				a.processes["default/p1"] = &ManagedProcess{
					Name: "p1", Namespace: "default", ModelPath: "/m.gguf",
					Priority: "low", StartedAt: time.Now(),
					EvictionProtection: true,
				}
				a.processes["default/p2"] = &ManagedProcess{
					Name: "p2", Namespace: "default", ModelPath: "/m.gguf",
					Priority: "low", StartedAt: time.Now(),
					EvictionProtection: true,
				}
			},
			stats:      MemoryStats{TotalMemory: 100 << 30, TotalRSS: 80 << 30},
			wantReason: SkipReasonAllProtected,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newPressureTestAgent(t)
			a.config.EvictionEnabled = tc.evictionEnable
			a.executors["llama-server"] = stubExecutor{}
			a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")
			tc.processes(a)

			before := readSkippedCounter(t, tc.wantReason)
			a.handleMemoryPressure(context.Background(), tc.level, tc.stats)
			after := readSkippedCounter(t, tc.wantReason)

			if got := after - before; got != 1 {
				t.Errorf("evictions_skipped_total{reason=%q} delta = %v, want 1", tc.wantReason, got)
			}
		})
	}
}

// readSkippedCounter pulls the current value of evictions_skipped_total
// for a given reason label out of the shared Prometheus registry. Skipped
// counters survive across tests in this package because the registry is a
// process-global; tests measure deltas instead of absolute values.
func readSkippedCounter(t *testing.T, reason string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := evictionsSkippedTotal.WithLabelValues(reason).Write(m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// failingStopExecutor is a stubExecutor whose StopProcess fails, so the
// eviction's deleteProcess reports a teardown error.
type failingStopExecutor struct{ stubExecutor }

func (failingStopExecutor) StopProcess(_ int) error { return errors.New("stop failed") }

// TestHandleMemoryPressure_CountsEvictionsTotal pins evictions_total: it rises
// by exactly one when the watchdog actually stops a process, and does not move
// when an eviction is skipped or the picked process's teardown fails.
func TestHandleMemoryPressure_CountsEvictionsTotal(t *testing.T) {
	addProcess := func(a *MetalAgent, name, priority string, pid int) {
		a.processes["default/"+name] = &ManagedProcess{
			Name: name, Namespace: "default", Priority: priority,
			ModelPath: "/m.gguf", PID: pid, StartedAt: time.Now(),
		}
	}
	one := func(a *MetalAgent) { addProcess(a, "svc-low", "low", 999990) }
	two := func(a *MetalAgent) {
		one(a)
		addProcess(a, "svc-high", "high", 999991)
	}
	tests := []struct {
		name      string
		enabled   bool
		exec      ProcessExecutor
		processes func(*MetalAgent)
		rssGiB    uint64
		want      float64
	}{
		{"eviction", true, stubExecutor{}, two, 80, 1},
		{"skipped: disabled", false, stubExecutor{}, two, 80, 0},
		{"skipped: below guard", true, stubExecutor{}, two, 30, 0},
		{"skipped: floor", true, stubExecutor{}, one, 80, 0},
		{"teardown failed", true, failingStopExecutor{}, two, 80, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newPressureTestAgent(t)
			a.config.EvictionEnabled = tc.enabled
			a.executors["llama-server"] = tc.exec
			a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")
			tc.processes(a)

			before := testutil.ToFloat64(evictionsTotal)
			a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
				TotalMemory: 100 << 30, TotalRSS: tc.rssGiB << 30,
			})
			if got := testutil.ToFloat64(evictionsTotal) - before; got != tc.want {
				t.Errorf("evictions_total delta = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHandleMemoryPressure_NormalClearsBlockedSet verifies that when pressure
// drops back to Normal, previously evicted services become respawnable again
// (pressureBlocked is reset).
func TestHandleMemoryPressure_NormalClearsBlockedSet(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)
	// The return to Normal also tries to restart svc-a. Its Model does not
	// exist here, so that start fails and withdraws svc-a's endpoint, which
	// needs a registry.
	a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")

	a.pressureBlocked["default/svc-a"] = evictedService{}
	a.lastPressureLevel = MemoryPressureCritical

	a.handleMemoryPressure(context.Background(), MemoryPressureNormal, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 10 << 30,
	})
	a.evictedRestarts.Wait()

	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.pressureBlocked) != 0 {
		t.Errorf("Normal pressure should reset pressureBlocked, got %d entries", len(a.pressureBlocked))
	}
	if a.lastPressureLevel != MemoryPressureNormal {
		t.Errorf("lastPressureLevel = %v, want Normal", a.lastPressureLevel)
	}
}

// returnToNormal reports a Warning-or-above to Normal transition to the
// agent, then waits for the evicted-service restarts it launches.
func returnToNormal(a *MetalAgent) {
	normalSample(a, 60)
}

// normalSample reports a Normal watchdog sample with availGiB of 100 GiB
// available, then waits for the evicted-service restarts it launches.
func normalSample(a *MetalAgent, availGiB uint64) {
	a.handleMemoryPressure(context.Background(), MemoryPressureNormal, MemoryStats{
		TotalMemory: 100 << 30, AvailableMemory: availGiB << 30, TotalRSS: 40 << 30,
	})
	a.evictedRestarts.Wait()
}

// blockEvicted marks keys as evicted under Critical pressure with rssGiB of
// resident memory each, the state handleMemoryPressure's eviction path
// leaves behind.
func blockEvicted(a *MetalAgent, rssGiB uint64, keys ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, key := range keys {
		a.pressureBlocked[key] = evictedService{rss: rssGiB << 30}
	}
	a.lastPressureLevel = MemoryPressureCritical
}

// runningKeys returns the sorted keys of the agent's managed processes.
func runningKeys(a *MetalAgent) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	keys := make([]string, 0, len(a.processes))
	for key := range a.processes {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// TestHandleMemoryPressure_NormalRestartsEvictedService is the regression test
// for #2078: a service evicted under Critical pressure must start again once
// pressure returns to Normal. No watcher runs here, so the restart has to come
// from handleMemoryPressure itself: the watcher emits an event only when an
// InferenceService's resourceVersion changes, so clearing the block alone left
// the service down until the agent restarted. Its MemoryPressure condition
// must also stop claiming Evicted.
func TestHandleMemoryPressure_NormalRestartsEvictedService(t *testing.T) {
	low := refusalISVC("svc-low")
	low.Spec.Priority = "low"
	high := refusalISVC("svc-high")
	high.Spec.Priority = "high"
	a, ex, _ := refusalFixture(t, MetalAgentConfig{EvictionEnabled: true}, low, high, refusalModel())
	ctx := context.Background()
	for _, isvc := range []*inferencev1alpha1.InferenceService{low, high} {
		if err := a.ensureProcess(ctx, isvc); err != nil {
			t.Fatalf("start %s: %v", isvc.Name, err)
		}
	}

	a.handleMemoryPressure(ctx, MemoryPressureCritical, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 80 << 30,
	})
	if !a.isPressureBlocked("default/svc-low") || ex.starts != 2 {
		t.Fatalf("setup: svc-low not evicted (blocked=%v, starts=%d)",
			a.isPressureBlocked("default/svc-low"), ex.starts)
	}
	if got := runningKeys(a); !slices.Equal(got, []string{"default/svc-high"}) {
		t.Fatalf("setup: running after eviction = %v, want only svc-high", got)
	}

	returnToNormal(a)

	if ex.starts != 3 || ex.lastCfg.Name != "svc-low" {
		t.Errorf("starts = %d (last %q), want 3 with svc-low restarted last", ex.starts, ex.lastCfg.Name)
	}
	if got, want := runningKeys(a), []string{"default/svc-high", "default/svc-low"}; !slices.Equal(got, want) {
		t.Errorf("running after the return to Normal = %v, want %v", got, want)
	}
	if a.isPressureBlocked("default/svc-low") {
		t.Error("svc-low is still blocked after the return to Normal")
	}

	got := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "svc-low"}, got); err != nil {
		t.Fatalf("get svc-low: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryPressure)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonMemoryNormal {
		t.Errorf("svc-low MemoryPressure condition = %+v, want status False reason %s", cond, ReasonMemoryNormal)
	}
}

// TestHandleMemoryPressure_NormalLeavesGoneEvictedServicesDown covers the
// services a return to Normal must not start even though it clears their
// block: one deleted while blocked, one being deleted (held by a finalizer),
// one scaled to zero, and one whose Model is no longer a Metal one (the
// watcher stopped claiming it, which never cleared its block). svc-live is
// the control: it proves the restart ran at all.
func TestHandleMemoryPressure_NormalLeavesGoneEvictedServicesDown(t *testing.T) {
	live := refusalISVC("svc-live")
	deleting := refusalISVC("svc-deleting")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	deleting.Finalizers = []string{"example.com/hold"}
	zero := refusalISVC("svc-zero")
	zero.Spec.Replicas = ptr.To(int32(0))
	moved := refusalISVC("svc-moved")
	moved.Spec.ModelRef = "cuda-model"
	cudaModel := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "cuda-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{Source: "https://example.invalid/c.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cuda"}},
	}
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, live, deleting, zero, moved, refusalModel(), cudaModel)
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, "default", newNopLogger())

	blockEvicted(a, 0, "default/svc-deleting", "default/svc-gone", "default/svc-live",
		"default/svc-moved", "default/svc-zero")
	returnToNormal(a)

	if got, want := runningKeys(a), []string{"default/svc-live"}; !slices.Equal(got, want) {
		t.Errorf("running after the return to Normal = %v, want %v", got, want)
	}
	if ex.starts != 1 {
		t.Errorf("engine started %d time(s), want 1 (svc-live only)", ex.starts)
	}
}

// TestHandleMemoryPressure_RestartStopsWhenPressureReturns covers pressure
// rising again while evicted services are being restarted: the services not
// yet restarted must be blocked again for the next return to Normal, not
// started under the pressure that evicted them.
func TestHandleMemoryPressure_RestartStopsWhenPressureReturns(t *testing.T) {
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, evictedISVC("svc-a"), evictedISVC("svc-b"), refusalModel())
	// svc-a restarts first (keys run in sorted order), and its model load
	// drives the host back to Critical.
	ex.onStart = func() {
		a.mu.Lock()
		a.lastPressureLevel = MemoryPressureCritical
		a.mu.Unlock()
	}

	blockEvicted(a, 0, "default/svc-a", "default/svc-b")
	returnToNormal(a)

	if ex.starts != 1 || ex.lastCfg.Name != "svc-a" {
		t.Errorf("starts = %d (last %q), want only svc-a started", ex.starts, ex.lastCfg.Name)
	}
	if !a.isPressureBlocked("default/svc-b") {
		t.Error("svc-b must be blocked again: pressure left Normal before its turn")
	}
	if a.isPressureBlocked("default/svc-a") {
		t.Error("svc-a was restarted, so it must not be blocked")
	}
	if got := memoryPressureCondition(t, a, "svc-b"); got != conditionEvicted {
		t.Errorf("svc-b MemoryPressure = %q, want %q while it is blocked again", got, conditionEvicted)
	}
}

// evictedISVC is a refusal-fixture InferenceService whose MemoryPressure
// condition reads Evicted, as the eviction path leaves it.
func evictedISVC(name string) *inferencev1alpha1.InferenceService {
	isvc := refusalISVC(name)
	isvc.Status.Conditions = []metav1.Condition{{
		Type: ConditionMemoryPressure, Status: metav1.ConditionTrue, Reason: ReasonEvicted,
		Message: "Evicted by memory watchdog", LastTransitionTime: metav1.Now(),
	}}
	return isvc
}

// memoryPressureCondition returns name's MemoryPressure condition as
// "Status/Reason", or "" when it has none.
func memoryPressureCondition(t *testing.T, a *MetalAgent, name string) string {
	t.Helper()
	got := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: name}, got); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryPressure); cond != nil {
		return string(cond.Status) + "/" + cond.Reason
	}
	return ""
}

// Condition values memoryPressureCondition returns for a blocked service and
// for one back at Normal.
const (
	conditionEvicted = "True/" + ReasonEvicted
	conditionNormal  = "False/" + ReasonMemoryNormal
)

// headroomFixture is a refusal fixture whose watchdog Warning threshold is
// 0.20, so with the 100 GiB normalSample reports a restart must leave 20 GiB
// available.
func headroomFixture(
	t *testing.T, objs ...runtimeObject,
) (*MetalAgent, *recordingExecutor) {
	t.Helper()
	a, ex, _ := refusalFixture(t, MetalAgentConfig{WatchdogConfig: &MemoryWatchdogConfig{
		Interval: time.Second, WarningThreshold: 0.20, CriticalThreshold: 0.10,
	}}, objs...)
	return a, ex
}

// TestHandleMemoryPressure_RestartWaitsForHeadroom covers the evict and restart
// loop admission cannot stop: admission compares one model's estimate with a
// fraction of total memory, never with what is available. A service whose
// eviction-time RSS would leave less than the Warning floor available stays
// blocked, and keeps its Evicted condition, until a later Normal sample shows
// enough room.
func TestHandleMemoryPressure_RestartWaitsForHeadroom(t *testing.T) {
	a, ex := headroomFixture(t, evictedISVC("svc-a"), refusalModel())
	blockEvicted(a, 30, "default/svc-a")

	normalSample(a, 45) // 45 - 30 = 15 GiB left, under the 20 GiB floor
	if ex.starts != 0 || !a.isPressureBlocked("default/svc-a") {
		t.Fatalf("with 15 GiB left: starts = %d, blocked = %v; want 0, true",
			ex.starts, a.isPressureBlocked("default/svc-a"))
	}
	if got := memoryPressureCondition(t, a, "svc-a"); got != conditionEvicted {
		t.Errorf("MemoryPressure = %q, want %q while svc-a waits", got, conditionEvicted)
	}

	normalSample(a, 55) // 55 - 30 = 25 GiB left
	if ex.starts != 1 || a.isPressureBlocked("default/svc-a") {
		t.Errorf("with 25 GiB left: starts = %d, blocked = %v; want 1, false",
			ex.starts, a.isPressureBlocked("default/svc-a"))
	}
	if got := memoryPressureCondition(t, a, "svc-a"); got != conditionNormal {
		t.Errorf("MemoryPressure = %q, want %q after the restart", got, conditionNormal)
	}
}

// TestHandleMemoryPressure_RestartReservesHeadroomAcrossServices covers two
// evicted services that each fit the headroom alone but not together: only
// the first is restarted, and the second waits.
func TestHandleMemoryPressure_RestartReservesHeadroomAcrossServices(t *testing.T) {
	a, ex := headroomFixture(t, evictedISVC("svc-a"), evictedISVC("svc-b"), refusalModel())
	blockEvicted(a, 20, "default/svc-a", "default/svc-b")

	normalSample(a, 55) // one leaves 35 GiB, both leave 15 GiB, under the 20 GiB floor

	if got, want := runningKeys(a), []string{"default/svc-a"}; !slices.Equal(got, want) || ex.starts != 1 {
		t.Errorf("running = %v after %d start(s), want %v after 1", got, ex.starts, want)
	}
	if !a.isPressureBlocked("default/svc-b") {
		t.Error("svc-b must stay blocked: with svc-a back there is no room for it")
	}
}

// TestPickRestartable pins the headroom arithmetic: services are taken in key
// order while their summed RSS leaves at least floor bytes available, one
// that does not fit is skipped, and a smaller one after it may still fit.
func TestPickRestartable(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name      string
		blocked   map[string]uint64 // key -> RSS in GiB
		available uint64            // GiB
		want      []string
	}{
		{"fits", map[string]uint64{"a": 30}, 55, []string{"a"}},
		{"under the floor", map[string]uint64{"a": 30}, 45, nil},
		{"exactly the floor", map[string]uint64{"a": 30}, 50, []string{"a"}},
		{"second does not fit with the first", map[string]uint64{"a": 20, "b": 20}, 55, []string{"a"}},
		{"smaller later one fits", map[string]uint64{"a": 40, "b": 10}, 45, []string{"b"}},
		{"unknown RSS reserves nothing", map[string]uint64{"a": 0}, 20, []string{"a"}},
		{"more than available", map[string]uint64{"a": 90}, 60, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocked := make(map[string]evictedService, len(tt.blocked))
			for key, rss := range tt.blocked {
				blocked[key] = evictedService{rss: rss * gib}
			}
			if got := pickRestartable(blocked, tt.available*gib, 20*gib); !slices.Equal(got, tt.want) {
				t.Errorf("pickRestartable = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWatchdogRestart_DebouncedAndRetried wires a real watchdog to the agent:
// a Warning sample inside the Normal debounce window starts nothing, the
// debounced return to Normal does not restart a service the headroom cannot
// take, and a later steady Normal sample (delivered because the service is
// still waiting) restarts it once there is room.
func TestWatchdogRestart_DebouncedAndRetried(t *testing.T) {
	a, ex := headroomFixture(t, evictedISVC("svc-a"), refusalModel())
	blockEvicted(a, 30, "default/svc-a")
	total := uint64(100 << 30)
	provider := &mockMemoryProvider{totalBytes: total}
	w := NewMemoryWatchdog(provider, a.processMemInfoSnapshot,
		func(level MemoryPressureLevel, stats MemoryStats) {
			a.handleMemoryPressure(context.Background(), level, stats)
		}, *a.config.WatchdogConfig, newNopLogger())
	w.wantNormal = a.hasEvictedWaiting

	sample := func(availGiB uint64) {
		t.Helper()
		provider.availableBytes = availGiB << 30
		w.sample()
		a.evictedRestarts.Wait()
	}
	for _, avail := range []uint64{5, 60, 60, 15, 60, 60} { // Critical, then Normal broken by Warning
		sample(avail)
	}
	if ex.starts != 0 {
		t.Fatalf("started %d time(s) before three consecutive Normal samples, want 0", ex.starts)
	}
	sample(45) // third Normal in a row: return delivered, but 45 - 30 leaves 15 GiB
	if ex.starts != 0 || !a.isPressureBlocked("default/svc-a") {
		t.Fatalf("after the return with 15 GiB left: starts = %d, blocked = %v; want 0, true",
			ex.starts, a.isPressureBlocked("default/svc-a"))
	}
	sample(60) // steady Normal, retried because svc-a waits: 30 GiB left
	if ex.starts != 1 || a.isPressureBlocked("default/svc-a") {
		t.Errorf("after the retry with 30 GiB left: starts = %d, blocked = %v; want 1, false",
			ex.starts, a.isPressureBlocked("default/svc-a"))
	}
}

// TestHandleMemoryPressure_ReblockSkipsRunningServices covers pressure rising
// mid-batch after a watch event already started (or is starting) a service
// whose block was cleared: blocking a live process again would mark it
// Evicted and stop a later restart from replacing it.
func TestHandleMemoryPressure_ReblockSkipsRunningServices(t *testing.T) {
	a, ex, _ := refusalFixture(t, MetalAgentConfig{},
		refusalISVC("svc-a"), refusalISVC("svc-b"), refusalISVC("svc-c"), refusalModel())
	ex.onStart = func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.lastPressureLevel = MemoryPressureCritical
		a.processes["default/svc-b"] = &ManagedProcess{Name: "svc-b", Namespace: "default", PID: 4343}
		a.starting["default/svc-c"] = true
	}

	blockEvicted(a, 0, "default/svc-a", "default/svc-b", "default/svc-c")
	returnToNormal(a)

	for _, key := range []string{"default/svc-b", "default/svc-c"} {
		if a.isPressureBlocked(key) {
			t.Errorf("%s is running or starting, so it must not be blocked again", key)
		}
	}
}

// TestHandleMemoryPressure_RestartKeepsBlockOnReadError covers a failed read of
// an evicted InferenceService during its restart: the service stays blocked,
// so a later Normal sample retries it, instead of being dropped and left down.
func TestHandleMemoryPressure_RestartKeepsBlockOnReadError(t *testing.T) {
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*inferencev1alpha1.InferenceService); ok && key.Name == "svc-a" {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, refusalISVC("svc-a"), refusalModel())

	blockEvicted(a, 0, "default/svc-a")
	returnToNormal(a)

	if ex.starts != 0 || !a.isPressureBlocked("default/svc-a") {
		t.Errorf("starts = %d, blocked = %v; want 0, true", ex.starts, a.isPressureBlocked("default/svc-a"))
	}
}

// TestHandleMemoryPressure_ReturnToNormalSideEffects pins what a return to
// Normal does to a running, never-evicted service now that the watchdog
// delivers it: one Normal-type MemoryPressureLevelChanged event, the
// condition back to False/Normal, and no EvictionSkipped event or skipped
// counter.
func TestHandleMemoryPressure_ReturnToNormalSideEffects(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "normal")
	a := newPressureTestAgent(t, isvc)
	a.config.EvictionEnabled = true
	rec := record.NewFakeRecorder(32)
	a.config.EventRecorder = rec
	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "normal", StartedAt: time.Now(),
	}
	a.handleMemoryPressure(context.Background(), MemoryPressureWarning, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 25 << 30,
	})
	_ = drainEvents(rec)

	reasons := []string{"disabled", "below_guard", SkipReasonEmpty, SkipReasonFloor,
		SkipReasonAllProtected, SkipReasonRuntimeIneligible}
	before := make(map[string]float64, len(reasons))
	for _, r := range reasons {
		before[r] = readSkippedCounter(t, r)
	}

	returnToNormal(a)

	events := drainEvents(rec)
	wantPrefix := corev1.EventTypeNormal + " " + EventReasonMemoryPressureLevelChanged
	if len(events) != 1 || !strings.HasPrefix(events[0], wantPrefix) ||
		!strings.Contains(events[0], "warning to normal") {
		t.Errorf("events = %v, want exactly one Normal %s for warning to normal",
			events, EventReasonMemoryPressureLevelChanged)
	}
	for _, r := range reasons {
		if d := readSkippedCounter(t, r) - before[r]; d != 0 {
			t.Errorf("evictions_skipped_total{reason=%q} moved by %v on a return to Normal", r, d)
		}
	}
	got := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "svc-a"}, got); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionMemoryPressure)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonMemoryNormal {
		t.Errorf("MemoryPressure condition = %+v, want status False reason %s", cond, ReasonMemoryNormal)
	}
}

// drainEvents reads everything currently buffered in a record.FakeRecorder
// channel and returns it. The FakeRecorder's channel buffer is configured by
// the test author; we drain non-blocking so an empty channel returns nil.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestHandleMemoryPressure_EmitsLevelChangedEvent verifies that a level
// transition publishes one MemoryPressureLevelChanged event per managed
// process. Type Warning when leaving Normal, Type Normal when returning.
// Closes #390 for the level-changed reason.
func TestHandleMemoryPressure_EmitsLevelChangedEvent(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "normal")
	a := newPressureTestAgent(t, isvc)
	rec := record.NewFakeRecorder(32)
	a.config.EventRecorder = rec

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "normal", StartedAt: time.Now(),
	}

	// Normal -> Warning transition
	a.handleMemoryPressure(context.Background(), MemoryPressureWarning, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 25 << 30,
	})

	events := drainEvents(rec)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event on transition, got %d: %v", len(events), events)
	}
	if !strings.Contains(events[0], EventReasonMemoryPressureLevelChanged) {
		t.Errorf("event missing reason %q: %s", EventReasonMemoryPressureLevelChanged, events[0])
	}
	if !strings.Contains(events[0], corev1.EventTypeWarning) {
		t.Errorf("non-Normal level should emit type Warning, got: %s", events[0])
	}
	if !strings.Contains(events[0], "normal to warning") {
		t.Errorf("event message should describe the transition, got: %s", events[0])
	}

	// Same-level tick: no new event (we only fire on transitions, not every
	// watchdog tick, otherwise sustained pressure floods event history).
	a.handleMemoryPressure(context.Background(), MemoryPressureWarning, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 25 << 30,
	})
	if len(drainEvents(rec)) != 0 {
		t.Error("same-level ticks must not emit a level-changed event")
	}
}

// TestHandleMemoryPressure_EmitsEvictedEvent verifies that a successful
// eviction emits an Evicted event of Type Warning on the target service. The
// event message must include the priority (so operators can reason about the
// eviction selector) and the system memory state at decision time.
func TestHandleMemoryPressure_EmitsEvictedEvent(t *testing.T) {
	low := newPressureTestISvc("svc-low", "low")
	high := newPressureTestISvc("svc-high", "high")
	a := newPressureTestAgent(t, low, high)
	a.config.EvictionEnabled = true
	a.executors["llama-server"] = stubExecutor{}
	a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")
	rec := record.NewFakeRecorder(32)
	a.config.EventRecorder = rec

	a.processes["default/svc-low"] = &ManagedProcess{
		Name: "svc-low", Namespace: "default", Priority: "low",
		ModelPath: "/m.gguf", PID: 999990, StartedAt: time.Now(),
	}
	a.processes["default/svc-high"] = &ManagedProcess{
		Name: "svc-high", Namespace: "default", Priority: "high",
		ModelPath: "/m.gguf", PID: 999991, StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 80 << 30,
	})

	events := drainEvents(rec)
	var evicted string
	for _, e := range events {
		if strings.Contains(e, EventReasonEvicted) {
			evicted = e
			break
		}
	}
	if evicted == "" {
		t.Fatalf("expected an Evicted event in the stream, got: %v", events)
	}
	if !strings.Contains(evicted, corev1.EventTypeWarning) {
		t.Errorf("Evicted must be Type Warning, got: %s", evicted)
	}
	if !strings.Contains(evicted, "priority=low") {
		t.Errorf("Evicted message should record priority of victim, got: %s", evicted)
	}
}

// TestHandleMemoryPressure_EmitsEvictionSkippedWhenDisabled covers the
// "watchdog wanted to act, refused" path: critical pressure with eviction
// disabled emits an EvictionSkipped event so operators understand why their
// host stayed pressured without action.
func TestHandleMemoryPressure_EmitsEvictionSkippedWhenDisabled(t *testing.T) {
	isvc := newPressureTestISvc("svc-low", "low")
	a := newPressureTestAgent(t, isvc)
	a.config.EvictionEnabled = false
	a.executors["llama-server"] = stubExecutor{}
	a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")
	rec := record.NewFakeRecorder(32)
	a.config.EventRecorder = rec

	a.processes["default/svc-low"] = &ManagedProcess{
		Name: "svc-low", Namespace: "default", Priority: "low",
		ModelPath: "/m.gguf", PID: 999990, StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 80 << 30,
	})

	events := drainEvents(rec)
	var skipped string
	for _, e := range events {
		if strings.Contains(e, EventReasonEvictionSkipped) {
			skipped = e
			break
		}
	}
	if skipped == "" {
		t.Fatalf("expected EvictionSkipped event when eviction disabled, got: %v", events)
	}
	if !strings.Contains(skipped, "skip-reason=disabled") {
		t.Errorf("EvictionSkipped should record skip-reason, got: %s", skipped)
	}
}

// TestEnsureProcess_EmitsRespawnBlockedEvent verifies the back-pressure
// guard surfaces as a Kubernetes event so an operator who runs
// `kubectl describe inferenceservice <svc>` after a "won't start" complaint
// sees the agent's reason rather than just empty status.
func TestEnsureProcess_EmitsRespawnBlockedEvent(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	a := newPressureTestAgent(t, isvc)
	a.executors["llama-server"] = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())
	rec := record.NewFakeRecorder(32)
	a.config.EventRecorder = rec

	a.pressureBlocked["default/svc-a"] = evictedService{}
	a.lastPressureLevel = MemoryPressureCritical

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess returned error, expected silent skip: %v", err)
	}

	events := drainEvents(rec)
	if len(events) != 1 {
		t.Fatalf("expected 1 RespawnBlocked event, got %d: %v", len(events), events)
	}
	if !strings.Contains(events[0], EventReasonRespawnBlocked) {
		t.Errorf("event missing reason %q: %s", EventReasonRespawnBlocked, events[0])
	}
	if !strings.Contains(events[0], corev1.EventTypeWarning) {
		t.Errorf("RespawnBlocked must be Type Warning, got: %s", events[0])
	}
	if !strings.Contains(events[0], MemoryPressureCritical.String()) {
		t.Errorf("RespawnBlocked should mention current pressure level, got: %s", events[0])
	}
}

// TestEmitInferenceEvent_NilRecorderIsNoop verifies the agent stays
// functional when the operator runs without an EventRecorder configured
// (today's default in tests; the metal-agent main.go always wires one in
// production). No panic, no crash, no event.
func TestEmitInferenceEvent_NilRecorderIsNoop(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "normal")
	a := newPressureTestAgent(t, isvc)
	// EventRecorder intentionally nil

	a.processes["default/svc-a"] = &ManagedProcess{
		Name: "svc-a", Namespace: "default", ModelPath: "/m.gguf",
		Priority: "normal", StartedAt: time.Now(),
	}

	a.handleMemoryPressure(context.Background(), MemoryPressureWarning, MemoryStats{
		TotalMemory: 100 << 30, TotalRSS: 25 << 30,
	})
}
