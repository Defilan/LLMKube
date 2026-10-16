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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	client  client.Client
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
	return newRestartFixtureWith(t, name, nil)
}

// newRestartFixtureWith is newRestartFixture with interceptor funcs, when
// non-nil, installed on the fake client.
func newRestartFixtureWith(t *testing.T, name string, funcs *interceptor.Funcs) *restartFixture {
	t.Helper()
	model := ggufModel(t, name)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name},
	}
	scheme := newTestScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	b := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithObjects(model, isvc)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
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
		agent:  a,
		client: c,
		rec:    rec,
		isvc:   isvc,
		key:    "default/" + name,
		clock:  time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
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

// endpointExists reports whether the agent's Service for the fixture's
// InferenceService exists.
func (f *restartFixture) endpointExists(t *testing.T) bool {
	t.Helper()
	key := types.NamespacedName{Namespace: "default", Name: f.agent.registry.serviceNameFor(f.isvc.Name)}
	err := f.client.Get(context.Background(), key, &corev1.Service{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get Service %s: %v", key, err)
	}
	return err == nil
}

// updateISVC applies mutate to the stored InferenceService and returns the
// updated object, as a watch event would deliver it.
func (f *restartFixture) updateISVC(
	t *testing.T, mutate func(*inferencev1alpha1.InferenceService),
) *inferencev1alpha1.InferenceService {
	t.Helper()
	cur := &inferencev1alpha1.InferenceService{}
	key := types.NamespacedName{Namespace: "default", Name: f.isvc.Name}
	if err := f.client.Get(context.Background(), key, cur); err != nil {
		t.Fatalf("get InferenceService: %v", err)
	}
	mutate(cur)
	if err := f.client.Update(context.Background(), cur); err != nil {
		t.Fatalf("update InferenceService: %v", err)
	}
	return cur
}

// failFirstRestart drives the fixture to a pending retry: the engine fails
// its health checks, is stopped, and the restart's start fails, so the next
// attempt is due healthRestartInitialBackoff from now.
func (f *restartFixture) failFirstRestart(t *testing.T) {
	t.Helper()
	f.exec.startErr = errors.New("llama-server exited during startup")
	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(context.Background())
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d after the first restart, want 1", got)
	}
	f.exec.startErr = nil
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

	if f.endpointExists(t) {
		t.Errorf("the withdrawn endpoint of the stopped engine survived the delete")
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
	if f.endpointExists(t) {
		t.Errorf("the withdrawn endpoint of the stopped engine survived the delete")
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
	old := f.tracked()
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
	f.agent.mu.RLock()
	oldHealthy, oldFailures := old.Healthy, old.healthFailures
	f.agent.mu.RUnlock()
	if !oldHealthy || oldFailures != 0 {
		t.Errorf("replaced process healthy=%v failures=%d, want untouched (true, 0)", oldHealthy, oldFailures)
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

// TestHealthMonitor_RecoveryOfRemovedProcessRegistersNothing covers a
// recovery whose process is removed (by a delete on the event loop) while the
// monitor fetches the InferenceService to re-register it. It used to read the
// port through the missing map entry and panic, taking the agent down; it
// must also not bring back an endpoint the delete removed.
func TestHealthMonitor_RecoveryOfRemovedProcessRegistersNothing(t *testing.T) {
	var f *restartFixture
	armed := false
	funcs := &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*inferencev1alpha1.InferenceService); ok && armed {
				armed = false
				f.agent.mu.Lock()
				delete(f.agent.processes, f.key)
				f.agent.mu.Unlock()
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	f = newRestartFixtureWith(t, "recover-removed", funcs)
	f.agent.processes[f.key].Healthy = false
	f.checker.result = true
	armed = true

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("recovery of a removed process panicked: %v", r)
			}
		}()
		f.probe(1)
	}()
	if armed {
		t.Fatal("the recovery never fetched the InferenceService; the test did not exercise the window")
	}
	if f.endpointExists(t) {
		t.Errorf("recovery registered an endpoint for a process that is no longer tracked")
	}
}

// cancellingChecker cancels the probe's context during the check, as agent
// shutdown does, and fails the probe the way a cancelled request fails.
type cancellingChecker struct {
	cancel context.CancelFunc
}

func (c *cancellingChecker) Check(ctx context.Context, _ int, _ string) (bool, error) {
	c.cancel()
	return false, ctx.Err()
}

// TestHealthMonitor_ProbeCutShortByShutdownIsNotAFailure checks that a probe
// that fails because the agent is shutting down is not counted: with a
// threshold of 1 it would stop the engine with a cancelled context, which
// leaves an oMLX or Ollama model loaded.
func TestHealthMonitor_ProbeCutShortByShutdownIsNotAFailure(t *testing.T) {
	f := newRestartFixture(t, "probe-shutdown")
	f.agent.config.HealthFailureThreshold = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monitor := NewHealthMonitor(f.agent, &cancellingChecker{cancel: cancel}, time.Second, newNopLogger())

	monitor.checkAll(ctx)

	if got := f.exec.recorded(); len(got) != 0 {
		t.Errorf("executor calls = %v after a probe cut short by shutdown, want none", got)
	}
	f.agent.mu.RLock()
	p := f.agent.processes[f.key]
	healthy := p != nil && p.Healthy
	f.agent.mu.RUnlock()
	if !healthy {
		t.Errorf("engine tracked=%v healthy=%v after a cancelled probe, want tracked and healthy", p != nil, healthy)
	}
}

// TestHealthRestart_WatchEventDuringBackoffDoesNotStart checks that a watch
// event (any write to the InferenceService, status included, emits one) does
// not start the engine while its restart is backing off.
func TestHealthRestart_WatchEventDuringBackoffDoesNotStart(t *testing.T) {
	f := newRestartFixture(t, "restart-watch-backoff")
	ctx := context.Background()
	f.failFirstRestart(t)

	f.advance(5 * time.Second)
	if err := f.agent.ensureProcess(ctx, f.isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d after a watch event 5s into a 10s backoff, want 1", got)
	}

	f.advance(5 * time.Second)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 2 {
		t.Errorf("starts = %d when the backoff expired, want 2", got)
	}
}

// TestHealthRestart_SpecChangeResetsBackoff checks that a new spec does not
// wait out the backoff earned by the old one: it starts on the next loop turn.
func TestHealthRestart_SpecChangeResetsBackoff(t *testing.T) {
	f := newRestartFixture(t, "restart-spec-reset")
	ctx := context.Background()
	f.failFirstRestart(t)

	changed := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.ContextSize = ptr.To(int32(8192))
	})
	if err := f.agent.ensureProcess(ctx, changed); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	f.agent.runDueRestarts(ctx)

	if got := f.exec.starts(); got != 2 {
		t.Fatalf("starts = %d right after a spec change during the backoff, want 2", got)
	}
	if p := f.tracked(); p == nil || p.SpecHash != computeSpecHash(changed) {
		t.Errorf("tracked process = %+v, want one started with the new spec", p)
	}
}

