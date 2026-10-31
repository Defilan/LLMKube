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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// unlabeledSecretModel is a sized Metal Model whose sourceSecretRef names
// "creds", so an unmarked "creds" Secret refuses it with
// SourceSecretNotAllowed before anything else is evaluated.
func unlabeledSecretModel() *inferencev1alpha1.Model {
	model := refusalModel()
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
	model.Status.Size = "1.0 GiB"
	return model
}

// warningEvents returns the Warning Events with reason that rec holds.
func warningEvents(rec *record.FakeRecorder, reason string) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+reason+" ") {
			out = append(out, e)
		}
	}
	return out
}

// recheckFixture is a refusal fixture over InferenceService "svc" and an
// unlabeled-Secret Model, with a watcher that records re-checks and a fake
// clock shared by the agent and the watcher.
func recheckFixture(t *testing.T) (*MetalAgent, *record.FakeRecorder, *fakeClock) {
	t.Helper()
	a, _, rec := refusalFixture(t, MetalAgentConfig{}, refusalISVC("svc"), unlabeledSecretModel(),
		hfCredentialSecret(nil))
	clk := &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	a.now = clk.now
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, "default", newNopLogger())
	a.watcher.now = clk.now
	return a, rec, clk
}

// recheckIn returns how long after the fake clock's now the watcher will
// deliver "default/svc" again, failing when no re-check is scheduled.
func recheckIn(t *testing.T, a *MetalAgent, clk *fakeClock) time.Duration {
	t.Helper()
	a.watcher.requeueMu.Lock()
	defer a.watcher.requeueMu.Unlock()
	at, ok := a.watcher.requeued["default/svc"]
	if !ok {
		t.Fatal("no re-check scheduled for default/svc")
	}
	return at.Sub(clk.t)
}

// refuseAt advances the clock to the scheduled re-check, consumes it, and
// runs one reconcile of isvc (the live object when nil), which must refuse
// with reason. It returns how long until the next re-check.
func refuseAt(
	t *testing.T, a *MetalAgent, clk *fakeClock, isvc *inferencev1alpha1.InferenceService, reason string,
) time.Duration {
	t.Helper()
	a.watcher.requeueMu.Lock()
	if at, ok := a.watcher.requeued["default/svc"]; ok {
		clk.t = at
	}
	a.watcher.requeueMu.Unlock()
	_ = a.watcher.takeRequeue("default/svc")
	if isvc == nil {
		isvc = freshISVC(t, a)
	}
	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("ensureProcess error = %v, want a %s refusal", err, reason)
	}
	return recheckIn(t, a, clk)
}

