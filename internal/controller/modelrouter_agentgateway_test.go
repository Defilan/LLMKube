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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// inferenceExtensionTestCRDDir is the directory of Inference Extension CRD stubs
// (InferencePool, InferenceModel) the agentgateway tests load alongside the base
// LLMKube CRDs, mirroring startGatewayTestEnv's aigw stubs.
const inferenceExtensionTestCRDDir = "../../test/crd/inference-extension"

// startAgentGatewayTestEnv starts an envtest with (or without) the Inference
// Extension CRDs registered alongside the base LLMKube CRDs, mirroring
// startGatewayTestEnv.
func startAgentGatewayTestEnv(t *testing.T, withCRDs bool) (client.Client, *rest.Config, func()) {
	t.Helper()
	crdPaths := []string{baseCRDPath}
	if withCRDs {
		crdPaths = append(crdPaths, inferenceExtensionTestCRDDir)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	s := scheme.Scheme
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		_ = env.Stop()
		t.Fatalf("new client: %v", err)
	}
	return c, cfg, func() { _ = env.Stop() }
}

// newModelRouterAgentGatewayReconciler builds an agentgateway reconciler backed
// by a client whose RESTMapper is dynamic, so the CRD-presence gate reflects the
// env it runs against.
func newModelRouterAgentGatewayReconciler(t *testing.T, cfg *rest.Config) *ModelRouterAgentGatewayReconciler {
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
	return &ModelRouterAgentGatewayReconciler{Client: c, Scheme: scheme.Scheme}
}

// reconcileAgentGatewayRouter runs a reconcile and fails the test on error.
func reconcileAgentGatewayRouter(t *testing.T, r *ModelRouterAgentGatewayReconciler, mr *inferencev1alpha1.ModelRouter) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
}

// TestModelRouterAgentGateway_CompilesInferenceExtension covers the happy path:
// a dataPlane: AgentGateway ModelRouter with two backends and a model rule
// produces an InferencePool + InferenceModel per backend and a multi-rule
// HTTPRoute that matches the model name and references the InferenceModels, all
// owner-ref'd to the ModelRouter, and sets AgentGatewayReady=True.
func TestModelRouterAgentGateway_CompilesInferenceExtension(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "qwen-cuda")
	makeBackendISvc(t, c, "qwen-metal")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "agw-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name:               "ai-gateway",
				Namespace:          "ai-gateway",
				EndpointPicker:     "llm-d-router-epp",
				EndpointPickerPort: 9002,
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

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	// This router has only InferenceServiceRef backends, so the reconcile must
	// SUCCEED; the assertions below require the compiled resources to exist.
	// (External-backend rejection is covered by
	// TestModelRouterAgentGateway_ExternalBackendFailsLoud.)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// An InferencePool + InferenceModel per backend, named after the
	// RouterBackend.
	for _, name := range []string{"qwen-cuda", "qwen-metal"} {
		pool := getUnstructured(t, c, inferencePoolGVK(), name)
		assertOwnedByRouter(t, pool, mr)
		// The pool selects the referenced InferenceService's pods by the SAME
		// selector the Deployment uses, and targets the Service port.
		selector, _, _ := unstructured.NestedStringMap(pool.Object, "spec", "selector")
		if got := selector["app"]; got != name {
			t.Errorf("pool %s selector app = %q, want %s", name, got, name)
		}
		ports, _, _ := unstructured.NestedSlice(pool.Object, "spec", "targetPorts")
		if len(ports) != 1 {
			t.Fatalf("pool %s targetPorts = %d, want 1", name, len(ports))
		}
		if num, _, _ := unstructured.NestedInt64(ports[0].(map[string]interface{}), "number"); num != 8080 {
			t.Errorf("pool %s targetPort = %d, want 8080", name, num)
		}
		// The pool references the operator's Endpoint Picker on the configured port.
		epRef, _, _ := unstructured.NestedMap(pool.Object, "spec", "endpointPickerRef")
		if epRef["name"] != "llm-d-router-epp" {
			t.Errorf("pool %s endpointPickerRef.name = %v, want llm-d-router-epp", name, epRef["name"])
		}

		model := getUnstructured(t, c, inferenceModelGVK(), name)
		assertOwnedByRouter(t, model, mr)
		modelName, _, _ := unstructured.NestedString(model.Object, "spec", "modelName")
		if modelName != name {
			t.Errorf("model %s modelName = %q, want %s", name, modelName, name)
		}
		poolRef, _, _ := unstructured.NestedMap(model.Object, "spec", "poolRef")
		if poolRef["name"] != name {
			t.Errorf("model %s poolRef.name = %v, want %s", name, poolRef["name"], name)
		}
		if poolRef["kind"] != inferencePoolKind {
			t.Errorf("model %s poolRef.kind = %v, want %s", name, poolRef["kind"], inferencePoolKind)
		}
	}

	// One HTTPRoute named after the ModelRouter, attached to the Gateway, its
	// single rule matching qwen35-27b and referencing both InferenceModels.
	route := getUnstructured(t, c, gatewayHTTPRouteGVK(), "agw-router")
	assertOwnedByRouter(t, route, mr)
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("route has %d rules, want 1", len(rules))
	}
	rule0 := rules[0].(map[string]interface{})
	if got := agentGatewayRouteModelOfRule(t, rule0); got != "qwen35-27b" {
		t.Errorf("rule model match = %q, want qwen35-27b", got)
	}
	refs := rule0["backendRefs"].([]interface{})
	if len(refs) != 2 {
		t.Fatalf("rule has %d backendRefs, want 2", len(refs))
	}
	for i, want := range []string{"qwen-cuda", "qwen-metal"} {
		ref := refs[i].(map[string]interface{})
		if ref["name"] != want {
			t.Errorf("backendRefs[%d].name = %v, want %s", i, ref["name"], want)
		}
		if ref["kind"] != inferenceModelKind {
			t.Errorf("backendRefs[%d].kind = %v, want %s", i, ref["kind"], inferenceModelKind)
		}
	}

	// status.agentGateway + AgentGatewayReady=True.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "agw-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter status: %v", err)
	}
	if fresh.Status.AgentGateway == nil || !fresh.Status.AgentGateway.PoolReady {
		t.Errorf("status.agentGateway.poolReady not set, got %+v", fresh.Status.AgentGateway)
	}
	if cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("AgentGatewayReady condition not True, got %+v", cond)
	}
}

