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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newPressureTestAgent(t *testing.T, isvcs ...*inferencev1alpha1.InferenceService) *MetalAgent {
	return newPressureTestAgentWithInterceptor(t, nil, isvcs...)
}

// newPressureTestAgentWithInterceptor is newPressureTestAgent with optional
// interceptor funcs on the fake client, so a test can make individual API
// calls (an UnregisterEndpoint delete, say) fail.
func newPressureTestAgentWithInterceptor(
	t *testing.T, funcs *interceptor.Funcs, isvcs ...*inferencev1alpha1.InferenceService,
) *MetalAgent {
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
	if funcs != nil {
		builder = builder.WithInterceptorFuncs(*funcs)
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

// killedStopExecutor reports the stop the grace-period SIGKILL path produces
// (child_exit.go): a teardown that completed without grace. Paired with
// TestMetalStopProcess_KilledAfterGraceIsTyped, which pins that the real
// stop path produces this type (#2091).
type killedStopExecutor struct{ stubExecutor }

func (killedStopExecutor) StopProcess(_ int) error { return &KilledAfterGraceError{PID: 999990} }

// TestHandleMemoryPressure_EvictionTeardownOutcomes pins how the eviction
// path reacts to each deleteProcess outcome (#2091): a SIGKILL after the
// grace period and an endpoint-unregister failure both leave the engine
// stopped, so the eviction completes and the block stays; only a stop that
// may have left the engine running clears the block.
func TestHandleMemoryPressure_EvictionTeardownOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		exec        ProcessExecutor
		funcs       *interceptor.Funcs
		seed        func(t *testing.T, a *MetalAgent)
		wantBlocked bool
		wantEvicted float64
		wantEvent   bool
	}{
		{
			name:        "killed after grace keeps the block",
			exec:        killedStopExecutor{},
			wantBlocked: true,
			wantEvicted: 1,
			wantEvent:   true,
		},
		{
			name: "failed endpoint unregister keeps the block",
			exec: stubExecutor{},
			funcs: &interceptor.Funcs{
				Delete: func(
					_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption,
				) error {
					return errors.New("apiserver unreachable")
				},
			},
			seed: func(t *testing.T, a *MetalAgent) {
				// deleteOwned only reaches Delete for an object the agent
				// owns; without a seeded owned Service the missing object
				// short-circuits and the unregister would succeed.
				t.Helper()
				svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
					Name:      a.registry.serviceNameFor("svc-low"),
					Namespace: "default",
					Labels:    map[string]string{managedByLabel: managedByValue},
				}}
				if err := a.config.K8sClient.Create(context.Background(), svc); err != nil {
					t.Fatalf("seed owned service: %v", err)
				}
			},
			wantBlocked: true,
			wantEvicted: 1,
			wantEvent:   true,
		},
		{
			name:        "a stop that may leave the engine running clears the block",
			exec:        failingStopExecutor{},
			wantBlocked: false,
			wantEvicted: 0,
			wantEvent:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			low := newPressureTestISvc("svc-low", "low")
			high := newPressureTestISvc("svc-high", "high")
			a := newPressureTestAgentWithInterceptor(t, tc.funcs, low, high)
			a.config.EvictionEnabled = true
			a.executors["llama-server"] = tc.exec
			a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")
			if tc.seed != nil {
				tc.seed(t, a)
			}
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

			before := testutil.ToFloat64(evictionsTotal)
			a.handleMemoryPressure(context.Background(), MemoryPressureCritical, MemoryStats{
				TotalMemory: 100 << 30, TotalRSS: 80 << 30,
			})

			a.mu.RLock()
			_, blocked := a.pressureBlocked["default/svc-low"]
			a.mu.RUnlock()
			if blocked != tc.wantBlocked {
				t.Errorf("pressureBlocked[default/svc-low] present = %v, want %v", blocked, tc.wantBlocked)
			}
			if got := testutil.ToFloat64(evictionsTotal) - before; got != tc.wantEvicted {
				t.Errorf("evictions_total delta = %v, want %v", got, tc.wantEvicted)
			}
			events := drainEvents(rec)
			hasEvicted := false
			for _, e := range events {
				if strings.Contains(e, EventReasonEvicted) {
					hasEvicted = true
					break
				}
			}
			if hasEvicted != tc.wantEvent {
				t.Errorf("Evicted event present = %v, want %v (events: %v)", hasEvicted, tc.wantEvent, events)
			}
		})
	}
}

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
