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
	"path/filepath"
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

// inferenceExtensionTestCRDDir is the directory of VENDORED UPSTREAM CRDs the
// agentgateway tests load alongside the base LLMKube CRDs: the real GIE v1
// InferencePool and agentgateway.dev AgentgatewayPolicy schemas (see
// test/crd/inference-extension/README.md), so envtest validates the generated
// spec shapes server-side, and the minimal HTTPRoute stub.
const inferenceExtensionTestCRDDir = "../../test/crd/inference-extension"

// startAgentGatewayTestEnv starts an envtest with (or without) the vendored
// Inference Extension / agentgateway CRDs registered alongside the base LLMKube
// CRDs, mirroring startGatewayTestEnv.
func startAgentGatewayTestEnv(t *testing.T, withCRDs bool) (client.Client, *rest.Config, func()) {
	if !withCRDs {
		return startAgentGatewayTestEnvWithCRDs(t)
	}
	return startAgentGatewayTestEnvWithCRDs(t, inferenceExtensionTestCRDDir)
}

// startAgentGatewayTestEnvWithCRDs starts an envtest loading baseCRDPath plus
// the given extra CRD directories or single CRD files, so a test can load a
// SUBSET of the vendored CRDs (for example InferencePool without
// AgentgatewayPolicy) and pin the gate's missing-kind reporting.
func startAgentGatewayTestEnvWithCRDs(t *testing.T, extraCRDPaths ...string) (client.Client, *rest.Config, func()) {
	t.Helper()
	crdPaths := append([]string{baseCRDPath}, extraCRDPaths...)
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
// produces an InferencePool per backend, the model-extraction AgentgatewayPolicy
// targeting the referenced Gateway, and a multi-rule HTTPRoute that matches the
// extracted model header and references the InferencePools directly, all
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
				Namespace:          testNS,
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

	// An InferencePool per backend, named after the referenced InferenceService.
	for _, name := range []string{"qwen-cuda", "qwen-metal"} {
		pool := getUnstructured(t, c, inferencePoolGVK(), name)
		assertOwnedByRouter(t, pool, mr)
		// The pool selects the referenced InferenceService's pods by the SAME
		// selector the Deployment uses, under the v1 matchLabels wrapper the real
		// GIE CRD requires, and targets the Service port.
		selector, _, _ := unstructured.NestedStringMap(pool.Object, "spec", "selector", "matchLabels")
		if got := selector["app"]; got != name {
			t.Errorf("pool %s selector matchLabels app = %q, want %s", name, got, name)
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
	}

	// The model-extraction AgentgatewayPolicy: agentgateway populates no model
	// header natively, so without this policy the route's model matches would
	// never fire. Pinned to the documented agentgateway BBR shape.
	policy := getUnstructured(t, c, agentGatewayPolicyGVK(), "agw-router")
	assertOwnedByRouter(t, policy, mr)
	targetRefs, _, _ := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
	if len(targetRefs) != 1 {
		t.Fatalf("policy has %d targetRefs, want 1", len(targetRefs))
	}
	targetRef := targetRefs[0].(map[string]interface{})
	if targetRef["kind"] != "Gateway" || targetRef["group"] != "gateway.networking.k8s.io" || targetRef["name"] != "ai-gateway" {
		t.Errorf("policy targetRef = %v, want Gateway gateway.networking.k8s.io/ai-gateway", targetRef)
	}
	if phase, _, _ := unstructured.NestedString(policy.Object, "spec", "traffic", "phase"); phase != "PreRouting" {
		t.Errorf("policy traffic.phase = %q, want PreRouting", phase)
	}
	sets, _, _ := unstructured.NestedSlice(policy.Object, "spec", "traffic", "transformation", "request", "set")
	if len(sets) != 1 {
		t.Fatalf("policy transformation request.set = %d entries, want 1", len(sets))
	}
	set := sets[0].(map[string]interface{})
	if set["name"] != "X-Gateway-Model-Name" {
		t.Errorf("policy extracts header %v, want X-Gateway-Model-Name", set["name"])
	}
	if set["value"] != "json(request.body).model" {
		t.Errorf("policy extraction expression = %v, want json(request.body).model", set["value"])
	}

	// One HTTPRoute named after the ModelRouter, attached to the Gateway, its
	// single rule matching qwen35-27b on the extracted header and referencing
	// both InferencePools directly (GIE v1: no InferenceModel indirection).
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
		if ref["kind"] != "InferencePool" {
			t.Errorf("backendRefs[%d].kind = %v, want InferencePool", i, ref["kind"])
		}
		if ref["group"] != "inference.networking.k8s.io" {
			t.Errorf("backendRefs[%d].group = %v, want inference.networking.k8s.io", i, ref["group"])
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

	// Generates NOTHING: no pool, policy, or route.
	assertNotExists(t, c, inferencePoolGVK(), "anthropic")
	assertNotExists(t, c, agentGatewayPolicyGVK(), "ext-router")
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
	assertNotExists(t, c, agentGatewayPolicyGVK(), "auth-router")
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
	assertNotExists(t, c, agentGatewayPolicyGVK(), "pii-router")
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
// with the required CRDs not installed, a dataPlane: AgentGateway
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
	// The message must name each missing kind, so the operator does not have to
	// read the detector to learn which CRD is meant (#1725).
	for _, want := range []string{"InferencePool", "AgentgatewayPolicy"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("condition message %q does not name missing kind %s", cond.Message, want)
		}
	}
}

// TestModelRouterAgentGateway_PolicyCRDAbsentNamesMissingKind covers the gate's
// missing-kind reporting when only SOME CRDs are installed: with the GIE
// InferencePool present but agentgateway.dev absent, the condition must name
// AgentgatewayPolicy (and not InferencePool), and nothing may be generated.
func TestModelRouterAgentGateway_PolicyCRDAbsentNamesMissingKind(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnvWithCRDs(t,
		filepath.Join(inferenceExtensionTestCRDDir, "httproute.yaml"),
		filepath.Join(inferenceExtensionTestCRDDir, "inferencepool.yaml"))
	defer stop()

	makeBackendISvc(t, c, "no-policy-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "no-policy-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "no-policy-cuda", InferenceServiceRef: corev1LocalRef("no-policy-cuda")},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "no-policy-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonCRDsMissing {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonCRDsMissing, cond)
	}
	if !strings.Contains(cond.Message, "AgentgatewayPolicy") {
		t.Errorf("condition message %q does not name AgentgatewayPolicy", cond.Message)
	}
	if strings.Contains(cond.Message, "InferencePool (") {
		t.Errorf("condition message %q must not report InferencePool as missing", cond.Message)
	}
	// The gate is the only thing between the reconciler and a loud create
	// failure; with the gate disengaged nothing may be generated.
	assertNotExists(t, c, inferencePoolGVK(), "no-policy-cuda")
}

