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

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func routerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func routerWithLocal(name, isvc string) *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DefaultRoute: name,
			Backends: []inferencev1alpha1.RouterBackend{{
				Name: name, InferenceServiceRef: &corev1.LocalObjectReference{Name: isvc},
			}},
		},
	}
}

func TestKeyedBackends(t *testing.T) {
	keyed := keyedISvc(RuntimeVLLM)
	keyed.Name = "keyed"
	open := keyedISvc(RuntimeVLLM)
	open.Name, open.Spec.Endpoint = "open", nil
	c := fake.NewClientBuilder().WithScheme(routerScheme(t)).WithObjects(keyed, open).Build()

	mr := routerWithLocal("a", "keyed")
	mr.Spec.Backends = append(mr.Spec.Backends,
		inferencev1alpha1.RouterBackend{Name: "b", InferenceServiceRef: &corev1.LocalObjectReference{Name: "open"}},
		inferencev1alpha1.RouterBackend{Name: "c", InferenceServiceRef: &corev1.LocalObjectReference{Name: "missing"}})
	got, err := keyedBackends(context.Background(), c, mr)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"a"}) {
		t.Errorf("keyedBackends = %v, want [a]", got)
	}
	if msg := keyedBackendsMessage(got); !strings.Contains(msg, `"a"`) || !strings.Contains(msg, "apiKeySecretRef") ||
		!strings.Contains(msg, "--api-key") {
		t.Errorf("message %q does not name the backend and both keying forms", msg)
	}
}

