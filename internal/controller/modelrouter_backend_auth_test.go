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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
	if msg := keyedBackendsMessage(got); !strings.Contains(msg, `"a"`) || !strings.Contains(msg, "apiKeySecretRef") {
		t.Errorf("message %q does not name the backend and the field", msg)
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
