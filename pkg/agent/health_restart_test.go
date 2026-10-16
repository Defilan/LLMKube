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
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests cover #2077: a health restart stops the unhealthy engine before
// it starts a new one, waits for consecutive failures, and backs off.

// restartExecutor records StopProcess and StartProcess calls in order. Each
// successful start returns a healthy process with the next PID (1, 2, ...).
// onStop, when set, runs inside StopProcess, so a test can deliver a watch
// event while an engine is stopping.
type restartExecutor struct {
	mu       sync.Mutex
	calls    []string
	pid      int
	now      func() time.Time
	startErr error
	stopErr  error
	onStop   func()
}

func (e *restartExecutor) StartProcess(_ context.Context, cfg ExecutorConfig) (*ManagedProcess, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, "start")
	if e.startErr != nil {
		return nil, e.startErr
	}
	e.pid++
	return &ManagedProcess{
		Name: cfg.Name, Namespace: cfg.Namespace, PID: e.pid, Port: 19000 + e.pid,
		Healthy: true, StartedAt: e.now(),
	}, nil
}

func (e *restartExecutor) StopProcess(pid int) error {
	e.mu.Lock()
	e.calls = append(e.calls, fmt.Sprintf("stop %d", pid))
	onStop, err := e.onStop, e.stopErr
	e.mu.Unlock()
	if onStop != nil {
		onStop()
	}
	return err
}

func (e *restartExecutor) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *restartExecutor) starts() int {
	n := 0
	for _, c := range e.recorded() {
		if c == "start" {
			n++
		}
	}
	return n
}

// restartFixture is an agent tracking one healthy engine (PID 1111) for the
// InferenceService isvc, with a fake clock, a failing health checker and a
// restartExecutor.
type restartFixture struct {
	agent   *MetalAgent
	exec    *restartExecutor
	checker *mockHealthChecker
	monitor *HealthMonitor
	rec     *record.FakeRecorder
	isvc    *inferencev1alpha1.InferenceService
	key     string
	clock   time.Time
}

func newRestartFixture(t *testing.T, name string) *restartFixture {
	t.Helper()
	model := ggufModel(t, name)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name},
	}
	scheme := newTestScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithObjects(model, isvc).
		Build()
	rec := record.NewFakeRecorder(256)
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:         c,
		Namespace:         "default",
		MemoryProvider:    &mockMemoryProvider{totalBytes: 128 << 30, availableBytes: 120 << 30},
		AllowedModelRoots: []string{filepath.Dir(model.Spec.Source)},
		EventRecorder:     rec,
	})
	a.registry = NewServiceRegistry(c, "10.0.0.1", newNopLogger(), "")

	f := &restartFixture{
		agent: a,
		rec:   rec,
		isvc:  isvc,
		key:   "default/" + name,
		clock: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
	}
	a.now = func() time.Time { return f.clock }
	f.exec = &restartExecutor{now: a.now}
	a.executors[runtimeLlamaServer] = f.exec
	a.executors[runtimeLlamaCPP] = f.exec
	a.processes[f.key] = &ManagedProcess{
		Name: name, Namespace: "default", PID: 1111, Port: 18080, Healthy: true,
		StartedAt: f.clock, Runtime: runtimeLlamaServer, SpecHash: computeSpecHash(isvc),
	}
	f.checker = &mockHealthChecker{result: false}
	f.monitor = NewHealthMonitor(a, f.checker, time.Second, newNopLogger())
	return f
}

// probe runs n health check cycles.
func (f *restartFixture) probe(n int) {
	for range n {
		f.monitor.checkAll(context.Background())
	}
}

func (f *restartFixture) advance(d time.Duration) {
	f.clock = f.clock.Add(d)
}

// tracked returns the process the agent tracks for the fixture's service.
func (f *restartFixture) tracked() *ManagedProcess {
	f.agent.mu.RLock()
	defer f.agent.mu.RUnlock()
	return f.agent.processes[f.key]
}

// sawEvent drains the recorder and reports whether an event with reason was
// emitted.
func (f *restartFixture) sawEvent(reason string) bool {
	found := false
	for {
		select {
		case e := <-f.rec.Events:
			if strings.Contains(e, " "+reason+" ") {
				found = true
			}
		default:
			return found
		}
	}
}