// TestModelRouterAgentGateway_ExternalBackendFailsLoud covers issue #4: an
// External backend (which the Gateway plane compiles but the agentgateway
// InferencePool path cannot, since a pool selects model-server pods) is a loud
// unsupported-field error, not a silent reduction.
func TestModelRouterAgentGateway_ExternalBackendFailsLoud(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "ext-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "anthropic", External: &inferencev1alpha1.ExternalProvider{Provider: "anthropic", Model: "claude"}},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "r", Route: inferencev1alpha1.RuleRoute{Backends: []string{"anthropic"}}},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
	}); err == nil {
		t.Fatal("expected reconcile to fail on an External backend, got nil")
	}

	// Generates NOTHING: no pool, model, or route.
	assertNotExists(t, c, inferencePoolGVK(), "anthropic")
	assertNotExists(t, c, inferenceModelGVK(), "anthropic")
	assertNotExists(t, c, gatewayHTTPRouteGVK(), "ext-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ext-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonReconcile {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonReconcile, cond)
	}
}

// TestModelRouterAgentGateway_UnsupportedAuthFailsLoud covers issue #4: a
// policy.auth.jwt block (which the Gateway plane compiles into a SecurityPolicy)
// is rejected loudly on the agentgateway path, which compiles no SecurityPolicy,
// rather than silently accepting the config and enforcing nothing.
func TestModelRouterAgentGateway_UnsupportedAuthFailsLoud(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "local-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "local-cuda", InferenceServiceRef: corev1LocalRef("local-cuda")},
			},
			Policy: &inferencev1alpha1.RouterPolicy{
				Auth: &inferencev1alpha1.RouterAuthSpec{
					JWT: &inferencev1alpha1.JWTAuthSpec{
						Provider:  "keycloak",
						Issuer:    "https://issuer.example.com",
						JWKSURI:   "https://issuer.example.com/certs",
						TeamClaim: "team",
					},
				},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "r", Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}}, Route: inferencev1alpha1.RuleRoute{Backends: []string{"local-cuda"}}},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	// Generates NOTHING.
	assertNotExists(t, c, inferencePoolGVK(), "local-cuda")
	assertNotExists(t, c, inferenceModelGVK(), "local-cuda")
	assertNotExists(t, c, gatewayHTTPRouteGVK(), "auth-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "auth-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonInvalidAuth {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonInvalidAuth, cond)
	}
	if !strings.Contains(cond.Message, "policy.auth is not supported in dataPlane: AgentGateway") {
		t.Errorf("message does not name the unsupported auth, got %q", cond.Message)
	}
}