// TestHealthRestart_ScaleToZeroDuringBackoff checks that replicas=0 during the
// backoff cancels the restart and removes the endpoint the stopped engine
// left withdrawn.
func TestHealthRestart_ScaleToZeroDuringBackoff(t *testing.T) {
	f := newRestartFixture(t, "restart-scale-backoff")
	ctx := context.Background()
	f.failFirstRestart(t)

	stopped := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.Replicas = ptr.To(int32(0))
	})
	updated := InferenceServiceEvent{Type: EventTypeUpdated, InferenceService: stopped}
	if err := f.agent.handleEvent(ctx, updated); err != nil {
		t.Fatalf("handleEvent(UPDATED): %v", err)
	}
	if f.endpointExists(t) {
		t.Errorf("endpoint of the stopped engine survived scale-to-zero")
	}

	f.advance(time.Hour)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Errorf("starts = %d after scale-to-zero, want 1 (only the restart before it)", got)
	}
}

// TestHealthRestart_ScaleToZeroDuringStopIsApplied covers replicas=0 landing
// while the health monitor stops the engine: its watch event finds the key
// held and is dropped, so the restart must apply the stop itself (no start,
// Stopped status, endpoint removed).
func TestHealthRestart_ScaleToZeroDuringStopIsApplied(t *testing.T) {
	f := newRestartFixture(t, "restart-scale-stopping")
	ctx := context.Background()
	f.exec.onStop = func() {
		stopped := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
			isvc.Spec.Replicas = ptr.To(int32(0))
		})
		updated := InferenceServiceEvent{Type: EventTypeUpdated, InferenceService: stopped}
		if err := f.agent.handleEvent(ctx, updated); err != nil {
			t.Errorf("handleEvent(UPDATED): %v", err)
		}
	}

	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)

	if got := f.exec.starts(); got != 0 {
		t.Errorf("starts = %d for a service scaled to zero during the stop, want 0", got)
	}
	cur := &inferencev1alpha1.InferenceService{}
	if err := f.client.Get(ctx, types.NamespacedName{Namespace: "default", Name: f.isvc.Name}, cur); err != nil {
		t.Fatalf("get InferenceService: %v", err)
	}
	if cur.Status.Phase != inferencev1alpha1.PhaseStopped {
		t.Errorf("status phase = %q, want %q", cur.Status.Phase, inferencev1alpha1.PhaseStopped)
	}
	if f.endpointExists(t) {
		t.Errorf("endpoint of the stopped engine survived scale-to-zero")
	}
}

