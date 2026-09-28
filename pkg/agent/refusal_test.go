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
	"reflect"
	"strings"
	"testing"

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

// recordingExecutor records starts and stops without spawning anything.
// onStart, when set, runs inside StartProcess (after the agent's pre-start
// checks), so a test can change the cluster mid-start.
type recordingExecutor struct {
	starts  int
	lastCfg ExecutorConfig
	stopped []int
	onStart func()
}

func (e *recordingExecutor) StartProcess(_ context.Context, cfg ExecutorConfig) (*ManagedProcess, error) {
	e.starts++
	e.lastCfg = cfg
	if e.onStart != nil {
		e.onStart()
	}
	return &ManagedProcess{Name: cfg.Name, Namespace: cfg.Namespace, PID: 4242, Port: 18080, Healthy: true}, nil
}

func (e *recordingExecutor) StopProcess(pid int) error {
	e.stopped = append(e.stopped, pid)
	return nil
}

// runtimeObject is client.Object; aliased to keep the fixture signature short.
type runtimeObject = client.Object

// refusalFixture wires an agent with a fake client, a fake recorder and a
// recordingExecutor for the llama.cpp runtime. objs are seeded into the client.
func refusalFixture(
	t *testing.T, cfg MetalAgentConfig, objs ...runtimeObject,
) (*MetalAgent, *recordingExecutor, *record.FakeRecorder) {
	t.Helper()
	return refusalFixtureWithInterceptor(t, cfg, nil, objs...)
}

// refusalFixtureWithInterceptor is refusalFixture with optional interceptor
// funcs installed on the fake client.
func refusalFixtureWithInterceptor(
	t *testing.T, cfg MetalAgentConfig, funcs *interceptor.Funcs, objs ...runtimeObject,
) (*MetalAgent, *recordingExecutor, *record.FakeRecorder) {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{})
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	cfg.K8sClient = b.Build()
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.ModelStorePath == "" {
		cfg.ModelStorePath = t.TempDir()
	}
	cfg.MemoryCheckMode = MemoryCheckModeWarn
	rec := record.NewFakeRecorder(16)
	cfg.EventRecorder = rec
	a := NewMetalAgent(cfg)
	ex := &recordingExecutor{}
	a.executors[runtimeLlamaServer] = ex
	a.executors[runtimeLlamaCPP] = ex
	a.registry = NewServiceRegistry(cfg.K8sClient, "10.0.0.5", newNopLogger(), "")
	return a, ex, rec
}

// refusalModelName is the Model every refusal fixture InferenceService uses.
const refusalModelName = "m"

func refusalISVC(name string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: refusalModelName},
	}
}

// refusalModel returns the Metal GGUF Model refusal fixtures reference.
func refusalModel() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: refusalModelName, Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{Source: "https://example.invalid/m.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"}},
	}
}

// drainEvents is defined in pressure_test.go and reused here.

// foreignAPIServerService returns a Service shaped like the cluster's own
// "kubernetes" Service: no managed-by label, no owner.
func foreignAPIServerService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default",
			Labels: map[string]string{"component": "apiserver"}},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "https", Port: 443}},
		},
	}
}

func getService(t *testing.T, c client.Client, name string) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, svc); err != nil {
		t.Fatalf("get Service %s: %v", name, err)
	}
	return svc
}

func assertConflictRefusal(t *testing.T, a *MetalAgent, rec *record.FakeRecorder, err error, name string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), EventReasonEndpointNameConflict) {
		t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonEndpointNameConflict)
	}
	events := drainEvents(rec)
	if !strings.Contains(strings.Join(events, "\n"), "Warning "+EventReasonEndpointNameConflict) {
		t.Errorf("events = %v, want a Warning %s", events, EventReasonEndpointNameConflict)
	}
	got := &inferencev1alpha1.InferenceService{}
	if gErr := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "default"}, got); gErr != nil {
		t.Fatalf("get InferenceService: %v", gErr)
	}
	if got.Status.SchedulingStatus != EventReasonEndpointNameConflict {
		t.Errorf("status.schedulingStatus = %q, want %q", got.Status.SchedulingStatus, EventReasonEndpointNameConflict)
	}
}

// An InferenceService whose name is taken by an unowned Service is refused
// before the engine starts: nothing is spawned, a Warning Event and the status
// carry the reason, the foreign Service is untouched and no EndpointSlice is
// created.
func TestEnsureProcess_EndpointNameConflictRefusedBeforeStart(t *testing.T) {
	isvc := refusalISVC("kubernetes")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{},
		isvc, refusalModel(), foreignAPIServerService())
	before := getService(t, a.config.K8sClient, "kubernetes")

	err := a.ensureProcess(context.Background(), isvc)

	assertConflictRefusal(t, a, rec, err, "kubernetes")
	if ex.starts != 0 {
		t.Errorf("engine started %d time(s); a taken endpoint name must be refused before start", ex.starts)
	}
	if after := getService(t, a.config.K8sClient, "kubernetes"); !reflect.DeepEqual(before, after) {
		t.Errorf("foreign Service was modified:\nbefore %+v\nafter  %+v", before, after)
	}
	if gErr := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "kubernetes", Namespace: "default"},
		&discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(gErr) {
		t.Errorf("EndpointSlice get err = %v, want NotFound", gErr)
	}
}

