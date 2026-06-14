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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests mirror the plain-Go envtest style of the InferenceService gateway
// tests (inferenceservice_gateway_test.go): each case spins up its own envtest
// with or without the aigw CRD stubs so the CRDs-present and CRDs-absent worlds
// never bleed together. They reuse startGatewayTestEnv / assertOwnedBy from that
// file (same package).

// newModelRouterGatewayReconciler builds a ModelRouter gateway reconciler backed
// by a client whose RESTMapper is dynamic, so the CRD-presence gate reflects the
// env it runs against.
func newModelRouterGatewayReconciler(t *testing.T, cfg *rest.Config) *ModelRouterGatewayReconciler {
	t.Helper()
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("http client: %v", err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		t.Fatalf("rest mapper: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme, Mapper: mapper})
	if err != nil {
		t.Fatalf("new mapped client: %v", err)
	}
	return &ModelRouterGatewayReconciler{Client: c, Scheme: scheme.Scheme}
}

func reconcileRouter(t *testing.T, r *ModelRouterGatewayReconciler, mr *inferencev1alpha1.ModelRouter) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
}

// makeBackendISvc creates a minimal InferenceService a ModelRouter backend can
// reference, so resolveBackends finds a real Service FQDN/port.
func makeBackendISvc(t *testing.T, c client.Client, name string) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: name,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create backend isvc %s: %v", name, err)
	}
}

// assertNotExists asserts a resource of the given GVK/name is absent.
func assertNotExists(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, u)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected %s/%s to not exist, get err = %v", gvk.Kind, name, err)
	}
}

// TestModelRouterGateway_FailoverProducesResources covers case (a): a
// dataPlane: Gateway ModelRouter with two backends and a primary-fallback rule
// produces a Backend + AIServiceBackend per backend, a multi-rule AIGatewayRoute
// with priority backendRefs, and the retry BackendTrafficPolicy, all owner-ref'd
// to the ModelRouter.
func TestModelRouterGateway_FailoverProducesResources(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")
	makeBackendISvc(t, c, "qwen-metal")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{
				Name:      "ai-gateway",
				Namespace: "ai-gateway",
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "qwen-cuda", InferenceServiceRef: corev1LocalRef("qwen-cuda")},
				{Name: "qwen-metal", InferenceServiceRef: corev1LocalRef("qwen-metal")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{
						Backends: []string{"qwen-cuda", "qwen-metal"},
						Strategy: "primary-fallback",
					},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// A Backend + AIServiceBackend per backend, named after the RouterBackend.
	for _, name := range []string{"qwen-cuda", "qwen-metal"} {
		backend := getUnstructured(t, c, backendGVK(), name)
		if host := backendHostname(t, backend); host != name+".default.svc.cluster.local" {
			t.Errorf("backend %s hostname = %q, want %s.default.svc.cluster.local", name, host, name)
		}
		assertOwnedByRouter(t, backend, mr)

		asb := getUnstructured(t, c, aiServiceBackendGVK(), name)
		schemaName, _, _ := unstructured.NestedString(asb.Object, "spec", "schema", "name")
		if schemaName != "OpenAI" {
			t.Errorf("aiservicebackend %s schema.name = %q, want OpenAI", name, schemaName)
		}
		assertOwnedByRouter(t, asb, mr)
	}

	// One AIGatewayRoute named after the ModelRouter, attached to the Gateway,
	// its single rule matching qwen35-27b with priority backendRefs (0 = primary
	// cuda, 1 = fallback metal).
	route := getUnstructured(t, c, aiGatewayRouteGVK(), "qwen-router")
	assertOwnedByRouter(t, route, mr)
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("route has %d rules, want 1", len(rules))
	}
	rule0 := rules[0].(map[string]interface{})
	if got := routeModelOfRule(t, rule0); got != "qwen35-27b" {
		t.Errorf("rule model match = %q, want qwen35-27b", got)
	}
	refs := rule0["backendRefs"].([]interface{})
	if len(refs) != 2 {
		t.Fatalf("rule has %d backendRefs, want 2", len(refs))
	}
	assertBackendRefPriority(t, refs[0], "qwen-cuda", 0)
	assertBackendRefPriority(t, refs[1], "qwen-metal", 1)

	// The retry BackendTrafficPolicy targets the generated HTTPRoute (shares the
	// route name) and carries the retry + passive healthCheck config.
	btp := getUnstructured(t, c, btpGVK(), "qwen-router")
	assertOwnedByRouter(t, btp, mr)
	targetRefs, _, _ := unstructured.NestedSlice(btp.Object, "spec", "targetRefs")
	if len(targetRefs) != 1 {
		t.Fatalf("btp has %d targetRefs, want 1", len(targetRefs))
	}
	tr := targetRefs[0].(map[string]interface{})
	if tr["kind"] != "HTTPRoute" || tr["name"] != "qwen-router" {
		t.Errorf("btp targetRef = %+v, want HTTPRoute/qwen-router", tr)
	}
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "retry"); !found {
		t.Error("btp missing spec.retry")
	}
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "healthCheck", "passive"); !found {
		t.Error("btp missing spec.healthCheck.passive")
	}
	// 2b adds rateLimit to THIS BTP; 2a must NOT include it.
	if _, found, _ := unstructured.NestedMap(btp.Object, "spec", "rateLimit"); found {
		t.Error("btp should not carry rateLimit in slice 2a")
	}

	// status.gateway + GatewayReady=True.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "qwen-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter status: %v", err)
	}
	if fresh.Status.Gateway == nil || !fresh.Status.Gateway.RouteReady {
		t.Errorf("status.gateway.routeReady not set, got %+v", fresh.Status.Gateway)
	}
	if cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("GatewayReady condition not True, got %+v", cond)
	}
}