// TestHealthRestart_WaitsForInFlightStart checks that the health monitor does
// not stop an engine while a start holds its key (a spec change being applied
// on the event loop), and stops it on a later probe once the key is free.
func TestHealthRestart_WaitsForInFlightStart(t *testing.T) {
	f := newRestartFixture(t, "restart-guard-monitor")
	f.agent.mu.Lock()
	f.agent.starting[f.key] = true
	f.agent.mu.Unlock()

	f.probe(DefaultHealthFailureThreshold)
	if got := f.exec.recorded(); len(got) != 0 {
		t.Fatalf("executor calls = %v while a start held the key, want none", got)
	}

	f.agent.mu.Lock()
	delete(f.agent.starting, f.key)
	f.agent.mu.Unlock()
	f.probe(1)
	f.agent.runDueRestarts(context.Background())
	if got, want := f.exec.recorded(), []string{"stop 1111", "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("executor calls = %v once the key was free, want %v", got, want)
	}
}

// TestHealthRestart_RearmsWhenKeyHeld checks that a due restart whose key is
// held by another start is not stranded: it wakes the event loop again
// shortly and starts once the key is free.
func TestHealthRestart_RearmsWhenKeyHeld(t *testing.T) {
	f := newRestartFixture(t, "restart-guard-rearm")
	ctx := context.Background()
	f.probe(DefaultHealthFailureThreshold)
	select {
	case <-f.agent.restartWake:
	default:
	}

	f.agent.mu.Lock()
	f.agent.starting[f.key] = true
	f.agent.mu.Unlock()
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 0 {
		t.Fatalf("starts = %d while another start held the key, want 0", got)
	}
	f.agent.mu.Lock()
	delete(f.agent.starting, f.key)
	f.agent.mu.Unlock()

	select {
	case <-f.agent.restartWake:
	case <-time.After(restartGuardRetry + 2*time.Second):
		t.Fatal("no wake-up after the key was held; the restart is stranded")
	}
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Errorf("starts = %d after the re-armed wake-up, want 1", got)
	}
}

// TestQueueRestart_StartsOnNextWake checks the entry point for other callers
// (the memory-pressure recovery path): a queued key starts on the next event
// loop wake-up after notBefore, and queueing does not bring forward a restart
// the health backoff delays.
func TestQueueRestart_StartsOnNextWake(t *testing.T) {
	f := newRestartFixture(t, "queue-start")
	ctx := context.Background()
	f.agent.mu.Lock()
	delete(f.agent.processes, f.key) // evicted, say
	f.agent.mu.Unlock()

	f.agent.queueRestart(f.key, f.clock.Add(30*time.Second), "test")
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 0 {
		t.Fatalf("starts = %d before notBefore, want 0", got)
	}
	f.advance(30 * time.Second)
	f.agent.runDueRestarts(ctx)
	if p := f.tracked(); p == nil || f.exec.starts() != 1 {
		t.Fatalf("queued key not started after notBefore: starts=%d tracked=%v", f.exec.starts(), p != nil)
	}
}

// TestQueueRestart_DoesNotShortenBackoff checks that queueing a key whose
// restart is backing off keeps the backoff.
func TestQueueRestart_DoesNotShortenBackoff(t *testing.T) {
	f := newRestartFixture(t, "queue-backoff")
	ctx := context.Background()
	f.failFirstRestart(t)

	f.agent.queueRestart(f.key, time.Time{}, "test")
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d after queueing during a 10s backoff, want 1", got)
	}
	f.advance(healthRestartInitialBackoff)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 2 {
		t.Errorf("starts = %d when the backoff expired, want 2", got)
	}
}