// TestKeyedBackendsCommandLineKey pins the command-line keying forms: a
// backend keyed through --api-key must fail the router the same way
// apiKeySecretRef does, and spec.args only counts when spec.command is set
// (the runtimes drop spec.args otherwise).
func TestKeyedBackendsCommandLineKey(t *testing.T) {
	withCommandArgs := func(command []string) *inferencev1alpha1.InferenceService {
		isvc := keyedISvc(RuntimeSGLANG)
		isvc.Spec.Endpoint = nil
		isvc.Spec.Command = command
		isvc.Spec.Args = []string{"--api-key", "$(MY_KEY)"}
		return isvc
	}
	flagOnly := keyedISvc(RuntimeVLLM, "--api-key", "$(MY_KEY)")
	flagOnly.Spec.Endpoint = nil
	for _, tc := range []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
		want bool
	}{
		{"--api-key in extraArgs, split form", flagOnly, true},
		{"--api-key= in extraArgs, inline form", func() *inferencev1alpha1.InferenceService {
			isvc := keyedISvc(RuntimeVLLM, "--api-key=$(MY_KEY)")
			isvc.Spec.Endpoint = nil
			return isvc
		}(), true},
		{"spec.args flag with spec.command", withCommandArgs([]string{"python3", "-m", "sglang.launch_server"}), true},
		{"spec.args flag without spec.command", withCommandArgs(nil), false},
		{"unrelated extraArgs", func() *inferencev1alpha1.InferenceService {
			isvc := keyedISvc(RuntimeVLLM, "--host", "0.0.0.0")
			isvc.Spec.Endpoint = nil
			return isvc
		}(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(routerScheme(t)).WithObjects(tc.isvc).Build()
			got, err := keyedBackends(context.Background(), c, routerWithLocal("b", "svc"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want && !slices.Equal(got, []string{"b"}) {
				t.Errorf("keyedBackends = %v, want [b]", got)
			}
			if !tc.want && len(got) != 0 {
				t.Errorf("keyedBackends = %v, want none", got)
			}
		})
	}
}

func TestBackendsReadyConditionKeyed(t *testing.T) {
	cc := &compiledConfig{
		Backends:      []inferencev1alpha1.BackendStatus{{Name: "a", Healthy: false}},
		KeyedBackends: []string{"a"},
	}
	ready, reason, msg := backendsReadyCondition(cc)
	if ready || reason != ModelRouterReasonBackendAPIKeyUnsupported || !strings.Contains(msg, `"a"`) {
		t.Errorf("got (%v, %q, %q), want not ready, BackendAPIKeyUnsupported, naming a", ready, reason, msg)
	}
	ready, reason, _ = backendsReadyCondition(&compiledConfig{
		Backends: []inferencev1alpha1.BackendStatus{{Name: "a", Healthy: true}},
	})
	if !ready || reason != ModelRouterReasonBackendsResolved {
		t.Errorf("healthy case = (%v, %q), want ready, BackendsResolved", ready, reason)
	}
}

// Proxy compile marks the keyed backend unhealthy with the reason in its message.
func TestCompileRouterConfigMarksKeyedBackend(t *testing.T) {
	keyed := keyedISvc(RuntimeVLLM)
	keyed.Name = "keyed"
	c := fake.NewClientBuilder().WithScheme(routerScheme(t)).WithObjects(keyed).Build()
	r := &ModelRouterReconciler{Client: c}
	cc, err := r.compileRouterConfig(context.Background(), routerWithLocal("a", "keyed"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cc.KeyedBackends, []string{"a"}) || cc.Backends[0].Healthy ||
		!strings.Contains(cc.Backends[0].Message, "apiKeySecretRef") {
		t.Errorf("compiled = keyed %v, status %+v; want backend a unhealthy and named", cc.KeyedBackends, cc.Backends[0])
	}
}

// TestModelRouterReconcileProxyKeyedBackend drives the real Proxy-mode
// Reconcile against an API server: the keyed-backend verdict must survive the
// status write and read back from the stored ModelRouter.
func TestModelRouterReconcileProxyKeyedBackend(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-vllm", Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "keyed-vllm",
			Runtime:  RuntimeVLLM,
			Endpoint: &inferencev1alpha1.EndpointSpec{
				Port: 8080,
				APIKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key",
				},
			},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create keyed backend isvc: %v", err)
	}
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneProxy,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "keyed-vllm", InferenceServiceRef: &corev1.LocalObjectReference{Name: "keyed-vllm"}},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "all", Route: inferencev1alpha1.RuleRoute{Backends: []string{"keyed-vllm"}}},
			},
			DefaultRoute: "keyed-vllm",
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := &ModelRouterReconciler{Client: c, Scheme: clientgoscheme.Scheme, RouterProxyImage: "test-image"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterConditionBackendsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonBackendAPIKeyUnsupported {
		t.Fatalf("BackendsReady = %+v, want False/%s", cond, ModelRouterReasonBackendAPIKeyUnsupported)
	}
	if !strings.Contains(cond.Message, `"keyed-vllm"`) {
		t.Errorf("BackendsReady message %q does not name the backend", cond.Message)
	}
}

// TestModelRouterReconcileProxyKeyedExtraArgsBackend drives the same proxy
// Reconcile with the key passed on the command line instead of
// apiKeySecretRef: the verdict must survive the status write and read back
// from the stored ModelRouter.
func TestModelRouterReconcileProxyKeyedExtraArgsBackend(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-vllm", Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:  "keyed-vllm",
			Runtime:   RuntimeVLLM,
			ExtraArgs: []string{"--api-key", "$(MY_KEY)"},
			Endpoint:  &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create keyed backend isvc: %v", err)
	}
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneProxy,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "keyed-vllm", InferenceServiceRef: &corev1.LocalObjectReference{Name: "keyed-vllm"}},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "all", Route: inferencev1alpha1.RuleRoute{Backends: []string{"keyed-vllm"}}},
			},
			DefaultRoute: "keyed-vllm",
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := &ModelRouterReconciler{Client: c, Scheme: clientgoscheme.Scheme, RouterProxyImage: "test-image"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterConditionBackendsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonBackendAPIKeyUnsupported {
		t.Fatalf("BackendsReady = %+v, want False/%s", cond, ModelRouterReasonBackendAPIKeyUnsupported)
	}
	if !strings.Contains(cond.Message, `"keyed-vllm"`) {
		t.Errorf("BackendsReady message %q does not name the backend", cond.Message)
	}
}
