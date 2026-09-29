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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// agentObjects returns an agent-labelled Service and EndpointSlice named name
// for the InferenceService isvcName.
func agentObjects(name, isvcName string) []runtimeObject {
	labels := func() map[string]string {
		return map[string]string{managedByLabel: managedByValue, "llmkube.ai/inference-service": isvcName}
	}
	sliceLabels := labels()
	sliceLabels[labelServiceName] = name
	return []runtimeObject{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels()}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: sliceLabels},
			AddressType: discoveryv1.AddressTypeIPv4},
	}
}

// adoptedService is the "<isvc>" Service after the controller adopted it for
// the relay: the managed-by label is gone and a selector points at the relay.
func adoptedService(name, isvcName string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
			Labels: map[string]string{"llmkube.ai/inference-service": isvcName}},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"inference.llmkube.dev/metal-relay": isvcName}},
	}
}

func assertGone(t *testing.T, c client.Client, name string) {
	t.Helper()
	key := types.NamespacedName{Namespace: "default", Name: name}
	if err := c.Get(context.Background(), key, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Errorf("Service %s get err = %v, want NotFound", name, err)
	}
	if err := c.Get(context.Background(), key, &discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(err) {
		t.Errorf("EndpointSlice %s get err = %v, want NotFound", name, err)
	}
}

func assertPresent(t *testing.T, c client.Client, name string, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, obj); err != nil {
		t.Errorf("%T %s must survive startup cleanup: %v", obj, name, err)
	}
}

// startupFixture builds an agent the way Start does before prepareRegistry:
// a registry in legacy mode and a watcher whose ownership predicate is the
// real one.
func startupFixture(t *testing.T, cfg MetalAgentConfig, funcs *interceptor.Funcs, objs ...runtimeObject) *MetalAgent {
	t.Helper()
	if cfg.IngressPort == 0 {
		cfg.IngressPort = 9443
	}
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	a, _, _ := refusalFixtureWithInterceptor(t, cfg, funcs, objs...)
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, "default", newNopLogger())
	return a
}

// Relay mode: prepareRegistry enables the ingress BEFORE the orphan sweep, and
// the sweep deletes by the listed object's own name, so an orphaned
// "<isvc>-agent" pair and an orphaned, never-adopted legacy "<isvc>" pair are
// both removed. An adopted Service (no managed-by label) is never touched.
func TestPrepareRegistry_RelaySweepsOrphansByObjectName(t *testing.T) {
	objs := append(agentObjects("gone-agent", "gone"), agentObjects("old", "old")...)
	objs = append(objs, adoptedService("adopted", "adopted"))
	a := startupFixture(t, MetalAgentConfig{}, nil, objs...)
	before := getService(t, a.config.K8sClient, "adopted")

	srv, err := a.prepareRegistry(context.Background())
	if err != nil {
		t.Fatalf("prepareRegistry: %v", err)
	}
	if srv == nil || !a.registry.relayMode() {
		t.Fatal("relay mode: prepareRegistry did not enable the ingress")
	}
	assertGone(t, a.config.K8sClient, "gone-agent")
	assertGone(t, a.config.K8sClient, "old")
	if after := getService(t, a.config.K8sClient, "adopted"); !reflect.DeepEqual(before, after) {
		t.Errorf("adopted Service was modified:\nbefore %+v\nafter  %+v", before, after)
	}
}

// The sweep counts what it actually deleted: one per orphaned Service removed,
// nothing for a Service whose delete failed, and it deletes regardless of
// whether the registry is in relay mode.
func TestReconcileOrphanEndpoints_CountsOnlyDeletions(t *testing.T) {
	funcs := &interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Service); ok && obj.GetName() == "stuck" {
				return errors.New("injected delete failure")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
	objs := append(agentObjects("gone-agent", "gone"), agentObjects("old", "old")...)
	objs = append(objs, agentObjects("stuck", "stuck")...)
	for _, relay := range []bool{false, true} {
		a := startupFixture(t, MetalAgentConfig{}, funcs, objs...)
		if relay {
			if err := a.registry.EnableIngress(9443, testIngressPin); err != nil {
				t.Fatal(err)
			}
		}
		n, err := a.registry.ReconcileOrphanEndpoints(context.Background(), "default")
		if err != nil {
			t.Fatalf("relay=%t: ReconcileOrphanEndpoints: %v", relay, err)
		}
		if n != 2 {
			t.Errorf("relay=%t: cleaned = %d, want 2 (gone-agent, old; stuck failed)", relay, n)
		}
		assertGone(t, a.config.K8sClient, "gone-agent")
		assertGone(t, a.config.K8sClient, "old")
		assertPresent(t, a.config.K8sClient, "stuck", &corev1.Service{})
	}
}

// The pair of an InferenceService that still exists is never swept.
func TestReconcileOrphanEndpoints_LiveISVCUntouched(t *testing.T) {
	isvc := refusalISVC("llm")
	objs := append([]runtimeObject{isvc, refusalModel()}, agentObjects("llm-agent", "llm")...)
	a := startupFixture(t, MetalAgentConfig{}, nil, objs...)
	n, err := a.registry.ReconcileOrphanEndpoints(context.Background(), "default")
	if err != nil || n != 0 {
		t.Fatalf("ReconcileOrphanEndpoints = %d, %v; want 0, nil", n, err)
	}
	assertPresent(t, a.config.K8sClient, "llm-agent", &corev1.Service{})
	assertPresent(t, a.config.K8sClient, "llm-agent", &discoveryv1.EndpointSlice{})
}