// TestModelRouterGateway_UnsupportedMatchFailsLoud covers case (b): a rule using
// dataClassification (a match the gateway data plane cannot express) sets
// GatewayReady=False with reason UnsupportedMatchInGatewayMode and generates
// NOTHING.
func TestModelRouterGateway_UnsupportedMatchFailsLoud(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "local-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "pii-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "local-cuda", InferenceServiceRef: corev1LocalRef("local-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:       "pii",
					Match:      &inferencev1alpha1.RuleMatch{DataClassification: []string{"pii"}},
					FailClosed: true,
					Route:      inferencev1alpha1.RuleRoute{Backends: []string{"local-cuda"}},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, mr)

	// Generates NOTHING: no Backend, AIServiceBackend, route, or BTP.
	assertNotExists(t, c, backendGVK(), "local-cuda")
	assertNotExists(t, c, aiServiceBackendGVK(), "local-cuda")
	assertNotExists(t, c, aiGatewayRouteGVK(), "pii-router")
	assertNotExists(t, c, btpGVK(), "pii-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pii-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonUnsupported {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonUnsupported, cond)
	}
	if fresh.Status.Gateway != nil && fresh.Status.Gateway.RouteReady {
		t.Errorf("status.gateway.routeReady should be false on unsupported match")
	}
}

// TestModelRouterGateway_ProxyModeProducesNothing covers case (c): a
// dataPlane: Proxy (default) ModelRouter generates no gateway resources (the
// gateway reconciler no-ops; the proxy path is owned by ModelRouterReconciler
// and is unaffected).
func TestModelRouterGateway_ProxyModeProducesNothing(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "proxy-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			// DataPlane omitted -> defaults to Proxy at the API server.
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "proxy-cuda", InferenceServiceRef: corev1LocalRef("proxy-cuda")},
			},
			DefaultRoute: "proxy-cuda",
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	// Confirm the default landed as Proxy.
	created := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "proxy-router", Namespace: testNS}, created); err != nil {
		t.Fatalf("get created router: %v", err)
	}
	if created.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneProxy {
		t.Fatalf("expected default DataPlane Proxy, got %q", created.Spec.DataPlane)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	reconcileRouter(t, r, created)

	assertNotExists(t, c, backendGVK(), "proxy-cuda")
	assertNotExists(t, c, aiServiceBackendGVK(), "proxy-cuda")
	assertNotExists(t, c, aiGatewayRouteGVK(), "proxy-router")
	assertNotExists(t, c, btpGVK(), "proxy-router")

	// The gateway reconciler must not have written status.gateway in Proxy mode.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "proxy-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if fresh.Status.Gateway != nil {
		t.Errorf("expected nil status.gateway in Proxy mode, got %+v", fresh.Status.Gateway)
	}
}