// TestHealthRestart_StopsUnhealthyEngineBeforeStartingNew is the #2077 bug:
// the restart used to start a second engine next to the unhealthy one, which
// kept running untracked with its weights still wired.
func TestHealthRestart_StopsUnhealthyEngineBeforeStartingNew(t *testing.T) {
	f := newRestartFixture(t, "restart-stops-first")
	restartsBefore := testutil.ToFloat64(processRestarts.WithLabelValues(f.isvc.Name, "default"))

	f.probe(DefaultHealthFailureThreshold)
	if p := f.tracked(); p != nil {
		t.Fatalf("unhealthy engine PID %d still tracked after %d failed probes", p.PID, DefaultHealthFailureThreshold)
	}
	select {
	case <-f.agent.restartWake:
	default:
		t.Fatal("no wake-up queued for the event loop, which starts the replacement")
	}
	f.agent.runDueRestarts(context.Background())

	if got, want := f.exec.recorded(), []string{"stop 1111", "start"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("executor calls = %v, want %v (the old engine stopped before the new one starts)", got, want)
	}
	p := f.tracked()
	if p == nil || p.PID != 1 || !p.Healthy {
		t.Fatalf("tracked process after restart = %+v, want the healthy replacement (PID 1)", p)
	}
	if got := testutil.ToFloat64(processRestarts.WithLabelValues(f.isvc.Name, "default")) - restartsBefore; got != 1 {
		t.Errorf("process_restarts_total grew by %v, want 1", got)
	}
	if !f.sawEvent(EventReasonHealthRestart) {
		t.Errorf("no %s event emitted for the restart", EventReasonHealthRestart)
	}
}

// TestHealthRestart_WaitsForConsecutiveFailures checks the failure threshold:
// failures broken up by a success never restart the engine, and the third
// failure in a row does.
func TestHealthRestart_WaitsForConsecutiveFailures(t *testing.T) {
	f := newRestartFixture(t, "restart-threshold")

	f.probe(2)
	f.checker.result = true
	f.probe(1)
	f.checker.result = false
	f.probe(2)
	f.agent.runDueRestarts(context.Background())
	if got := f.exec.recorded(); len(got) != 0 {
		t.Fatalf("executor calls = %v after two runs of two failed probes, want none", got)
	}
	if p := f.tracked(); p == nil || p.PID != 1111 {
		t.Fatalf("tracked process = %+v, want the original engine (PID 1111)", p)
	}

	f.probe(1)
	f.agent.runDueRestarts(context.Background())
	if got, want := f.exec.recorded(), []string{"stop 1111", "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("executor calls = %v after three consecutive failed probes, want %v", got, want)
	}
}

// TestHealthRestart_ConfiguredThreshold checks that --health-failure-threshold
// (MetalAgentConfig.HealthFailureThreshold) replaces the default.
func TestHealthRestart_ConfiguredThreshold(t *testing.T) {
	f := newRestartFixture(t, "restart-configured")
	f.agent.config.HealthFailureThreshold = 1

	f.probe(1)
	f.agent.runDueRestarts(context.Background())
	if got, want := f.exec.recorded(), []string{"stop 1111", "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("executor calls = %v after one failed probe with threshold 1, want %v", got, want)
	}
}

// TestHealthRestart_FailedRestartRetriesWithGrowingBackoff checks that a
// restart whose start fails is retried without any InferenceService change,
// after 10s, then 20s, and stops retrying once an engine is up.
func TestHealthRestart_FailedRestartRetriesWithGrowingBackoff(t *testing.T) {
	f := newRestartFixture(t, "restart-backoff")
	f.exec.startErr = errors.New("llama-server exited during startup")
	ctx := context.Background()

	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d after the restart, want 1", got)
	}
	if !f.sawEvent(EventReasonRestartFailed) {
		t.Errorf("no %s event emitted for the failed restart", EventReasonRestartFailed)
	}

	steps := []struct {
		advance    time.Duration
		wantStarts int
	}{
		{9 * time.Second, 1},  // inside the first 10s backoff
		{1 * time.Second, 2},  // 10s: first retry
		{19 * time.Second, 2}, // inside the doubled 20s backoff
		{1 * time.Second, 3},  // 20s: second retry
	}
	for i, s := range steps {
		f.advance(s.advance)
		f.agent.runDueRestarts(ctx)
		if got := f.exec.starts(); got != s.wantStarts {
			t.Fatalf("step %d (+%s): starts = %d, want %d", i, s.advance, got, s.wantStarts)
		}
	}

	f.exec.startErr = nil
	f.advance(40 * time.Second)
	f.agent.runDueRestarts(ctx)
	if p := f.tracked(); p == nil || !p.Healthy {
		t.Fatalf("no healthy engine tracked after the retry succeeded: %+v", p)
	}
	f.advance(time.Hour)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 4 {
		t.Errorf("starts = %d an hour after a successful retry, want 4 (no further retries)", got)
	}
}