// Legacy mode: the agent's own "<isvc>-agent" pair for an InferenceService it
// still serves is deleted at startup, so a relay-to-legacy switch does not
// leave a relay slice fighting the controller. Its legacy "<isvc>" pair and a
// legacy Service for an InferenceService literally named "x-agent" survive, and
// so does the relay pair of an InferenceService this agent does not own.
func TestPrepareRegistry_LegacyRemovesOwnRelayObjects(t *testing.T) {
	llm := refusalISVC("llm")
	xAgent := refusalISVC("x-agent")
	other := refusalISVC("other")
	objs := make([]runtimeObject, 0, 12)
	objs = append(objs, llm, xAgent, other, refusalModel())
	objs = append(objs, agentObjects("llm-agent", "llm")...)
	objs = append(objs, agentObjects("llm", "llm")...)
	objs = append(objs, agentObjects("x-agent", "x-agent")...)
	objs = append(objs, agentObjects("other-agent", "other")...)
	a := startupFixture(t, MetalAgentConfig{
		LegacyDirectEndpoints:     true,
		InferenceServiceAllowlist: []string{"llm", "x-agent"},
	}, nil, objs...)
	a.watcher.SetNameAllowlist(a.config.InferenceServiceAllowlist)

	srv, err := a.prepareRegistry(context.Background())
	if err != nil || srv != nil {
		t.Fatalf("prepareRegistry = %v, %v; want nil, nil in legacy mode", srv, err)
	}
	assertGone(t, a.config.K8sClient, "llm-agent")
	assertPresent(t, a.config.K8sClient, "llm", &corev1.Service{})
	assertPresent(t, a.config.K8sClient, "x-agent", &corev1.Service{})
	assertPresent(t, a.config.K8sClient, "x-agent", &discoveryv1.EndpointSlice{})
	assertPresent(t, a.config.K8sClient, "other-agent", &corev1.Service{})
	assertPresent(t, a.config.K8sClient, "other-agent", &discoveryv1.EndpointSlice{})
}

// Relay mode keeps the agent's own "<isvc>-agent" pair at startup (it is
// withdrawn, not deleted).
func TestPrepareRegistry_RelayKeepsOwnRelayObjects(t *testing.T) {
	llm := refusalISVC("llm")
	objs := append([]runtimeObject{llm, refusalModel()}, agentObjects("llm-agent", "llm")...)
	a := startupFixture(t, MetalAgentConfig{}, nil, objs...)
	if _, err := a.prepareRegistry(context.Background()); err != nil {
		t.Fatalf("prepareRegistry: %v", err)
	}
	assertPresent(t, a.config.K8sClient, "llm-agent", &corev1.Service{})
}

// An InferenceService named "m-agent" registering in relay mode must not
// delete InferenceService "m"'s relay slice, which shares its name.
func TestRegisterEndpoint_RelayLegacySliceCleanupChecksISVCLabel(t *testing.T) {
	c := newRegistryTestClient(t, agentObjects("m-agent", "m")...)
	r := newRelayRegistry(c)
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m-agent", Namespace: "default"}}
	if err := r.RegisterEndpoint(context.Background(), isvc, 51234); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "m-agent"}, slice); err != nil {
		t.Fatalf("InferenceService m's relay slice was deleted by m-agent's registration: %v", err)
	}
}

// Relay mode refuses an InferenceService whose "<name>-agent" Service name
// would exceed the 63-character DNS label limit, before the engine starts.
func TestEnsureProcess_RelayRefusesTooLongServiceName(t *testing.T) {
	name := strings.Repeat("a", 58) // + "-agent" = 64
	isvc := refusalISVC(name)
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, refusalModel())
	if err := a.registry.EnableIngress(9443, testIngressPin); err != nil {
		t.Fatal(err)
	}

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonServiceNameTooLong) {
		t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonServiceNameTooLong)
	}
	if ex.starts != 0 {
		t.Errorf("engine started %d time(s), want 0", ex.starts)
	}
	if !strings.Contains(strings.Join(drainEvents(rec), "\n"), "Warning "+EventReasonServiceNameTooLong) {
		t.Errorf("want a Warning %s event", EventReasonServiceNameTooLong)
	}

	// 57 characters fits (57 + 6 = 63) and legacy mode never appends the suffix.
	ok := refusalISVC(strings.Repeat("b", 57))
	a2, ex2, _ := refusalFixture(t, MetalAgentConfig{}, ok, refusalModel())
	if err := a2.registry.EnableIngress(9443, testIngressPin); err != nil {
		t.Fatal(err)
	}
	if err := a2.ensureProcess(context.Background(), ok); err != nil || ex2.starts != 1 {
		t.Errorf("57-char name: ensureProcess = %v, starts = %d; want nil, 1", err, ex2.starts)
	}
	legacy := refusalISVC(name)
	a3, ex3, _ := refusalFixture(t, MetalAgentConfig{LegacyDirectEndpoints: true}, legacy, refusalModel())
	if err := a3.ensureProcess(context.Background(), legacy); err != nil || ex3.starts != 1 {
		t.Errorf("legacy 58-char name: ensureProcess = %v, starts = %d; want nil, 1", err, ex3.starts)
	}
}
