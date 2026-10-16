/*
Copyright 2026.

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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests cover #2078: services evicted under memory pressure restart
// once pressure is Normal and the available memory has room for them, through
// the event loop's restart queue (queueRestart / runDueRestarts).

// returnToNormal reports a Normal sample with 60 of 100 GiB available, then
// runs the restarts it queued, as the event loop would.
func returnToNormal(a *MetalAgent) {
	normalSample(a, 60)
	a.runDueRestarts(context.Background())
}

// normalSample reports a Normal watchdog sample with availGiB of 100 GiB
// available. It only queues restarts; runDueRestarts starts them.
func normalSample(a *MetalAgent, availGiB uint64) {
	a.handleMemoryPressure(context.Background(), MemoryPressureNormal, MemoryStats{
		TotalMemory: 100 << 30, AvailableMemory: availGiB << 30, TotalRSS: 40 << 30,
	})
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

// restartPending reports whether key is owed a restart by the event loop.
func restartPending(a *MetalAgent, key string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	st := a.restarts[key]
	return st != nil && st.pending
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
func headroomFixture(t *testing.T, objs ...runtimeObject) (*MetalAgent, *recordingExecutor) {
	t.Helper()
	a, ex, _ := refusalFixture(t, MetalAgentConfig{WatchdogConfig: &MemoryWatchdogConfig{
		Interval: time.Second, WarningThreshold: 0.20, CriticalThreshold: 0.10,
	}}, objs...)
	return a, ex
}

// TestHandleMemoryPressure_NormalQueuesEvictedRestart covers the hand-off: a
// return to Normal lifts the block of an evicted service that fits and queues
// its restart on the event loop, instead of starting it itself.
func TestHandleMemoryPressure_NormalQueuesEvictedRestart(t *testing.T) {
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, refusalISVC("svc-a"), refusalModel())
	blockEvicted(a, 0, "default/svc-a")

	normalSample(a, 60)

	if a.isPressureBlocked("default/svc-a") || !restartPending(a, "default/svc-a") {
		t.Errorf("blocked = %v, restart pending = %v; want false, true",
			a.isPressureBlocked("default/svc-a"), restartPending(a, "default/svc-a"))
	}
	if ex.starts != 0 {
		t.Errorf("started %d time(s) before the event loop ran the restart, want 0", ex.starts)
	}
	if a.lastPressureLevel != MemoryPressureNormal {
		t.Errorf("lastPressureLevel = %v, want Normal", a.lastPressureLevel)
	}
}

// TestHandleMemoryPressure_NormalRestartsEvictedService is the regression test
// for #2078: a service evicted under Critical pressure must start again once
// pressure returns to Normal. The watcher emits an event only when an
// InferenceService's resourceVersion changes, so clearing the block alone left
// the service down until the agent restarted. Its MemoryPressure condition
// must stop claiming Evicted once its engine runs, and not before.
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

	normalSample(a, 60)
	if got := memoryPressureCondition(t, a, "svc-low"); got != conditionEvicted {
		t.Errorf("MemoryPressure = %q before the restart ran, want %q", got, conditionEvicted)
	}
	a.runDueRestarts(ctx)

	if ex.starts != 3 || ex.lastCfg.Name != "svc-low" {
		t.Errorf("starts = %d (last %q), want 3 with svc-low restarted last", ex.starts, ex.lastCfg.Name)
	}
	if got, want := runningKeys(a), []string{"default/svc-high", "default/svc-low"}; !slices.Equal(got, want) {
		t.Errorf("running after the return to Normal = %v, want %v", got, want)
	}
	if got := memoryPressureCondition(t, a, "svc-low"); got != conditionNormal {
		t.Errorf("MemoryPressure = %q after the restart, want %q", got, conditionNormal)
	}
}

// TestHandleMemoryPressure_NormalLeavesGoneEvictedServicesDown covers the
// services a return to Normal must not start: one deleted while blocked, one
// being deleted (held by a finalizer), one scaled to zero, and one whose Model
// is no longer a Metal one (no longer claimed by this agent). svc-live is the
// control: it proves the restart ran at all.
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

// TestHandleMemoryPressure_QueuedRestartOfGoneServiceNeverStarts covers a
// service deleted, or scaled to zero, after its restart was queued but before
// the event loop ran it: it is not started, the stop is applied, and the next
// Normal sample releases the RSS it held reserved.
func TestHandleMemoryPressure_QueuedRestartOfGoneServiceNeverStarts(t *testing.T) {
	a, ex, _ := refusalFixture(t, MetalAgentConfig{},
		refusalISVC("svc-del"), refusalISVC("svc-zero"), refusalModel())
	ctx := context.Background()
	blockEvicted(a, 10, "default/svc-del", "default/svc-zero")
	normalSample(a, 60)
	if !restartPending(a, "default/svc-del") || !restartPending(a, "default/svc-zero") {
		t.Fatal("setup: restarts not queued")
	}

	gone := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "svc-del"}, gone); err != nil {
		t.Fatal(err)
	}
	if err := a.config.K8sClient.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	zero := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "svc-zero"}, zero); err != nil {
		t.Fatal(err)
	}
	zero.Spec.Replicas = ptr.To(int32(0))
	if err := a.config.K8sClient.Update(ctx, zero); err != nil {
		t.Fatal(err)
	}

	a.runDueRestarts(ctx)

	if ex.starts != 0 {
		t.Errorf("engine started %d time(s), want 0", ex.starts)
	}
	stopped := &inferencev1alpha1.InferenceService{}
	stoppedKey := types.NamespacedName{Namespace: "default", Name: "svc-zero"}
	if err := a.config.K8sClient.Get(ctx, stoppedKey, stopped); err != nil {
		t.Fatal(err)
	}
	if stopped.Status.Phase != inferencev1alpha1.PhaseStopped {
		t.Errorf("svc-zero phase = %q, want %q", stopped.Status.Phase, inferencev1alpha1.PhaseStopped)
	}
	normalSample(a, 60)
	a.mu.RLock()
	reserved := len(a.pressureRestarting)
	a.mu.RUnlock()
	if reserved != 0 {
		t.Errorf("%d cancelled restart(s) still hold RSS reserved, want 0", reserved)
	}
}

// TestHandleMemoryPressure_PressureRiseBlocksQueuedRestart covers pressure
// rising after restarts were queued: the loop starts queued services back to
// back, later than they were queued, so a service whose turn comes after
// pressure left Normal must go back to blocked, not start, and not count as
// a failed restart (no RestartFailed event, no retry).
func TestHandleMemoryPressure_PressureRiseBlocksQueuedRestart(t *testing.T) {
	a, ex, rec := refusalFixture(t, MetalAgentConfig{},
		evictedISVC("svc-a"), evictedISVC("svc-b"), refusalModel())
	ctx := context.Background()
	// svc-a starts first (keys run in sorted order), and its model load
	// drives the host back to Critical.
	ex.onStart = func() {
		a.handleMemoryPressure(ctx, MemoryPressureCritical, MemoryStats{TotalMemory: 100 << 30})
	}
	blockEvicted(a, 0, "default/svc-a", "default/svc-b")
	normalSample(a, 60)
	_ = drainEvents(rec)

	a.runDueRestarts(ctx)

	if ex.starts != 1 || ex.lastCfg.Name != "svc-a" {
		t.Errorf("starts = %d (last %q), want only svc-a started", ex.starts, ex.lastCfg.Name)
	}
	if !a.isPressureBlocked("default/svc-b") || restartPending(a, "default/svc-b") {
		t.Errorf("svc-b blocked = %v, restart pending = %v; want true, false",
			a.isPressureBlocked("default/svc-b"), restartPending(a, "default/svc-b"))
	}
	if got := memoryPressureCondition(t, a, "svc-b"); got != conditionEvicted {
		t.Errorf("svc-b MemoryPressure = %q, want %q while it is blocked again", got, conditionEvicted)
	}
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, EventReasonRestartFailed) {
			t.Errorf("a pressure-held restart was reported failed: %s", e)
		}
	}
}

// TestQueueEvictedRestarts_DeletedBeforeQueueNeverStarts covers the ownership
// race: a DELETED event (the service deleted, or no longer claimed by this
// agent while the object still exists) handled after a blocked service was
// picked but before its restart is queued. The restart must not be queued,
// or runRestart would start a service this agent no longer owns.
func TestQueueEvictedRestarts_DeletedBeforeQueueNeverStarts(t *testing.T) {
	isvc := refusalISVC("svc-a")
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	ctx := context.Background()
	blockEvicted(a, 0, "default/svc-a")

	a.mu.Lock()
	a.lastPressureLevel = MemoryPressureNormal
	picked := a.restartCandidatesLocked(MemoryStats{TotalMemory: 100 << 30, AvailableMemory: 60 << 30}, true)
	a.mu.Unlock()
	if len(picked) != 1 {
		t.Fatalf("setup: picked %d services, want 1", len(picked))
	}
	if err := a.handleEvent(ctx, InferenceServiceEvent{Type: EventTypeDeleted, InferenceService: isvc}); err != nil {
		t.Fatalf("DELETED: %v", err)
	}
	a.queueEvictedRestarts(ctx, picked)
	a.runDueRestarts(ctx)

	if restartPending(a, "default/svc-a") || ex.starts != 0 {
		t.Errorf("restart pending = %v, starts = %d; want false, 0", restartPending(a, "default/svc-a"), ex.starts)
	}
}

// TestQueueEvictedRestarts_SkipsStartingOrOwedKeys covers a picked service
// whose key is starting, or already owed a restart: queueing it would mark a
// restart pending under a start in progress (a spurious RestartFailed later).
// It stays blocked for a later sample.
func TestQueueEvictedRestarts_SkipsStartingOrOwedKeys(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(a *MetalAgent)
	}{
		{"starting", func(a *MetalAgent) { a.starting["default/svc-a"] = true }},
		{"owed a restart", func(a *MetalAgent) {
			a.restartStateLocked("default/svc-a").pending = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := refusalFixture(t, MetalAgentConfig{}, refusalISVC("svc-a"), refusalModel())
			blockEvicted(a, 0, "default/svc-a")
			a.mu.Lock()
			tc.setup(a)
			a.mu.Unlock()

			normalSample(a, 60)

			a.mu.RLock()
			_, queued := a.pressureRestarting["default/svc-a"]
			a.mu.RUnlock()
			if !a.isPressureBlocked("default/svc-a") || queued {
				t.Errorf("blocked = %v, queued = %v; want true, false", a.isPressureBlocked("default/svc-a"), queued)
			}
		})
	}
}

// TestHandleMemoryPressure_RestartWaitsForHeadroom covers the evict and restart
// loop admission cannot stop: admission compares one model's estimate with a
// fraction of total memory, never with what is available. A service whose
// eviction-time RSS would leave less than the Warning floor available stays
// blocked, and keeps its Evicted condition, until a later Normal sample shows
// enough room.
func TestHandleMemoryPressure_RestartWaitsForHeadroom(t *testing.T) {
	a, ex := headroomFixture(t, evictedISVC("svc-a"), refusalModel())
	ctx := context.Background()
	blockEvicted(a, 30, "default/svc-a")

	normalSample(a, 45) // 45 - 30 = 15 GiB left, under the 20 GiB floor
	a.runDueRestarts(ctx)
	if ex.starts != 0 || !a.isPressureBlocked("default/svc-a") {
		t.Fatalf("with 15 GiB left: starts = %d, blocked = %v; want 0, true",
			ex.starts, a.isPressureBlocked("default/svc-a"))
	}
	if got := memoryPressureCondition(t, a, "svc-a"); got != conditionEvicted {
		t.Errorf("MemoryPressure = %q, want %q while svc-a waits", got, conditionEvicted)
	}

	normalSample(a, 55) // 55 - 30 = 25 GiB left
	a.runDueRestarts(ctx)
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
	a.runDueRestarts(context.Background())

	if got, want := runningKeys(a), []string{"default/svc-a"}; !slices.Equal(got, want) || ex.starts != 1 {
		t.Errorf("running = %v after %d start(s), want %v after 1", got, ex.starts, want)
	}
	if !a.isPressureBlocked("default/svc-b") {
		t.Error("svc-b must stay blocked: with svc-a back there is no room for it")
	}
}

// TestHandleMemoryPressure_QueuedRestartReservesHeadroom covers a later sample
// while a restart is still queued: available memory does not show what the
// queued service will take yet, so its RSS stays reserved and a second
// service is not picked against it. Once the first engine runs its RSS is no
// longer reserved.
func TestHandleMemoryPressure_QueuedRestartReservesHeadroom(t *testing.T) {
	a, ex := headroomFixture(t, evictedISVC("svc-a"), evictedISVC("svc-b"), refusalModel())
	blockEvicted(a, 20, "default/svc-a", "default/svc-b")
	normalSample(a, 55) // svc-a queued
	if !restartPending(a, "default/svc-a") || !a.isPressureBlocked("default/svc-b") {
		t.Fatal("setup: want svc-a queued and svc-b blocked")
	}

	normalSample(a, 55) // 55 - 20 reserved - 20 = 15 GiB, under the floor
	if !a.isPressureBlocked("default/svc-b") {
		t.Fatal("svc-b was picked against the RSS svc-a's queued restart has reserved")
	}

	a.runDueRestarts(context.Background()) // svc-a runs; its RSS now shows in available
	normalSample(a, 45)                    // 45 - 20 = 25 GiB
	if a.isPressureBlocked("default/svc-b") || ex.starts != 1 {
		t.Errorf("after svc-a started: svc-b blocked = %v, starts = %d; want false, 1",
			a.isPressureBlocked("default/svc-b"), ex.starts)
	}
}

// TestPickRestartable pins the headroom arithmetic: services are taken in key
// order while their summed RSS, plus what is already reserved, leaves at
// least floor bytes available; one that does not fit, or whose backoff has
// not passed, is skipped, and a later one may still fit.
func TestPickRestartable(t *testing.T) {
	const gib = 1 << 30
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	tests := []struct {
		name      string
		blocked   map[string]uint64 // key -> RSS in GiB
		waiting   string            // key whose notBefore is still ahead
		available uint64            // GiB
		reserved  uint64            // GiB
		want      []string
	}{
		{"fits", map[string]uint64{"a": 30}, "", 55, 0, []string{"a"}},
		{"under the floor", map[string]uint64{"a": 30}, "", 45, 0, nil},
		{"exactly the floor", map[string]uint64{"a": 30}, "", 50, 0, []string{"a"}},
		{"second does not fit with the first", map[string]uint64{"a": 20, "b": 20}, "", 55, 0, []string{"a"}},
		{"smaller later one fits", map[string]uint64{"a": 40, "b": 10}, "", 45, 0, []string{"b"}},
		{"unknown RSS reserves nothing", map[string]uint64{"a": 0}, "", 20, 0, []string{"a"}},
		{"more than available", map[string]uint64{"a": 90}, "", 60, 0, nil},
		{"reserved counts", map[string]uint64{"a": 20}, "", 55, 20, nil},
		{"backoff not passed", map[string]uint64{"a": 10, "b": 10}, "a", 55, 0, []string{"b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocked := make(map[string]evictedService, len(tt.blocked))
			for key, rss := range tt.blocked {
				svc := evictedService{rss: rss * gib}
				if key == tt.waiting {
					svc.notBefore = later
				}
				blocked[key] = svc
			}
			got := pickRestartable(blocked, now, tt.available*gib, 20*gib, tt.reserved*gib)
			if !slices.Equal(got, tt.want) {
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
	provider := &mockMemoryProvider{totalBytes: 100 << 30}
	w := NewMemoryWatchdog(provider, a.processMemInfoSnapshot,
		func(level MemoryPressureLevel, stats MemoryStats) {
			a.handleMemoryPressure(context.Background(), level, stats)
		}, *a.config.WatchdogConfig, newNopLogger())
	w.wantNormal = a.hasEvictedWaiting

	sample := func(availGiB uint64) {
		t.Helper()
		provider.availableBytes = availGiB << 30
		w.sample()
		a.runDueRestarts(context.Background())
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

// TestHandleMemoryPressure_ReEvictionBacksOff covers a service evicted again
// soon after its restart: its next restart waits 1 minute, then 2, doubling
// on each quick re-eviction, and starts from no wait again once it has stayed
// up for 10 minutes.
func TestHandleMemoryPressure_ReEvictionBacksOff(t *testing.T) {
	low := refusalISVC("svc-low")
	low.Spec.Priority = "low"
	high := refusalISVC("svc-high")
	high.Spec.Priority = "high"
	a, _, _ := refusalFixture(t, MetalAgentConfig{EvictionEnabled: true}, low, high, refusalModel())
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	ctx := context.Background()
	for _, isvc := range []*inferencev1alpha1.InferenceService{low, high} {
		if err := a.ensureProcess(ctx, isvc); err != nil {
			t.Fatalf("start %s: %v", isvc.Name, err)
		}
	}
	const key = "default/svc-low"
	evict := func() time.Duration {
		t.Helper()
		a.handleMemoryPressure(ctx, MemoryPressureCritical, MemoryStats{TotalMemory: 100 << 30, TotalRSS: 80 << 30})
		a.mu.RLock()
		svc, blocked := a.pressureBlocked[key]
		a.mu.RUnlock()
		if !blocked {
			t.Fatal("svc-low was not evicted")
		}
		if svc.notBefore.IsZero() {
			return 0
		}
		return svc.notBefore.Sub(clock)
	}
	restart := func() {
		t.Helper()
		normalSample(a, 60)
		a.runDueRestarts(ctx)
		if !slices.Contains(runningKeys(a), key) {
			t.Fatal("svc-low was not restarted")
		}
	}

	if wait := evict(); wait != 0 {
		t.Errorf("first eviction: wait = %v, want 0", wait)
	}
	restart()
	clock = clock.Add(2 * time.Minute)
	if wait := evict(); wait != time.Minute {
		t.Errorf("evicted 2m after its restart: wait = %v, want 1m", wait)
	}
	normalSample(a, 60)
	if !a.isPressureBlocked(key) {
		t.Fatal("svc-low restarted before its 1m wait passed")
	}
	clock = clock.Add(time.Minute)
	restart()
	clock = clock.Add(time.Minute)
	if wait := evict(); wait != 2*time.Minute {
		t.Errorf("evicted again 1m after its restart: wait = %v, want 2m", wait)
	}
	clock = clock.Add(2 * time.Minute)
	restart()
	clock = clock.Add(10 * time.Minute)
	if wait := evict(); wait != 0 {
		t.Errorf("evicted after 10m up: wait = %v, want 0 (backoff reset)", wait)
	}
}

// TestNextEvictedRestart_CapsWait pins the cap: a quick re-eviction never
// waits more than evictedRestartMaxWait.
func TestNextEvictedRestart_CapsWait(t *testing.T) {
	a, _, _ := refusalFixture(t, MetalAgentConfig{})
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	a.evictionBackoff["default/svc-a"] = &evictionBackoff{restartedAt: clock.Add(-time.Minute), wait: 20 * time.Minute}

	a.mu.Lock()
	got := a.nextEvictedRestartLocked("default/svc-a").Sub(clock)
	a.mu.Unlock()
	if got != evictedRestartMaxWait {
		t.Errorf("wait = %v, want the %v cap", got, evictedRestartMaxWait)
	}
}

// TestHandleEvent_DeletedForgetsEvictedService covers the watcher reporting a
// blocked service gone (deleted, or no longer claimed by this agent): its
// block and backoff are dropped, so nothing keeps waiting for room for it.
func TestHandleEvent_DeletedForgetsEvictedService(t *testing.T) {
	isvc := refusalISVC("svc-a")
	a, _, _ := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	blockEvicted(a, 10, "default/svc-a")
	a.evictionBackoff["default/svc-a"] = &evictionBackoff{restartedAt: time.Now()}

	if err := a.handleEvent(context.Background(),
		InferenceServiceEvent{Type: EventTypeDeleted, InferenceService: isvc}); err != nil {
		t.Fatalf("DELETED: %v", err)
	}

	a.mu.RLock()
	_, backoff := a.evictionBackoff["default/svc-a"]
	a.mu.RUnlock()
	if a.isPressureBlocked("default/svc-a") || backoff || a.hasEvictedWaiting() {
		t.Errorf("blocked = %v, backoff kept = %v after DELETED; want false, false",
			a.isPressureBlocked("default/svc-a"), backoff)
	}
}

// TestHandleMemoryPressure_RestartKeepsBlockOnReadError covers a failed read of
// a picked InferenceService: the service stays blocked, so a later Normal
// sample retries it, instead of being dropped and left down.
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

	if ex.starts != 0 || !a.isPressureBlocked("default/svc-a") || restartPending(a, "default/svc-a") {
		t.Errorf("starts = %d, blocked = %v, restart pending = %v; want 0, true, false",
			ex.starts, a.isPressureBlocked("default/svc-a"), restartPending(a, "default/svc-a"))
	}
}

// TestEnsureProcess_BlockedServiceWaitsAtNormal covers a watch event (any write
// to the InferenceService, status included) for a service still waiting for
// room at Normal pressure: the block holds, or the event would start the
// service around the headroom gate.
func TestEnsureProcess_BlockedServiceWaitsAtNormal(t *testing.T) {
	isvc := refusalISVC("svc-a")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	blockEvicted(a, 30, "default/svc-a")
	a.mu.Lock()
	a.lastPressureLevel = MemoryPressureNormal
	a.mu.Unlock()

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}

	if ex.starts != 0 || !a.isPressureBlocked("default/svc-a") {
		t.Errorf("starts = %d, blocked = %v; want 0, true", ex.starts, a.isPressureBlocked("default/svc-a"))
	}
	if events := strings.Join(drainEvents(rec), "\n"); !strings.Contains(events, EventReasonRespawnBlocked) {
		t.Errorf("events = %q, want a %s event", events, EventReasonRespawnBlocked)
	}
}