// TestHealthRestart_DeleteCancelsPendingRetry checks that a service the
// watcher reports deleted is not started again by a pending retry. The
// watcher reports a delete for a service that still exists once it stops
// matching this agent (allowlist, accelerator), so the retry's own lookup of
// the InferenceService cannot be what stops it.
func TestHealthRestart_DeleteCancelsPendingRetry(t *testing.T) {
	f := newRestartFixture(t, "restart-delete")
	f.exec.startErr = errors.New("llama-server exited during startup")
	ctx := context.Background()

	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)
	deleted := InferenceServiceEvent{Type: EventTypeDeleted, InferenceService: f.isvc}
	if err := f.agent.handleEvent(ctx, deleted); err != nil {
		t.Fatalf("handleEvent(DELETED): %v", err)
	}

	f.exec.startErr = nil
	f.advance(time.Hour)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Errorf("starts = %d after the delete, want 1 (only the restart before it)", got)
	}
	if p := f.tracked(); p != nil {
		t.Errorf("a pending retry started deleted service again: PID %d", p.PID)
	}
}

// TestHealthRestart_DeleteWhileStoppingSchedulesNoRestart covers a delete the
// event loop handles while the health monitor is still stopping the unhealthy
// engine: there is no pending restart to cancel yet, and none may be
// scheduled once the stop returns.
func TestHealthRestart_DeleteWhileStoppingSchedulesNoRestart(t *testing.T) {
	f := newRestartFixture(t, "restart-delete-stopping")
	ctx := context.Background()
	f.exec.onStop = func() {
		deleted := InferenceServiceEvent{Type: EventTypeDeleted, InferenceService: f.isvc}
		if err := f.agent.handleEvent(ctx, deleted); err != nil {
			t.Errorf("handleEvent(DELETED): %v", err)
		}
	}

	f.probe(DefaultHealthFailureThreshold)
	f.advance(time.Hour)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 0 {
		t.Errorf("starts = %d after a delete during the stop, want 0", got)
	}
	if p := f.tracked(); p != nil {
		t.Errorf("deleted service started again: PID %d", p.PID)
	}
}

// TestHealthRestart_ShutdownCancelsPendingRetry checks that shutdown cancels
// a pending retry.
func TestHealthRestart_ShutdownCancelsPendingRetry(t *testing.T) {
	f := newRestartFixture(t, "restart-shutdown")
	f.exec.startErr = errors.New("llama-server exited during startup")
	ctx := context.Background()

	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)
	if err := f.agent.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	f.exec.startErr = nil
	f.advance(time.Hour)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Errorf("starts = %d after shutdown, want 1 (only the restart before it)", got)
	}
}

// TestHealthRestart_BackoffResetsAfterHealthyRun checks that a crash loop's
// backoff applies to an engine that fails again soon, and is forgotten once
// an engine has run healthy for ten minutes.
func TestHealthRestart_BackoffResetsAfterHealthyRun(t *testing.T) {
	f := newRestartFixture(t, "restart-reset")
	ctx := context.Background()

	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d after the first restart, want 1", got)
	}

	// The replacement fails again a minute later: this restart waits 10s.
	f.advance(time.Minute)
	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d right after a second failure, want 1 (backed off)", got)
	}
	f.advance(healthRestartInitialBackoff)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 2 {
		t.Fatalf("starts = %d after the 10s backoff, want 2", got)
	}

	// This one stays healthy for ten minutes, then fails: it restarts at once.
	f.checker.result = true
	f.advance(healthRestartBackoffReset)
	f.probe(1)
	f.checker.result = false
	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 3 {
		t.Errorf("starts = %d after a failure that followed a healthy run, want 3 (backoff reset)", got)
	}
}