// TestModelRouterAgentGateway_UnsupportedMatchFailsLoud covers issue #4: a rule
// using a match the agentgateway data plane cannot express sets
// AgentGatewayReady=False and generates NOTHING.
func TestModelRouterAgentGateway_UnsupportedMatchFailsLoud(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "local-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "pii-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "local-cuda", InferenceServiceRef: corev1LocalRef("local-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:       "pii",
					Match:      &inferencev1alpha1.RuleMatch{TaskComplexity: "complex"},
					FailClosed: true,
					Route:      inferencev1alpha1.RuleRoute{Backends: []string{"local-cuda"}},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	assertNotExists(t, c, inferencePoolGVK(), "local-cuda")
	assertNotExists(t, c, inferenceModelGVK(), "local-cuda")
	assertNotExists(t, c, gatewayHTTPRouteGVK(), "pii-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pii-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonUnsupported {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonUnsupported, cond)
	}
}

// TestModelRouterAgentGateway_FailedReconcileEmitsWarningEvent covers issue #3:
// a failed reconcile must be VISIBLE at the request path. The data plane keeps
// serving its last-good compilation, so a status condition alone is not enough;
// a Warning Event must be emitted.
func TestModelRouterAgentGateway_FailedReconcileEmitsWarningEvent(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	mr := &inferencev1alpha1.ModelRouter{}
	mr.SetName("router")
	mr.SetNamespace("default")
	mr.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneAgentGateway

	rec := events.NewFakeRecorder(4)
	r := &ModelRouterAgentGatewayReconciler{
		Client:   fake.NewClientBuilder().WithScheme(testScheme).WithObjects(mr).WithStatusSubresource(mr).Build(),
		Scheme:   testScheme,
		Recorder: rec,
	}

	if err := r.setAgentGatewayNotReady(context.Background(), mr, modelRouterAgentGatewayReasonReconcile, "backend is broken"); err != nil {
		t.Fatalf("setAgentGatewayNotReady: %v", err)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Warning") {
			t.Errorf("event must be a Warning, got %q", ev)
		}
		if !strings.Contains(ev, "last successfully compiled") {
			t.Errorf("event must say the stale pools/routes are still serving, got %q", ev)
		}
	default:
		t.Fatal("no event emitted; a failed reconcile would be invisible at the request path")
	}
}

// TestModelRouterAgentGateway_CRDsAbsentIsCleanNoOp covers the CRD-presence gate:
// with the Inference Extension CRDs not installed, a dataPlane: AgentGateway
// ModelRouter does not error, creates nothing, and sets the disabled condition.
func TestModelRouterAgentGateway_CRDsAbsentIsCleanNoOp(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, false)
	defer stop()

	makeBackendISvc(t, c, "absent-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "absent-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "absent-cuda", InferenceServiceRef: corev1LocalRef("absent-cuda")},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "absent-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonCRDsMissing {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonCRDsMissing, cond)
	}
}

// TestModelRouterAgentGateway_ProxyModeProducesNothing covers the no-op path: a
// dataPlane: Proxy ModelRouter generates no agentgateway resources (the
// agentgateway reconciler no-ops; the proxy path is owned by
// ModelRouterReconciler and is unaffected).
func TestModelRouterAgentGateway_ProxyModeProducesNothing(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "proxy-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneProxy,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "proxy-cuda", InferenceServiceRef: corev1LocalRef("proxy-cuda")},
			},
			DefaultRoute: "proxy-cuda",
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	assertNotExists(t, c, inferencePoolGVK(), "proxy-cuda")
	assertNotExists(t, c, inferenceModelGVK(), "proxy-cuda")
	assertNotExists(t, c, gatewayHTTPRouteGVK(), "proxy-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "proxy-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if fresh.Status.AgentGateway != nil {
		t.Errorf("expected nil status.agentGateway in Proxy mode, got %+v", fresh.Status.AgentGateway)
	}
}

// agentGatewayRouteModelOfRule extracts the x-ai-eg-model header match value
// from the first match of a route rule map.
func agentGatewayRouteModelOfRule(t *testing.T, rule map[string]interface{}) string {
	t.Helper()
	matches, _, _ := unstructured.NestedSlice(rule, "matches")
	if len(matches) == 0 {
		return ""
	}
	headers, _, _ := unstructured.NestedSlice(matches[0].(map[string]interface{}), "headers")
	if len(headers) == 0 {
		return ""
	}
	h := headers[0].(map[string]interface{})
	if h["name"] != agentGatewayHTTPModelHeader {
		t.Errorf("first header match name = %v, want %s", h["name"], agentGatewayHTTPModelHeader)
	}
	v, _ := h["value"].(string)
	return v
}

// ensure apierrors import is used (assertNotExists relies on it).
var _ = apierrors.IsNotFound
