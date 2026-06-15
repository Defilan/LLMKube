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
	"sort"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// routeBackendRefNames returns the ordered backendRef names of a route rule, so
// ejection cases can assert which backends survived into the route.
func routeBackendRefNames(t *testing.T, rule map[string]interface{}) []string {
	t.Helper()
	refs, ok := rule["backendRefs"].([]interface{})
	if !ok {
		t.Fatalf("rule has no backendRefs slice, got %T", rule["backendRefs"])
	}
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		m := r.(map[string]interface{})
		names = append(names, m["name"].(string))
	}
	return names
}

// firstRouteRule fetches the AIGatewayRoute and returns its single rule.
func firstRouteRule(t *testing.T, c client.Client, routeName string) map[string]interface{} {
	t.Helper()
	route := getUnstructured(t, c, aiGatewayRouteGVK(), routeName)
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("route %s has %d rules, want 1", routeName, len(rules))
	}
	return rules[0].(map[string]interface{})
}

// readyMessageOf returns the GatewayReady condition message after a reconcile.
func readyMessageOf(t *testing.T, c client.Client, name string) string {
	t.Helper()
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter %s: %v", name, err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected GatewayReady=True, got %+v", cond)
	}
	return cond.Message
}

// failoverRouter builds a two-backend primary-fallback ModelRouter in Gateway
// mode (primary listed first), used by the ejection envtest cases.
func failoverRouter(name, primary, fallback string) *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: primary, InferenceServiceRef: corev1LocalRef(primary)},
				{Name: fallback, InferenceServiceRef: corev1LocalRef(fallback)},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{
						Backends: []string{primary, fallback},
						Strategy: "primary-fallback",
					},
				},
			},
		},
	}
}

// TestResolveBackends_HealthFromReadyReplicas verifies resolveBackends reads a
// backend's Healthy flag off the referenced InferenceService's
// Status.ReadyReplicas: 0 -> unhealthy, >0 -> healthy.
func TestResolveBackends_HealthFromReadyReplicas(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "healthy-isvc")   // ReadyReplicas 1 from the helper
	makeBackendISvc(t, c, "unhealthy-isvc") // flipped to 0 below
	setISvcReadyReplicas(t, c, "unhealthy-isvc", 0)

	mr := failoverRouter("health-router", "healthy-isvc", "unhealthy-isvc")

	r := newModelRouterGatewayReconciler(t, cfg)
	backends, err := r.resolveBackends(context.Background(), mr)
	if err != nil {
		t.Fatalf("resolveBackends: %v", err)
	}

	got := map[string]bool{}
	for _, b := range backends {
		got[b.Name] = b.Healthy
	}
	if !got["healthy-isvc"] {
		t.Errorf("healthy-isvc Healthy = false, want true")
	}
	if got["unhealthy-isvc"] {
		t.Errorf("unhealthy-isvc Healthy = true, want false")
	}
}

// TestModelRouterGateway_EjectsUnhealthyBackendFromRoute is the end-to-end slice
// 4b case: a two-backend failover router whose primary is unhealthy
// (ReadyReplicas 0) generates an AIGatewayRoute whose rule backendRefs contain
// ONLY the healthy fallback, while both Backend/AIServiceBackend objects still
// exist (ready for re-add on recovery), and the ready message reports the ejection.
func TestModelRouterGateway_EjectsUnhealthyBackendFromRoute(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "primary-cuda")
	makeBackendISvc(t, c, "fallback-metal")
	setISvcReadyReplicas(t, c, "primary-cuda", 0) // primary down

	mr := failoverRouter("eject-router", "primary-cuda", "fallback-metal")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// Route's rule routes only to the healthy fallback.
	rule := firstRouteRule(t, c, "eject-router")
	if got := routeBackendRefNames(t, rule); len(got) != 1 || got[0] != "fallback-metal" {
		t.Errorf("route backendRefs = %v, want [fallback-metal]", got)
	}

	// Both Backend + AIServiceBackend objects still exist, including the ejected one.
	for _, name := range []string{"primary-cuda", "fallback-metal"} {
		getUnstructured(t, c, backendGVK(), name)
		getUnstructured(t, c, aiServiceBackendGVK(), name)
	}

	// Ready message reports the ejection.
	msg := readyMessageOf(t, c, "eject-router")
	if !contains(msg, "ejected 1 unhealthy backend(s)") || !contains(msg, "primary-cuda") {
		t.Errorf("ready message = %q, want it to mention ejecting primary-cuda", msg)
	}
}