// A refusal that repeats itself is re-checked on a doubling backoff capped at
// refusalRecheckMax, and the backoff starts over when the refusal's message
// changes, when the InferenceService's spec changes, when the service stops
// being refused, and when it is deleted.
func TestRefusalRecheck_BackoffGrowsAndResets(t *testing.T) {
	const reason = EventReasonSourceSecretNotAllowed
	s := time.Second

	t.Run("growth", func(t *testing.T) {
		a, _, clk := recheckFixture(t)
		got := make([]time.Duration, 0, 7)
		for range 7 {
			got = append(got, refuseAt(t, a, clk, nil, reason))
		}
		want := []time.Duration{5 * s, 10 * s, 20 * s, 40 * s, 60 * s, 60 * s, 60 * s}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("re-check waits = %v, want %v", got, want)
			}
		}
	})

	t.Run("message change", func(t *testing.T) {
		a, _, clk := recheckFixture(t)
		for range 3 {
			refuseAt(t, a, clk, nil, reason)
		}
		// The Secret disappears: same reason, new message.
		if err := a.config.K8sClient.Delete(context.Background(), hfCredentialSecret(nil)); err != nil {
			t.Fatal(err)
		}
		if wait := refuseAt(t, a, clk, nil, reason); wait != 5*s {
			t.Errorf("wait after a new message = %v, want %v", wait, 5*s)
		}
	})

	t.Run("spec change", func(t *testing.T) {
		a, _, clk := recheckFixture(t)
		for range 3 {
			refuseAt(t, a, clk, nil, reason)
		}
		cur := freshISVC(t, a)
		cur.Generation++
		if wait := refuseAt(t, a, clk, cur, reason); wait != 5*s {
			t.Errorf("wait after a spec change = %v, want %v", wait, 5*s)
		}
	})

	t.Run("not refused in between", func(t *testing.T) {
		a, rec, clk := recheckFixture(t)
		for range 3 {
			refuseAt(t, a, clk, nil, reason)
		}
		_ = drainEvents(rec)
		stopped := freshISVC(t, a)
		stopped.Spec.Replicas = ptr.To[int32](0)
		if err := a.ensureProcess(context.Background(), stopped); err != nil {
			t.Fatalf("scale to zero: %v", err)
		}
		if wait := refuseAt(t, a, clk, nil, reason); wait != 5*s {
			t.Errorf("wait after the service was not refused = %v, want %v", wait, 5*s)
		}
		if got := warningEvents(rec, reason); len(got) != 1 {
			t.Errorf("events after a new refusal episode = %q, want one Warning %s", got, reason)
		}
	})

	t.Run("delete", func(t *testing.T) {
		a, rec, clk := recheckFixture(t)
		for range 3 {
			refuseAt(t, a, clk, nil, reason)
		}
		_ = drainEvents(rec)
		if err := a.handleEvent(context.Background(), InferenceServiceEvent{
			Type: EventTypeDeleted, InferenceService: refusalISVC("svc"),
		}); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if wait := refuseAt(t, a, clk, nil, reason); wait != 5*s {
			t.Errorf("wait after a delete = %v, want %v", wait, 5*s)
		}
		if got := warningEvents(rec, reason); len(got) != 1 {
			t.Errorf("events after a delete = %q, want one Warning %s", got, reason)
		}
	})
}

// Re-checks that refuse again with the same reason and message emit no new
// Warning Event and write no status; a new message or a new reason emits one.
func TestRefusalRecheck_OneEventPerDistinctRefusal(t *testing.T) {
	var statusWrites int
	funcs := &interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*inferencev1alpha1.InferenceService); ok && sub == "status" {
				statusWrites++
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	a, ex, rec := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs,
		refusalISVC("svc"), unlabeledSecretModel(), hfCredentialSecret(nil))
	clk := &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	a.now = clk.now
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, "default", newNopLogger())
	a.watcher.now = clk.now

	for range 5 {
		refuseAt(t, a, clk, nil, EventReasonSourceSecretNotAllowed)
	}
	if got := warningEvents(rec, EventReasonSourceSecretNotAllowed); len(got) != 1 {
		t.Fatalf("events over five identical refusals = %q, want exactly one Warning %s",
			got, EventReasonSourceSecretNotAllowed)
	}
	if statusWrites != 1 {
		t.Errorf("status writes over five identical refusals = %d, want 1", statusWrites)
	}

	// Same reason, new message: the Secret is gone.
	if err := a.config.K8sClient.Delete(context.Background(), hfCredentialSecret(nil)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		refuseAt(t, a, clk, nil, EventReasonSourceSecretNotAllowed)
	}
	got := warningEvents(rec, EventReasonSourceSecretNotAllowed)
	if len(got) != 1 || !strings.Contains(got[0], "does not exist") {
		t.Fatalf("events after the message changed = %q, want one Warning naming the missing Secret", got)
	}

	// New reason: a foreign Service takes the endpoint name.
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}},
	}
	if err := a.config.K8sClient.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		refuseAt(t, a, clk, nil, EventReasonEndpointNameConflict)
	}
	if got := warningEvents(rec, EventReasonEndpointNameConflict); len(got) != 1 {
		t.Errorf("events after the reason changed = %q, want exactly one Warning %s",
			got, EventReasonEndpointNameConflict)
	}
	if ex.starts != 0 {
		t.Errorf("engine started %d times while refused, want 0", ex.starts)
	}
}