// The race backstop: the name is free at the pre-start check but a foreign
// Service appears while the engine is starting. Registration conflicts, so the
// engine is stopped, the refusal is reported, and the foreign Service is left
// as its creator wrote it.
func TestEnsureProcess_EndpointNameConflictBackstopStopsEngine(t *testing.T) {
	isvc := refusalISVC("kubernetes")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	ex.onStart = func() {
		if err := a.config.K8sClient.Create(context.Background(), foreignAPIServerService()); err != nil {
			t.Errorf("create foreign Service mid-start: %v", err)
		}
	}

	err := a.ensureProcess(context.Background(), isvc)

	assertConflictRefusal(t, a, rec, err, "kubernetes")
	if ex.starts != 1 {
		t.Errorf("starts = %d, want 1 (the pre-check saw a free name)", ex.starts)
	}
	if !reflect.DeepEqual(ex.stopped, []int{4242}) {
		t.Errorf("stopped = %v, want [4242]", ex.stopped)
	}
	after := getService(t, a.config.K8sClient, "kubernetes")
	if after.Labels[managedByLabel] != "" || after.Labels["component"] != "apiserver" ||
		len(after.Spec.Ports) != 1 || after.Spec.Ports[0].Port != 443 {
		t.Errorf("foreign Service was modified: %+v", after)
	}
}

// Re-evaluating a refused InferenceService (the watcher re-delivers it on
// every resourceVersion change) must not flap the status through "" and must
// never start the engine.
func TestEnsureProcess_EndpointNameConflictNoStatusFlap(t *testing.T) {
	var written []string
	funcs := &interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string,
			obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if is, ok := obj.(*inferencev1alpha1.InferenceService); ok && sub == "status" {
				written = append(written, is.Status.SchedulingStatus)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	isvc := refusalISVC("kubernetes")
	// A sized model under a generous absolute budget, so memory admission
	// passes: its success path clears a non-empty SchedulingStatus, which is
	// the flap this test guards against.
	model := refusalModel()
	model.Spec.Hardware.MemoryBudget = "1Ti"
	model.Status.Size = "1 GiB"
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, funcs,
		isvc, model, foreignAPIServerService())

	for i := range 2 {
		cur := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(context.Background(),
			types.NamespacedName{Name: "kubernetes", Namespace: "default"}, cur); err != nil {
			t.Fatal(err)
		}
		if err := a.ensureProcess(context.Background(), cur); err == nil {
			t.Fatalf("call %d: ensureProcess succeeded, want a conflict refusal", i+1)
		}
	}

	if ex.starts != 0 {
		t.Errorf("starts = %d after two refused evaluations, want 0", ex.starts)
	}
	if len(written) == 0 {
		t.Fatal("no status writes recorded; the refusal must be reported on the status")
	}
	for i, s := range written {
		if s != EventReasonEndpointNameConflict {
			t.Errorf("status write %d set schedulingStatus = %q, want %q (writes: %q)",
				i, s, EventReasonEndpointNameConflict, written)
		}
	}
}

// Switching an InferenceService from a CUDA Model to a Metal Model leaves the
// Service the controller created for it (controller ownerReference to the
// same InferenceService, no managed-by label). The agent takes it over
// instead of refusing the migration.
func TestEnsureProcess_ControllerOwnedServiceIsTakenOver(t *testing.T) {
	isvc := refusalISVC("migrated")
	isvc.UID = "isvc-uid-migrated"
	owned := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "migrated", Namespace: "default",
			Labels: map[string]string{"app": "migrated"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(), Kind: "InferenceService",
				Name: "migrated", UID: isvc.UID, Controller: ptr.To(true),
			}}},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "migrated"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 8080}},
		},
	}
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel(), owned)

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if ex.starts != 1 {
		t.Errorf("starts = %d, want 1", ex.starts)
	}
	if len(ex.stopped) != 0 {
		t.Errorf("stopped = %v, want none", ex.stopped)
	}
	svc := getService(t, a.config.K8sClient, "migrated")
	if svc.Labels[managedByLabel] != managedByValue {
		t.Errorf("Service labels = %v, want %s=%s", svc.Labels, managedByLabel, managedByValue)
	}
}