// TestModelRouterAgentGateway_PoolCRDAbsentNamesMissingKind is the mirror case:
// agentgateway.dev present but the GIE InferencePool absent; the condition must
// name InferencePool.
func TestModelRouterAgentGateway_PoolCRDAbsentNamesMissingKind(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnvWithCRDs(t,
		filepath.Join(inferenceExtensionTestCRDDir, "agentgatewaypolicy.yaml"))
	defer stop()

	makeBackendISvc(t, c, "no-pool-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "no-pool-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "no-pool-cuda", InferenceServiceRef: corev1LocalRef("no-pool-cuda")},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "no-pool-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonCRDsMissing {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonCRDsMissing, cond)
	}
	if !strings.Contains(cond.Message, "InferencePool (inference.networking.k8s.io/v1)") {
		t.Errorf("condition message %q does not name InferencePool with its group/version", cond.Message)
	}
	if strings.Contains(cond.Message, "AgentgatewayPolicy (") {
		t.Errorf("condition message %q must not report AgentgatewayPolicy as missing", cond.Message)
	}
}

// TestModelRouterAgentGateway_CrossNamespaceGatewayFailsLoud covers the
// same-namespace boundary of the model-extraction AgentgatewayPolicy: the
// AgentgatewayPolicy CRD requires targetRefs to be same-namespace and PreRouting
// policies to target a Gateway, and a policy in the Gateway's namespace could
// not carry an owner reference to the ModelRouter. So a router whose
// agentGatewayRef points at a Gateway in another namespace is refused BEFORE any
// generation, with a condition that says how to fix it.
func TestModelRouterAgentGateway_CrossNamespaceGatewayFailsLoud(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "crossns-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "crossns-router", Namespace: testNS},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name:               "ai-gateway",
				Namespace:          "other-ns",
				EndpointPicker:     "epp",
				EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "crossns-cuda", InferenceServiceRef: corev1LocalRef("crossns-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "r",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"crossns-cuda"}},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	// Generates NOTHING.
	assertNotExists(t, c, inferencePoolGVK(), "crossns-cuda")
	assertNotExists(t, c, agentGatewayPolicyGVK(), "crossns-router")
	assertNotExists(t, c, gatewayHTTPRouteGVK(), "crossns-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "crossns-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonCrossNSGateway {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonCrossNSGateway, cond)
	}
	if !strings.Contains(cond.Message, "other-ns/ai-gateway") {
		t.Errorf("condition message %q does not name the referenced Gateway", cond.Message)
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
	assertNotExists(t, c, agentGatewayPolicyGVK(), "proxy-router")
	assertNotExists(t, c, gatewayHTTPRouteGVK(), "proxy-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "proxy-router", Namespace: testNS}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if fresh.Status.AgentGateway != nil {
		t.Errorf("expected nil status.agentGateway in Proxy mode, got %+v", fresh.Status.AgentGateway)
	}
}

// TestModelRouterAgentGateway_UndeclaredBackendRefFailsLoud covers the
// compile-time backend check: a rule (or defaultRoute) referencing a backend
// that is not declared in spec.backends must fail the reconcile loudly. Refs
// resolve to InferencePools by backend name, so an undeclared name would
// otherwise vanish silently at render time. The webhook rejects such a spec;
// these tests construct the CR directly, the webhook-bypassed path the
// reconciler must still survive.
func TestModelRouterAgentGateway_UndeclaredBackendRefFailsLoud(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	makeBackendISvc(t, c, "local-cuda")

	cases := []struct {
		name   string
		slug   string
		mutate func(*inferencev1alpha1.ModelRouterSpec)
	}{
		{
			name: "rule references an undeclared backend",
			slug: "rule-ghost-backend",
			mutate: func(s *inferencev1alpha1.ModelRouterSpec) {
				s.Rules = []inferencev1alpha1.RouterRule{{
					Name:  "r",
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"local-cuda", "ghost"}},
				}}
			},
		},
		{
			name: "defaultRoute references an undeclared backend",
			slug: "defaultroute-ghost-backend",
			mutate: func(s *inferencev1alpha1.ModelRouterSpec) {
				s.DefaultRoute = "ghost"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := &inferencev1alpha1.ModelRouter{
				ObjectMeta: metav1.ObjectMeta{Name: tc.slug, Namespace: testNS},
				Spec: inferencev1alpha1.ModelRouterSpec{
					DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
					AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
						Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
					},
					Backends: []inferencev1alpha1.RouterBackend{
						{Name: "local-cuda", InferenceServiceRef: corev1LocalRef("local-cuda")},
					},
				},
			}
			tc.mutate(&mr.Spec)
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatalf("create modelrouter: %v", err)
			}

			r := newModelRouterAgentGatewayReconciler(t, cfg)
			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
			})
			if err == nil {
				t.Fatal("expected reconcile to fail on an undeclared backend ref, got nil")
			}
			if !strings.Contains(err.Error(), "not declared in spec.backends") {
				t.Errorf("error %q does not name the undeclared backend", err.Error())
			}

			// Generates NOTHING, not even for the declared backend.
			assertNotExists(t, c, inferencePoolGVK(), "local-cuda")
			assertNotExists(t, c, agentGatewayPolicyGVK(), tc.slug)
			assertNotExists(t, c, gatewayHTTPRouteGVK(), tc.slug)

			fresh := &inferencev1alpha1.ModelRouter{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: testNS}, fresh); err != nil {
				t.Fatalf("get modelrouter: %v", err)
			}
			cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonReconcile {
				t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonReconcile, cond)
			}
		})
	}
}

// agentGatewayRouteModelOfRule extracts the model header match value from the
// first match of a route rule map, asserting the documented agentgateway BBR
// header name (the generated AgentgatewayPolicy extracts into the SAME header;
// the two must never drift apart).
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
	if h["name"] != "X-Gateway-Model-Name" {
		t.Errorf("first header match name = %v, want X-Gateway-Model-Name", h["name"])
	}
	v, _ := h["value"].(string)
	return v
}

// ensure apierrors import is used (assertNotExists relies on it).
var _ = apierrors.IsNotFound