// recheckEnvtestClient starts an envtest API server with the CRDs and
// returns a client for it. Unlike the fake client, the real API server leaves
// resourceVersion alone on an identical status write, which is what left a
// refused service un-re-checked (#2329).
func recheckEnvtestClient(t *testing.T) client.WithWatch {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		matches, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*"))
		if len(matches) == 0 {
			t.Skip("no envtest binaries: run `make setup-envtest` or set KUBEBUILDER_ASSETS")
		}
		assets = matches[0]
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{repoFile(t, "config/crd/bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: newTestScheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Through the real watcher poll loop against a real API server: a service
// refused because its Secret is not labeled starts once the Secret is
// labeled, although nothing touches the InferenceService. The label goes on
// after the one re-check the refusal's own status write triggers, which is
// all the agent did before (#2329). The repeated refusals add one Warning
// Event, not one each.
func TestWatchLoop_RefusedServiceStartsAfterCauseClears(t *testing.T) {
	shortenRefusalRecheck(t)
	base := recheckEnvtestClient(t)
	ctx := context.Background()

	model := unlabeledSecretModel()
	size := model.Status.Size
	if err := base.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	model.Status.Size = size
	if err := base.Status().Update(ctx, model); err != nil {
		t.Fatalf("set Model size: %v", err)
	}
	if err := base.Create(ctx, hfCredentialSecret(nil)); err != nil {
		t.Fatalf("create Secret: %v", err)
	}

	funcs, lists := countLists(nil)
	c := interceptor.NewClient(base, *funcs)
	rec := record.NewFakeRecorder(64)
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient: c, Namespace: "default", ModelStorePath: t.TempDir(),
		MemoryCheckMode: MemoryCheckModeWarn, EventRecorder: rec,
	})
	ex := &recordingExecutor{}
	a.executors[runtimeLlamaServer] = ex
	a.executors[runtimeLlamaCPP] = ex
	a.registry = NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")

	refusals := 0
	started := driveWatchLoop(t, a, lists, refusalISVC("svc"), func(_ int, err error) {
		if err == nil || !strings.Contains(err.Error(), EventReasonSourceSecretNotAllowed) {
			return
		}
		refusals++
		if refusals == 2 {
			labeled := &corev1.Secret{}
			if gErr := base.Get(ctx, types.NamespacedName{Namespace: "default", Name: "creds"}, labeled); gErr != nil {
				t.Errorf("get Secret: %v", gErr)
				return
			}
			labeled.Labels = modelSourceCredentialsLabels()
			if uErr := base.Update(ctx, labeled); uErr != nil {
				t.Errorf("label Secret: %v", uErr)
			}
		}
	}, func() bool { return ex.starts == 1 })
	if refusals < 2 {
		t.Fatalf("saw %d refusals before the label, want at least 2", refusals)
	}
	if !started {
		t.Fatalf("after %v the service had not started; a refused service is not re-checked once its "+
			"refusal repeats", watchLoopTimeout)
	}
	if got := warningEvents(rec, EventReasonSourceSecretNotAllowed); len(got) != 1 {
		t.Errorf("events = %q, want exactly one Warning %s for %d identical refusals",
			got, EventReasonSourceSecretNotAllowed, refusals)
	}
}

// RequeueAt delivers an unchanged service on the first poll at or after its
// time, once; a key queued twice keeps the earlier time.
func TestPoll_RequeueAtWaitsUntilDue(t *testing.T) {
	f := newModelLookupFixture(t)
	clk := &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	f.watcher.now = clk.now
	key := "default/" + lookupISVCName
	seen := map[string]string{}
	_ = f.pollOnce(t, seen)

	f.watcher.RequeueAt(key, clk.t.Add(10*time.Second))
	f.watcher.RequeueAt(key, clk.t.Add(5*time.Second))
	f.watcher.RequeueAt(key, clk.t.Add(20*time.Second))
	clk.t = clk.t.Add(4 * time.Second)
	if got := eventSummary(f.pollOnce(t, seen)); len(got) != 0 {
		t.Fatalf("poll before the re-check is due = %v, want no events", got)
	}
	clk.t = clk.t.Add(time.Second)
	if got := eventSummary(f.pollOnce(t, seen)); len(got) != 1 || got[0] != "UPDATED "+key {
		t.Fatalf("poll once the earliest re-check is due = %v, want UPDATED %s", got, key)
	}
	clk.t = clk.t.Add(time.Minute)
	if got := eventSummary(f.pollOnce(t, seen)); len(got) != 0 {
		t.Fatalf("later poll = %v, want the re-check delivered once", got)
	}
}