// TestModelRouterGateway_CRDsAbsentIsCleanNoOp covers case (d): with the aigw
// CRDs not installed, a dataPlane: Gateway ModelRouter does not error/crash,
// creates nothing, and sets the disabled GatewayReady condition.
func TestModelRouterGateway_CRDsAbsentIsCleanNoOp(t *testing.T) {
	c, cfg, stop := startGatewayTestEnv(t, false)
	defer stop()

	makeBackendISvc(t, c, "absent-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "absent-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane:  inferencev1alpha1.ModelRouterDataPlaneGateway,
			GatewayRef: &inferencev1alpha1.GatewayReference{Name: "ai-gateway", Namespace: "ai-gateway"},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "absent-cuda", InferenceServiceRef: corev1LocalRef("absent-cuda")},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterGatewayReconciler(t, cfg)
	// Must not error or panic.
	reconcileRouter(t, r, mr)

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "absent-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterGatewayReasonCRDsMissing {
		t.Errorf("expected GatewayReady=False/%s, got %+v", modelRouterGatewayReasonCRDsMissing, cond)
	}
}

// --- helpers ---

// corev1LocalRef builds a *LocalObjectReference inline (avoids repeating the
// struct literal at every backend call site).
func corev1LocalRef(name string) *corev1.LocalObjectReference {
	return &corev1.LocalObjectReference{Name: name}
}

// assertOwnedByRouter verifies obj carries a controller owner reference to mr.
func assertOwnedByRouter(t *testing.T, obj *unstructured.Unstructured, mr *inferencev1alpha1.ModelRouter) {
	t.Helper()
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "ModelRouter" && ref.Name == mr.Name {
			if ref.Controller == nil || !*ref.Controller {
				t.Errorf("%s/%s owner ref to %s is not a controller ref", obj.GetKind(), obj.GetName(), mr.Name)
			}
			return
		}
	}
	t.Errorf("%s/%s missing owner reference to ModelRouter %s", obj.GetKind(), obj.GetName(), mr.Name)
}

// routeModelOfRule extracts the x-ai-eg-model header match value from the first
// match of a route rule map.
func routeModelOfRule(t *testing.T, rule map[string]interface{}) string {
	t.Helper()
	matches := rule["matches"].([]interface{})
	headers := matches[0].(map[string]interface{})["headers"].([]interface{})
	for _, h := range headers {
		header := h.(map[string]interface{})
		if header["name"] == aiGatewayModelHeader {
			val, _ := header["value"].(string)
			return val
		}
	}
	t.Fatalf("rule match has no %s header", aiGatewayModelHeader)
	return ""
}

// assertBackendRefPriority verifies a backendRef has the given name and priority.
func assertBackendRefPriority(t *testing.T, ref interface{}, wantName string, wantPriority int64) {
	t.Helper()
	m := ref.(map[string]interface{})
	if m["name"] != wantName {
		t.Errorf("backendRef name = %v, want %s", m["name"], wantName)
	}
	got, ok := m["priority"].(int64)
	if !ok {
		t.Fatalf("backendRef %s priority is %T, want int64", wantName, m["priority"])
	}
	if got != wantPriority {
		t.Errorf("backendRef %s priority = %d, want %d", wantName, got, wantPriority)
	}
}