// TestHealthRestart_StopErrorDelaysStart checks that an engine that did not
// stop cleanly (for example one that had to be killed) gets the initial
// backoff to release its memory before a new engine loads the weights again.
func TestHealthRestart_StopErrorDelaysStart(t *testing.T) {
	f := newRestartFixture(t, "restart-stop-error")
	f.exec.stopErr = errors.New("process 1111 did not exit gracefully, killed")
	ctx := context.Background()

	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 0 {
		t.Fatalf("starts = %d right after a stop error, want 0", got)
	}
	f.advance(healthRestartInitialBackoff)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Errorf("starts = %d after the initial backoff, want 1", got)
	}
}

// replacingChecker fails every probe of oldPort and, during the first one,
// replaces the tracked process with replacement, as a spec change handled on
// the event loop would while the probe is in flight.
type replacingChecker struct {
	agent       *MetalAgent
	key         string
	oldPort     int
	replacement *ManagedProcess
	replaced    bool
}

func (c *replacingChecker) Check(_ context.Context, port int, _ string) (bool, error) {
	if port != c.oldPort {
		return true, nil
	}
	if !c.replaced {
		c.agent.mu.Lock()
		c.agent.processes[c.key] = c.replacement
		c.agent.mu.Unlock()
		c.replaced = true
	}
	return false, nil
}

// TestHealthMonitor_IgnoresResultForReplacedProcess checks that a failed
// probe of an engine that was replaced while the probe ran is not counted
// against the new engine. With a threshold of 1, counting it would stop the
// freshly started engine.
func TestHealthMonitor_IgnoresResultForReplacedProcess(t *testing.T) {
	f := newRestartFixture(t, "restart-replaced")
	f.agent.config.HealthFailureThreshold = 1
	replacement := &ManagedProcess{
		Name: f.isvc.Name, Namespace: "default", PID: 2222, Port: 18081, Healthy: true,
		StartedAt: f.clock, Runtime: runtimeLlamaServer, SpecHash: computeSpecHash(f.isvc),
	}
	f.monitor = NewHealthMonitor(f.agent, &replacingChecker{
		agent: f.agent, key: f.key, oldPort: 18080, replacement: replacement,
	}, time.Second, newNopLogger())

	f.probe(1)
	f.agent.runDueRestarts(context.Background())

	if got := f.exec.recorded(); len(got) != 0 {
		t.Errorf("executor calls = %v, want none: the failure belonged to the replaced engine", got)
	}
	f.agent.mu.RLock()
	healthy, failures := replacement.Healthy, replacement.healthFailures
	f.agent.mu.RUnlock()
	if p := f.tracked(); p != replacement || !healthy || failures != 0 {
		t.Errorf("replacement tracked=%v healthy=%v failures=%d, want tracked, healthy, 0",
			p == replacement, healthy, failures)
	}
}

// TestEnsureProcess_LeavesUnhealthyProcessToHealthMonitor covers the watch
// event path of #2077: a status change on the InferenceService (the endpoint
// withdrawal causes one) used to reach reconcileProcess for the unhealthy
// engine and start a second one beside it. With the same spec it must leave
// the engine to the health monitor; with a new spec it must stop it first.
func TestEnsureProcess_LeavesUnhealthyProcessToHealthMonitor(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*inferencev1alpha1.InferenceService)
		wantCalls []string
		wantPID   int
	}{
		{"same spec", func(*inferencev1alpha1.InferenceService) {}, nil, 1111},
		{"spec changed", func(isvc *inferencev1alpha1.InferenceService) {
			isvc.Spec.ContextSize = ptr.To(int32(8192))
		}, []string{"stop 1111", "start"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t, "unhealthy-"+strings.ReplaceAll(tc.name, " ", "-"))
			f.agent.processes[f.key].Healthy = false
			isvc := f.isvc.DeepCopy()
			tc.mutate(isvc)

			if err := f.agent.ensureProcess(context.Background(), isvc); err != nil {
				t.Fatalf("ensureProcess: %v", err)
			}
			if got := f.exec.recorded(); !reflect.DeepEqual(got, tc.wantCalls) {
				t.Errorf("executor calls = %v, want %v", got, tc.wantCalls)
			}
			if p := f.tracked(); p == nil || p.PID != tc.wantPID {
				t.Errorf("tracked process = %+v, want PID %d", p, tc.wantPID)
			}
		})
	}
}