// TestModelRouterGateway_BothHealthyNoEjection is the control case: both backends
// healthy -> both appear in the route, in order, and the ready message reports no
// ejection.
func TestModelRouterGateway_BothHealthyNoEjection(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "h-primary")
	makeBackendISvc(t, c, "h-fallback")

	mr := failoverRouter("healthy-route", "h-primary", "h-fallback")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	rule := firstRouteRule(t, c, "healthy-route")
	got := routeBackendRefNames(t, rule)
	if len(got) != 2 || got[0] != "h-primary" || got[1] != "h-fallback" {
		t.Errorf("route backendRefs = %v, want [h-primary h-fallback]", got)
	}

	if msg := readyMessageOf(t, c, "healthy-route"); contains(msg, "ejected") {
		t.Errorf("ready message = %q, want no ejection mention", msg)
	}
}

// TestModelRouterGateway_AllUnhealthyKeepsRouteIntact is the never-empty
// invariant under envtest: when every backend of a rule is unhealthy, the rule
// keeps ALL its backendRefs (an empty rule is invalid; 4a's active probe carries
// it) and the ready message reports no ejection.
func TestModelRouterGateway_AllUnhealthyKeepsRouteIntact(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "down-primary")
	makeBackendISvc(t, c, "down-fallback")
	setISvcReadyReplicas(t, c, "down-primary", 0)
	setISvcReadyReplicas(t, c, "down-fallback", 0)

	mr := failoverRouter("all-down-route", "down-primary", "down-fallback")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	rule := firstRouteRule(t, c, "all-down-route")
	got := routeBackendRefNames(t, rule)
	if len(got) != 2 || got[0] != "down-primary" || got[1] != "down-fallback" {
		t.Errorf("route backendRefs = %v, want [down-primary down-fallback] (never-empty)", got)
	}

	if msg := readyMessageOf(t, c, "all-down-route"); contains(msg, "ejected") {
		t.Errorf("ready message = %q, want no ejection (never-empty rule ejects nothing)", msg)
	}
}

// TestModelRoutersForInferenceService_Mapper verifies the watch map function
// enqueues the gateway-mode ModelRouter that references a changed
// InferenceService, and excludes a router that does not reference it as well as a
// Proxy-mode router that does.
func TestModelRoutersForInferenceService_Mapper(t *testing.T) {
	referencing := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "referencing", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneGateway,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "b", InferenceServiceRef: corev1LocalRef("watched-isvc")},
			},
		},
	}
	unrelated := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneGateway,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "b", InferenceServiceRef: corev1LocalRef("other-isvc")},
			},
		},
	}
	proxyMode := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-mode", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			// Proxy mode (the default); owned by ModelRouterReconciler, not this one.
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "b", InferenceServiceRef: corev1LocalRef("watched-isvc")},
			},
		},
	}

	r := newFakeRouterReconcilerWithGateway(t, referencing, unrelated, proxyMode)

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "watched-isvc", Namespace: testNS},
	}
	reqs := r.modelRoutersForInferenceService(context.Background(), isvc)

	got := make([]string, 0, len(reqs))
	for _, req := range reqs {
		got = append(got, req.Name)
	}
	sort.Strings(got)

	if len(got) != 1 || got[0] != "referencing" {
		t.Errorf("mapper requests = %v, want [referencing] (excludes unrelated and proxy-mode)", got)
	}
}