// TestQueueRestart_DeletedBeforeRunNeverStarts checks that a delete cancels a
// queued restart.
func TestQueueRestart_DeletedBeforeRunNeverStarts(t *testing.T) {
	f := newRestartFixture(t, "queue-delete")
	ctx := context.Background()
	f.agent.mu.Lock()
	delete(f.agent.processes, f.key)
	f.agent.mu.Unlock()

	f.agent.queueRestart(f.key, f.clock.Add(time.Minute), "test")
	deleted := InferenceServiceEvent{Type: EventTypeDeleted, InferenceService: f.isvc}
	if err := f.agent.handleEvent(ctx, deleted); err != nil {
		t.Fatalf("handleEvent(DELETED): %v", err)
	}
	f.advance(time.Hour)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 0 {
		t.Errorf("starts = %d for a queued key deleted before it ran, want 0", got)
	}
}

// TestHealthRestart_SpecChangeKeepsReleaseFloor checks that a spec change
// after an unclean stop (one that had to kill the engine) resets the backoff
// but still waits the release floor, so the new engine does not wire its
// weights while the killed one may still hold them.
func TestHealthRestart_SpecChangeKeepsReleaseFloor(t *testing.T) {
	f := newRestartFixture(t, "restart-release-floor")
	f.exec.stopErr = errors.New("process 1111 did not exit gracefully, killed")
	ctx := context.Background()

	f.probe(DefaultHealthFailureThreshold)
	changed := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.ContextSize = ptr.To(int32(8192))
	})
	if err := f.agent.ensureProcess(ctx, changed); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 0 {
		t.Fatalf("starts = %d right after a spec change that followed an unclean stop, want 0", got)
	}

	f.advance(healthRestartInitialBackoff)
	f.agent.runDueRestarts(ctx)
	if p := f.tracked(); p == nil || f.exec.starts() != 1 || p.SpecHash != computeSpecHash(changed) {
		t.Errorf("after the release floor: starts=%d tracked=%+v, want one engine with the new spec", f.exec.starts(), p)
	}
}

// TestHealthRestart_SpecChangeDuringLivenessBackoffResets covers a spec
// change after the health monitor stopped a repeatedly failing engine and
// before its backed-off restart ran: the new spec starts at once instead of
// waiting out the old spec's backoff.
func TestHealthRestart_SpecChangeDuringLivenessBackoffResets(t *testing.T) {
	f := newRestartFixture(t, "restart-spec-liveness")
	ctx := context.Background()

	f.probe(DefaultHealthFailureThreshold)
	f.agent.runDueRestarts(ctx)
	f.advance(time.Minute)
	f.probe(DefaultHealthFailureThreshold) // the replacement fails too: next restart in 10s
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d during the 10s backoff, want 1", got)
	}

	changed := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.ContextSize = ptr.To(int32(8192))
	})
	if err := f.agent.ensureProcess(ctx, changed); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	f.agent.runDueRestarts(ctx)
	if p := f.tracked(); p == nil || f.exec.starts() != 2 || p.SpecHash != computeSpecHash(changed) {
		t.Errorf("after a spec change in the backoff: starts=%d tracked=%+v, want the new spec started at once",
			f.exec.starts(), p)
	}
}

// TestQueueRestart_FailedStartWaitsInitialBackoff checks that a queued start
// that fails is retried after the initial backoff, not at once.
func TestQueueRestart_FailedStartWaitsInitialBackoff(t *testing.T) {
	f := newRestartFixture(t, "queue-retry")
	ctx := context.Background()
	f.agent.mu.Lock()
	delete(f.agent.processes, f.key)
	f.agent.mu.Unlock()
	f.exec.startErr = errors.New("llama-server exited during startup")

	f.agent.queueRestart(f.key, time.Time{}, "test")
	f.agent.runDueRestarts(ctx)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts = %d right after a failed queued start, want 1", got)
	}
	f.advance(healthRestartInitialBackoff)
	f.agent.runDueRestarts(ctx)
	if got := f.exec.starts(); got != 2 {
		t.Errorf("starts = %d after the initial backoff, want 2", got)
	}
}

// TestHealthRestart_ShutdownDuringStopSchedulesNothing checks that the
// health monitor schedules no restart when the agent shuts down while it is
// stopping the unhealthy engine.
func TestHealthRestart_ShutdownDuringStopSchedulesNothing(t *testing.T) {
	f := newRestartFixture(t, "restart-shutdown-stopping")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.exec.onStop = cancel

	for range DefaultHealthFailureThreshold {
		f.monitor.checkAll(ctx)
	}
	f.advance(time.Hour)
	f.agent.runDueRestarts(context.Background())
	if got := f.exec.starts(); got != 0 {
		t.Errorf("starts = %d after a shutdown during the stop, want 0", got)
	}
}
