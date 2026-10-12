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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
// CRDs, mirroring startGatewayTestEnv. With CRDs, the API server is SHARED:
// one envtest per distinct CRD set, started lazily, stopped by TestMain. Every
// test works in its own generated namespace (newTestNamespace), so tests stay
// independent on the shared server. Without CRDs - or with a SUBSET, which is
// how the missing-kind tests pin the gate - the test keeps a private
// environment, because those tests need CRDs ABSENT.
func startAgentGatewayTestEnv(t *testing.T, withCRDs bool) (client.Client, *rest.Config, func()) {
	if !withCRDs {
		return startAgentGatewayTestEnvWithCRDs(t)
	}
	return startSharedAgentGatewayTestEnv(t, baseCRDPath, inferenceExtensionTestCRDDir)
}

// Shared envtest state. Starting an envtest is an apiserver plus an etcd
// (~4s locally, more on CI); before sharing, this suite started 23 of them and
// pushed the package past Go's 10 minute test timeout.
var (
	sharedAgentGatewayMu   sync.Mutex
	sharedAgentGatewayEnvs = map[string]*sharedAgentGatewayEnv{}
)

type sharedAgentGatewayEnv struct {
	env    *envtest.Environment
	cfg    *rest.Config
	client client.Client
}

// startSharedAgentGatewayTestEnv returns the shared envtest for the given CRD
// path set, starting it lazily on first use. The returned stop function is a
// no-op: the environment is stopped by TestMain after all tests have run.
func startSharedAgentGatewayTestEnv(t *testing.T, crdPaths ...string) (client.Client, *rest.Config, func()) {
	t.Helper()
	key := strings.Join(crdPaths, "\x00")
	sharedAgentGatewayMu.Lock()
	shared, ok := sharedAgentGatewayEnvs[key]
	if !ok {
		env := &envtest.Environment{
			CRDDirectoryPaths:     crdPaths,
			ErrorIfCRDPathMissing: true,
		}
		if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
			env.BinaryAssetsDirectory = dir
		}
		cfg, err := env.Start()
		if err != nil {
			sharedAgentGatewayMu.Unlock()
			t.Fatalf("start shared envtest: %v", err)
		}
		if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
			sharedAgentGatewayMu.Unlock()
			t.Fatalf("add scheme: %v", err)
		}
		c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			sharedAgentGatewayMu.Unlock()
			t.Fatalf("new shared client: %v", err)
		}
		shared = &sharedAgentGatewayEnv{env: env, cfg: cfg, client: c}
		sharedAgentGatewayEnvs[key] = shared
	}
	sharedAgentGatewayMu.Unlock()
	return shared.client, shared.cfg, func() {}
}

// stopSharedAgentGatewayEnvs stops every shared envtest environment. Called
// from TestMain after all tests have run.
func stopSharedAgentGatewayEnvs() {
	sharedAgentGatewayMu.Lock()
	defer sharedAgentGatewayMu.Unlock()
	for _, shared := range sharedAgentGatewayEnvs {
		_ = shared.env.Stop()
	}
	sharedAgentGatewayEnvs = map[string]*sharedAgentGatewayEnv{}
}

// TestMain stops the shared envtest environments after all tests have run; a
// shared environment outlives any single test's stop function.
func TestMain(m *testing.M) {
	code := m.Run()
	stopSharedAgentGatewayEnvs()
	os.Exit(code)
}

// startAgentGatewayTestEnvWithCRDs starts an envtest loading baseCRDPath plus
// the given extra CRD directories or single CRD files, so a test can load a
// SUBSET of the vendored CRDs (for example InferencePool without
// AgentgatewayPolicy) and pin the gate's missing-kind reporting. Private by
// design: these tests need CRDs ABSENT.
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
	ns := newTestNamespace(t, c)

	makeBackendISvcInNamespace(t, c, ns, "qwen-cuda")
	makeGateway(t, c, ns, "ai-gateway", nil, nil)

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "agw-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name:               "ai-gateway",
				Namespace:          ns,
				EndpointPicker:     "llm-d-router-epp",
				EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "qwen-cuda", InferenceServiceRef: corev1LocalRef("qwen-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "qwen",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{
						Backends: []string{"qwen-cuda"},
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
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Ready routers recheck periodically: a foreign PreRouting policy created
	// after this reconcile would otherwise be silently shadowed until an
	// unrelated event.
	if res.RequeueAfter != agentGatewayConflictRequeueAfter {
		t.Errorf("requeue after = %v, want %v (the conflict recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
	}

	// An InferencePool for the backend, named after the referenced
	// InferenceService. One backend: a router compiling to several pools is
	// rejected (TestModelRouterAgentGateway_MultiPoolRejected), since a GIE EPP
	// binds to exactly one pool.
	for _, name := range []string{"qwen-cuda"} {
		pool := getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, name)
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
	policy := getUnstructuredInNamespace(t, c, agentGatewayPolicyGVK(), ns, "agw-router")
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
	// the InferencePool directly (GIE v1: no InferenceModel indirection).
	route := getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "agw-router")
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
	if len(refs) != 1 {
		t.Fatalf("rule has %d backendRefs, want 1", len(refs))
	}
	ref := refs[0].(map[string]interface{})
	if ref["name"] != "qwen-cuda" {
		t.Errorf("backendRefs[0].name = %v, want qwen-cuda", ref["name"])
	}
	if ref["kind"] != "InferencePool" {
		t.Errorf("backendRefs[0].kind = %v, want InferencePool", ref["kind"])
	}
	if ref["group"] != "inference.networking.k8s.io" {
		t.Errorf("backendRefs[0].group = %v, want inference.networking.k8s.io", ref["group"])
	}

	// status.agentGateway + AgentGatewayReady=True.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "agw-router", Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter status: %v", err)
	}
	if fresh.Status.AgentGateway == nil || !fresh.Status.AgentGateway.PoolReady {
		t.Errorf("status.agentGateway.poolReady not set, got %+v", fresh.Status.AgentGateway)
	}
	if cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("AgentGatewayReady condition not True, got %+v", cond)
	}
}

// TestModelRouterAgentGateway_MultiPoolRejected covers the single-picker
// boundary: backends on two distinct InferenceServices compile to two
// InferencePools, but agentGatewayRef names ONE Endpoint Picker and a GIE EPP
// binds to exactly one pool, so the compiled shape would make endpoint
// decisions for the wrong pool. The router is refused with a named condition
// and generates nothing.
func TestModelRouterAgentGateway_MultiPoolRejected(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeBackendISvcInNamespace(t, c, ns, "mp-cuda")
	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "mp-metal")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "mp-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "mp-cuda", InferenceServiceRef: corev1LocalRef("mp-cuda")},
				{Name: "mp-metal", InferenceServiceRef: corev1LocalRef("mp-metal")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "r",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"mp-cuda", "mp-metal"}},
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
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "mp-cuda")
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "mp-metal")
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, "mp-router")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "mp-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonMultiPool {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonMultiPool, cond)
	}
	if !strings.Contains(cond.Message, "exactly one pool") {
		t.Errorf("condition message %q does not explain the single-EPP constraint", cond.Message)
	}
}

// TestModelRouterAgentGateway_PoolDedupSameInferenceService covers pool dedup:
// two RouterBackends referencing the SAME InferenceService compile to ONE
// InferencePool (one pool is one EPP's worth of routing), and a rule
// referencing both backends renders a single backendRef. If the dedup in
// compileAgentGatewayRouteBackendRefs is removed, this test fails on the
// duplicated ref.
func TestModelRouterAgentGateway_PoolDedupSameInferenceService(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeBackendISvcInNamespace(t, c, ns, "dedup-cuda")
	makeGateway(t, c, ns, "ai-gateway", nil, nil)

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "dedup-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			// Two backends, one InferenceService: one distinct pool, allowed.
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "dedup-a", InferenceServiceRef: corev1LocalRef("dedup-cuda")},
				{Name: "dedup-b", InferenceServiceRef: corev1LocalRef("dedup-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "r",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{"dedup-a", "dedup-b"}},
				},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	// Exactly ONE InferencePool, though two backends requested one each.
	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(schema.GroupVersionKind{
		Group: inferenceExtensionGroup, Version: inferenceExtensionVersion, Kind: "InferencePoolList",
	})
	if err := c.List(context.Background(), pools, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools.Items) != 1 {
		t.Fatalf("expected exactly 1 InferencePool for 2 backends on one InferenceService, got %d", len(pools.Items))
	}

	// The rule references the pool ONCE.
	route := getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "dedup-router")
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("route has %d rules, want 1", len(rules))
	}
	refs := rules[0].(map[string]interface{})["backendRefs"].([]interface{})
	if len(refs) != 1 {
		t.Errorf("rule has %d backendRefs, want 1 (pool dedup removed?)", len(refs))
	}
}

// TestModelRouterAgentGateway_ExternalBackendFailsLoud covers issue #4: an
// External backend (which the Gateway plane compiles but the agentgateway
// InferencePool path cannot, since a pool selects model-server pods) is a loud
// unsupported-field error, not a silent reduction.
func TestModelRouterAgentGateway_ExternalBackendFailsLoud(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "ext-router", Namespace: ns},
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
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "anthropic")
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, "ext-router")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "ext-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ext-router", Namespace: ns}, fresh); err != nil {
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
	ns := newTestNamespace(t, c)

	makeBackendISvcInNamespace(t, c, ns, "local-cuda")
	makeGateway(t, c, ns, "ai-gateway", nil, nil)

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-router", Namespace: ns},
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
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "local-cuda")
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, "auth-router")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "auth-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "auth-router", Namespace: ns}, fresh); err != nil {
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
	ns := newTestNamespace(t, c)

	makeBackendISvcInNamespace(t, c, ns, "local-cuda")
	makeGateway(t, c, ns, "ai-gateway", nil, nil)

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "pii-router", Namespace: ns},
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

	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "local-cuda")
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, "pii-router")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "pii-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pii-router", Namespace: ns}, fresh); err != nil {
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

// makeNamespace creates a namespace for the cross-namespace tests; envtest
// ships only the default namespace.
func makeNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// makeGateway creates the referenced Gateway with the given listeners (nil
// means one plain HTTP listener with NO allowedRoutes, which Gateway API
// defaults to namespaces.from: Same). The permissive gateway.yaml stub stores
// any listener shape; the operator never writes Gateways.
func makeGateway(t *testing.T, c client.Client, ns, name string, labels map[string]string, listeners []interface{}) {
	t.Helper()
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
	gw.SetName(name)
	gw.SetNamespace(ns)
	gw.SetLabels(labels)
	if listeners == nil {
		listeners = []interface{}{map[string]interface{}{
			"name": "http", "port": map[string]interface{}{"number": int64(80)}, "protocol": "HTTP",
		}}
	}
	gw.Object["spec"] = map[string]interface{}{"listeners": listeners}
	if err := c.Create(context.Background(), gw); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create Gateway %s/%s: %v", ns, name, err)
	}
}

// gatewayListenerAllowingAll is an HTTP listener whose allowedRoutes admits
// routes from every namespace.
func gatewayListenerAllowingAll() map[string]interface{} {
	return map[string]interface{}{
		"name": "http", "port": map[string]interface{}{"number": int64(80)}, "protocol": "HTTP",
		"allowedRoutes": map[string]interface{}{"namespaces": map[string]interface{}{"from": "All"}},
	}
}

// crossNamespaceRouterFixture builds a dataPlane: AgentGateway router in
// testNS whose agentGatewayRef points at Gateway "ai-gateway" in gwNS. The
// backend InferenceService (svc, in testNS) must exist before reconciling.
func crossNamespaceRouterFixture(ns, name, gwNS, svc string) *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name:               "ai-gateway",
				Namespace:          gwNS,
				EndpointPicker:     "epp",
				EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: svc, InferenceServiceRef: corev1LocalRef(svc)},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "r",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{svc}},
				},
			},
		},
	}
}

// newTestNamespace creates a namespace with a generated name on the shared
// API server, so tests that share the server never collide.
func newTestNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "agw-test-"}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create test namespace: %v", err)
	}
	return ns.Name
}

// makeBackendISvcInNamespace creates a ready backend InferenceService in an
// explicit namespace (makeBackendISvc is pinned to testNS).
func makeBackendISvcInNamespace(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: name,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create backend isvc %s: %v", name, err)
	}
	setISvcReadyReplicasInNamespace(t, c, ns, name, 1)
}

// setISvcReadyReplicasInNamespace writes Status.ReadyReplicas on a backend
// InferenceService in an explicit namespace (setISvcReadyReplicas is pinned to
// testNS).
func setISvcReadyReplicasInNamespace(t *testing.T, c client.Client, ns, name string, replicas int32) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, isvc); err != nil {
		t.Fatalf("get backend isvc %s: %v", name, err)
	}
	isvc.Status.ReadyReplicas = replicas
	if err := c.Status().Update(context.Background(), isvc); err != nil {
		t.Fatalf("update backend isvc %s status: %v", name, err)
	}
}

// getUnstructuredInNamespace fetches one unstructured object from an explicit
// namespace (getUnstructured is pinned to testNS).
func getUnstructuredInNamespace(t *testing.T, c client.Client, gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, u)
	if err != nil {
		t.Fatalf("get %s %s/%s: %v", gvk.Kind, ns, name, err)
	}
	return u
}

// sameNamespaceRouterFixture builds a dataPlane: AgentGateway router in the
// test namespace whose agentGatewayRef has no namespace (a same-namespace
// Gateway named ai-gateway) and whose single backend references svc.
func sameNamespaceRouterFixture(ns, name, svc string) *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name:               "ai-gateway",
				EndpointPicker:     "epp",
				EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: svc, InferenceServiceRef: corev1LocalRef(svc)},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{
					Name:  "r",
					Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}},
					Route: inferencev1alpha1.RuleRoute{Backends: []string{svc}},
				},
			},
		},
	}
}

// getPolicyInNamespace fetches one AgentgatewayPolicy from an explicit
// namespace (getUnstructured is pinned to testNS).
func getPolicyInNamespace(t *testing.T, c client.Client, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(agentGatewayPolicyGVK())
	u.SetNamespace(ns)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, u)
	if err != nil {
		t.Fatalf("get AgentgatewayPolicy %s/%s: %v", ns, name, err)
	}
	return u
}

// assertNoPolicyInNamespace asserts an AgentgatewayPolicy is absent from an
// explicit namespace (assertNotExists is pinned to testNS).
func assertNoPolicyInNamespace(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(agentGatewayPolicyGVK())
	u.SetNamespace(ns)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, u)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected AgentgatewayPolicy %s/%s to not exist, get err = %v", ns, name, err)
	}
}

// assertNotExistsInNamespace asserts an unstructured object of the given GVK
// is absent from an explicit namespace (assertNotExists is pinned to testNS).
func assertNotExistsInNamespace(t *testing.T, c client.Client, gvk schema.GroupVersionKind, ns, name string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, u)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected %s %s/%s to not exist, get err = %v", gvk.Kind, ns, name, err)
	}
}

// assertExtractionPolicyShape asserts the generated model-extraction
// AgentgatewayPolicy: the documented BBR shape, the router-UID label, and (for
// a same-namespace policy) a controller owner reference. Cross-namespace
// policies cannot carry that reference.
func assertExtractionPolicyShape(t *testing.T, p *unstructured.Unstructured, mr *inferencev1alpha1.ModelRouter, sameNamespace bool) {
	t.Helper()
	targetRefs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if len(targetRefs) != 1 {
		t.Fatalf("policy has %d targetRefs, want 1", len(targetRefs))
	}
	targetRef := targetRefs[0].(map[string]interface{})
	if targetRef["kind"] != "Gateway" || targetRef["group"] != "gateway.networking.k8s.io" || targetRef["name"] != "ai-gateway" {
		t.Errorf("policy targetRef = %v, want Gateway gateway.networking.k8s.io/ai-gateway", targetRef)
	}
	if phase, _, _ := unstructured.NestedString(p.Object, "spec", "traffic", "phase"); phase != "PreRouting" {
		t.Errorf("policy traffic.phase = %q, want PreRouting", phase)
	}
	sets, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "transformation", "request", "set")
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
	if got := p.GetLabels()[agentGatewayRouterUIDLabel]; got != string(mr.UID) {
		t.Errorf("policy uid label = %q, want the router's uid %q", got, string(mr.UID))
	}
	refs := p.GetOwnerReferences()
	if sameNamespace {
		assertOwnedByRouter(t, p, mr)
	} else if len(refs) != 0 {
		t.Errorf("cross-namespace policy has %d owner references, want 0 (owner refs cannot cross namespaces)", len(refs))
	}
}

// TestModelRouterAgentGateway_CrossNamespaceGatewayGeneratesPolicyInGatewayNS
// covers the common setup the issue reporter runs: the ModelRouter in one
// namespace, the agentgateway Gateway in another. The extraction policy cannot
// be owner-referenced across namespaces and must target the Gateway
// same-namespace, so it is generated in the GATEWAY's namespace, labeled with
// the router's UID, and guarded by a finalizer on the router. Pools and the
// route stay in the router's namespace.
func TestModelRouterAgentGateway_CrossNamespaceGatewayGeneratesPolicyInGatewayNS(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	gwNS := newTestNamespace(t, c)
	makeBackendISvcInNamespace(t, c, ns, "crossns-cuda")
	makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})

	mr := crossNamespaceRouterFixture(ns, "crossns-router", gwNS, "crossns-cuda")
	_, policyName := agentGatewayPolicyIdentity(mr)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	// Pools and route in the router's namespace, owned as before.
	pool := getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, "crossns-cuda")
	assertOwnedByRouter(t, pool, mr)
	route := getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "crossns-router")
	assertOwnedByRouter(t, route, mr)

	// The extraction policy in the GATEWAY's namespace: no owner reference,
	// UID-labeled, same BBR shape as the same-namespace case.
	policy := getPolicyInNamespace(t, c, gwNS, policyName)
	assertExtractionPolicyShape(t, policy, mr, false)

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if !controllerutil.ContainsFinalizer(fresh, modelRouterAgentGatewayFinalizer) {
		t.Errorf("router missing %s finalizer; the cross-namespace policy would be stranded on delete", modelRouterAgentGatewayFinalizer)
	}
	if cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("AgentGatewayReady condition not True, got %+v", cond)
	}
}

// TestModelRouterAgentGateway_GatewayNotFoundRefuses covers the missing-Gateway
// refusal: previously a router whose agentGatewayRef named a Gateway that does
// not exist went Ready while generating objects nothing would ever serve
// through. The reconciler now refuses with a named condition.
func TestModelRouterAgentGateway_GatewayNotFoundRefuses(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeBackendISvcInNamespace(t, c, ns, "missing-gw-cuda")
	// No Gateway created: the ref points at one that does not exist.

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-gw-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
			},
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "missing-gw-cuda", InferenceServiceRef: corev1LocalRef("missing-gw-cuda")},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "r", Route: inferencev1alpha1.RuleRoute{Backends: []string{"missing-gw-cuda"}}},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Nothing watches Gateways, so the refusal must requeue: a router applied
	// before its Gateway (normal GitOps order) recovers on the next recheck.
	if res.RequeueAfter != agentGatewayConflictRequeueAfter {
		t.Errorf("requeue after = %v, want %v (the consent recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
	}

	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "missing-gw-cuda")
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, "missing-gw-router")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "missing-gw-router")

	refused := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, refused); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(refused.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonGatewayNotFound {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonGatewayNotFound, cond)
	}
	if !strings.Contains(cond.Message, ns+"/ai-gateway") {
		t.Errorf("condition message %q does not name the missing Gateway", cond.Message)
	}
	if controllerutil.ContainsFinalizer(refused, modelRouterAgentGatewayFinalizer) {
		t.Errorf("finalizer present on a refused router; nothing was generated")
	}

	// The Gateway appears: the next reconcile goes Ready.
	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	reconcileAgentGatewayRouter(t, r, mr)
	getPolicyInNamespace(t, c, ns, "missing-gw-router")
}

// TestModelRouterAgentGateway_DeletedGatewayWithdrawsPolicy covers the
// GatewayNotFound half of consent withdrawal. The NotAllowed half is covered
// by TestModelRouterAgentGateway_RevokedConsentWithdrawsPolicy; without this
// test, disabling the withdrawal on the GatewayNotFound path alone leaves the
// suite green. A Gateway that vanishes must take the Gateway-wide policy and
// the finalizer with it.
func TestModelRouterAgentGateway_DeletedGatewayWithdrawsPolicy(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	gwNS := newTestNamespace(t, c)
	makeBackendISvcInNamespace(t, c, ns, "gonegw-cuda")
	makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})

	mr := crossNamespaceRouterFixture(ns, "gonegw-router", gwNS, "gonegw-cuda")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	_, policyName := agentGatewayPolicyIdentity(mr)
	getPolicyInNamespace(t, c, gwNS, policyName)
	ready := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, ready); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if !controllerutil.ContainsFinalizer(ready, modelRouterAgentGatewayFinalizer) {
		t.Fatal("expected the finalizer on a Ready cross-namespace router")
	}

	// The Gateway disappears.
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
	gw.SetName("ai-gateway")
	gw.SetNamespace(gwNS)
	if err := c.Delete(context.Background(), gw); err != nil {
		t.Fatalf("delete gateway: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertNoPolicyInNamespace(t, c, gwNS, policyName)
	gone := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, gone); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if controllerutil.ContainsFinalizer(gone, modelRouterAgentGatewayFinalizer) {
		t.Errorf("finalizer still present after the Gateway disappeared; the withdrawal guard is gone")
	}
	cond := apimeta.FindStatusCondition(gone.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonGatewayNotFound {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonGatewayNotFound, cond)
	}
}

// TestModelRouterAgentGateway_GatewayDoesNotAdmitRouterNamespace covers the
// consent check: writing the model-extraction AgentgatewayPolicy into the
// Gateway's namespace requires a listener whose allowedRoutes admits the
// router's namespace (Same by default, All, or a Selector matching the router
// namespace's labels). Without this check any router in any namespace could
// make the operator inject body parsing into every request on a Gateway it
// does not own; the round-2 review reproduced exactly that on a live cluster.
func TestModelRouterAgentGateway_GatewayDoesNotAdmitRouterNamespace(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	gwNS := newTestNamespace(t, c)

	listener := func(from string, selector map[string]interface{}) map[string]interface{} {
		l := map[string]interface{}{
			"name": "http", "port": map[string]interface{}{"number": int64(80)}, "protocol": "HTTP",
		}
		if from != "" {
			ns := map[string]interface{}{"from": from}
			if selector != nil {
				ns["selector"] = selector
			}
			l["allowedRoutes"] = map[string]interface{}{"namespaces": ns}
		}
		return l
	}

	// listenerWith builds a listener with an explicit protocol and an optional
	// allowedRoutes.kinds list. namespaces.from is All so the protocol and
	// kinds criteria are what the subtest exercises.
	listenerWith := func(protocol string, kinds []interface{}) map[string]interface{} {
		ar := map[string]interface{}{
			"namespaces": map[string]interface{}{"from": "All"},
		}
		if kinds != nil {
			ar["kinds"] = kinds
		}
		return map[string]interface{}{
			"name": "http", "port": map[string]interface{}{"number": int64(80)}, "protocol": protocol,
			"allowedRoutes": ar,
		}
	}

	cases := []struct {
		name           string
		listeners      []interface{}
		routerNSLabels map[string]string
		wantReady      bool
	}{
		{
			// One listener with NO allowedRoutes key: Gateway API defaults
			// namespaces.from to Same, so absence never admits a
			// cross-namespace router. The listener is built explicitly rather
			// than relying on makeGateway's nil-listener default.
			name:      "unset allowedRoutes defaults to Same and refuses a cross-namespace router",
			listeners: []interface{}{listener("", nil)},
			wantReady: false,
		},
		{
			name:      "Same refuses a cross-namespace router",
			listeners: []interface{}{listener("Same", nil)},
			wantReady: false,
		},
		{
			name:      "All admits a cross-namespace router",
			listeners: []interface{}{listener("All", nil)},
			wantReady: true,
		},
		{
			name:           "Selector matching the router namespace's labels admits",
			listeners:      []interface{}{listener("Selector", map[string]interface{}{"matchLabels": map[string]interface{}{"team": "llm"}})},
			routerNSLabels: map[string]string{"team": "llm"},
			wantReady:      true,
		},
		{
			name:           "Selector not matching the router namespace's labels refuses",
			listeners:      []interface{}{listener("Selector", map[string]interface{}{"matchLabels": map[string]interface{}{"team": "other"}})},
			routerNSLabels: map[string]string{"team": "llm"},
			wantReady:      false,
		},
		{
			name:      "invalid selector refuses (fail closed)",
			listeners: []interface{}{listener("Selector", map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{"key": "team", "operator": "Bogus", "values": []interface{}{"x"}}}})},
			wantReady: false,
		},
		{
			name:      "a listener that cannot carry HTTPRoute (TLS) refuses",
			listeners: []interface{}{listenerWith("TLS", nil)},
			wantReady: false,
		},
		{
			name:      "allowedRoutes.kinds without HTTPRoute refuses",
			listeners: []interface{}{listenerWith("HTTP", []interface{}{map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "GRPCRoute"}})},
			wantReady: false,
		},
		{
			name:      "allowedRoutes.kinds containing HTTPRoute admits",
			listeners: []interface{}{listenerWith("HTTP", []interface{}{map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute"}})},
			wantReady: true,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routerNS := ns
			if tc.routerNSLabels != nil {
				routerNS = fmt.Sprintf("consent-ns-%d", i+1)
				makeNamespace(t, c, routerNS)
				ns := &corev1.Namespace{}
				if err := c.Get(context.Background(), types.NamespacedName{Name: routerNS}, ns); err != nil {
					t.Fatalf("get namespace: %v", err)
				}
				ns.SetLabels(tc.routerNSLabels)
				if err := c.Update(context.Background(), ns); err != nil {
					t.Fatalf("label namespace: %v", err)
				}
			}
			svc := fmt.Sprintf("consent-svc-%d", i+1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: svc, Namespace: routerNS},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: svc, Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
				},
			}
			if err := c.Create(context.Background(), isvc); err != nil {
				t.Fatalf("create isvc: %v", err)
			}
			isvc.Status.ReadyReplicas = 1
			if err := c.Status().Update(context.Background(), isvc); err != nil {
				t.Fatalf("ready isvc: %v", err)
			}

			gwName := fmt.Sprintf("ai-gateway-%d", i+1)
			makeGateway(t, c, gwNS, gwName, nil, tc.listeners)

			mr := &inferencev1alpha1.ModelRouter{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("consent-router-%d", i+1), Namespace: routerNS},
				Spec: inferencev1alpha1.ModelRouterSpec{
					DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
					AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
						Name: gwName, Namespace: gwNS, EndpointPicker: "epp", EndpointPickerPort: 9002,
					},
					Backends: []inferencev1alpha1.RouterBackend{
						{Name: svc, InferenceServiceRef: corev1LocalRef(svc)},
					},
					Rules: []inferencev1alpha1.RouterRule{
						{Name: "r", Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}}, Route: inferencev1alpha1.RuleRoute{Backends: []string{svc}}},
					},
				},
			}
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatalf("create modelrouter: %v", err)
			}

			r := newModelRouterAgentGatewayReconciler(t, cfg)
			res, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
			})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			fresh := &inferencev1alpha1.ModelRouter{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: routerNS}, fresh); err != nil {
				t.Fatalf("get modelrouter: %v", err)
			}
			cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
			policyName := fmt.Sprintf("%s-consent-router-%d", strings.ReplaceAll(routerNS, ".", "-"), i+1)
			if tc.wantReady {
				if cond == nil || cond.Status != metav1.ConditionTrue {
					t.Fatalf("expected AgentGatewayReady=True, got %+v (%s)", cond, condMessage(cond))
				}
				getPolicyInNamespace(t, c, gwNS, policyName)
				return
			}
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonNotAllowed {
				t.Fatalf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonNotAllowed, cond)
			}
			if !strings.Contains(cond.Message, gwNS+"/"+gwName) {
				t.Errorf("condition message %q does not name the Gateway", cond.Message)
			}
			if !strings.Contains(cond.Message, routerNS) {
				t.Errorf("condition message %q does not name the router namespace", cond.Message)
			}
			// Nothing may be written into the Gateway's namespace, and the
			// router's own namespace stays empty too.
			assertNoPolicyInNamespace(t, c, gwNS, policyName)
			assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, svc)
			assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, mr.Name)
			if controllerutil.ContainsFinalizer(fresh, modelRouterAgentGatewayFinalizer) {
				t.Errorf("finalizer present on a refused router; nothing was generated")
			}
			_ = res
		})
	}
}

// TestModelRouterAgentGateway_RevokedConsentWithdrawsPolicy covers consent
// withdrawal: the consent check gates creation but, before this fix, never
// withdrew - a Gateway owner who flipped allowedRoutes from All to Same left
// the Gateway-wide body-extraction policy in place (reproduced live: the
// router flipped to GatewayDoesNotAllowRouterNamespace at the 2-minute requeue
// while the policy stayed). A revoked (or vanished) Gateway must take the
// policy and the finalizer with it.
func TestModelRouterAgentGateway_RevokedConsentWithdrawsPolicy(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	gwNS := newTestNamespace(t, c)
	makeBackendISvcInNamespace(t, c, ns, "revoke-cuda")
	makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})

	mr := crossNamespaceRouterFixture(ns, "revoke-router", gwNS, "revoke-cuda")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	_, policyName := agentGatewayPolicyIdentity(mr)
	getPolicyInNamespace(t, c, gwNS, policyName)
	ready := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, ready); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if !controllerutil.ContainsFinalizer(ready, modelRouterAgentGatewayFinalizer) {
		t.Fatal("expected the finalizer on a Ready cross-namespace router")
	}

	// The Gateway owner revokes consent.
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ai-gateway", Namespace: gwNS}, gw); err != nil {
		t.Fatalf("get gateway: %v", err)
	}
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	listeners[0].(map[string]interface{})["allowedRoutes"] = map[string]interface{}{
		"namespaces": map[string]interface{}{"from": "Same"},
	}
	gw.Object["spec"].(map[string]interface{})["listeners"] = listeners
	if err := c.Update(context.Background(), gw); err != nil {
		t.Fatalf("revoke consent: %v", err)
	}

	revokedRes, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The refusal must requeue so restored consent is noticed without an
	// unrelated event.
	if revokedRes.RequeueAfter != agentGatewayConflictRequeueAfter {
		t.Errorf("requeue after = %v, want %v (the consent recheck)", revokedRes.RequeueAfter, agentGatewayConflictRequeueAfter)
	}

	assertNoPolicyInNamespace(t, c, gwNS, policyName)
	revoked := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, revoked); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if controllerutil.ContainsFinalizer(revoked, modelRouterAgentGatewayFinalizer) {
		t.Errorf("finalizer still present after consent was revoked; the withdrawal guard is gone")
	}
	cond := apimeta.FindStatusCondition(revoked.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonNotAllowed {
		t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonNotAllowed, cond)
	}

	// The Gateway owner restores consent: the requeued reconcile goes Ready
	// and regenerates the policy.
	listeners, _, _ = unstructured.NestedSlice(gw.Object, "spec", "listeners")
	listeners[0].(map[string]interface{})["allowedRoutes"] = map[string]interface{}{
		"namespaces": map[string]interface{}{"from": "All"},
	}
	gw.Object["spec"].(map[string]interface{})["listeners"] = listeners
	if err := c.Update(context.Background(), gw); err != nil {
		t.Fatalf("restore consent: %v", err)
	}
	reconcileAgentGatewayRouter(t, r, mr)
	getPolicyInNamespace(t, c, gwNS, policyName)
}

// condMessage is a small helper for failure output.
func condMessage(cond *metav1.Condition) string {
	if cond == nil {
		return "<nil>"
	}
	return cond.Message
}

// TestModelRouterAgentGateway_CleanupToleratesRemovedCRD covers the finalizer
// wedge the round-2 review reproduced live: after the AgentgatewayPolicy CRD
// is deleted while the operator runs, the CACHED REST mapping keeps resolving
// and the List returns 404 (IsNotFound, not NoMatch), which looped a router in
// Terminating until an operator restart. Cleanup and the conflict List must
// treat NotFound like NoMatch (the API server removed the CRs with the CRD),
// and a cleanup that still fails must surface a Warning event, because a
// status patch is futile on a terminating object.
func TestModelRouterAgentGateway_CleanupToleratesRemovedCRD(t *testing.T) {
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{
			Name: "crdgone-router", Namespace: testNS,
			Finalizers: []string{modelRouterAgentGatewayFinalizer},
		},
	}
	mr.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneAgentGateway
	mr.Spec.AgentGatewayRef = &inferencev1alpha1.AgentGatewayReference{
		Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
	}

	notFoundList := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return apierrors.NewNotFound(schema.GroupResource{Group: "agentgateway.dev", Resource: "agentgatewaypolicies"}, "")
		},
	}

	t.Run("cleanup tolerates the 404 a cached mapping returns after CRD removal", func(t *testing.T) {
		s := runtime.NewScheme()
		if err := inferencev1alpha1.AddToScheme(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
		c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(notFoundList).Build()
		r := &ModelRouterAgentGatewayReconciler{Client: c, Scheme: s}
		if err := r.cleanupExtractionPolicies(context.Background(), mr, ""); err != nil {
			t.Fatalf("cleanup must tolerate NotFound like NoMatch, got %v", err)
		}
	})

	t.Run("conflict list tolerates the same 404", func(t *testing.T) {
		s := runtime.NewScheme()
		if err := inferencev1alpha1.AddToScheme(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
		c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(notFoundList).Build()
		r := &ModelRouterAgentGatewayReconciler{Client: c, Scheme: s}
		conflicts, err := r.agentGatewayPreRoutingConflicts(context.Background(), mr, nil)
		if err != nil {
			t.Fatalf("conflict list must tolerate NotFound like NoMatch, got %v", err)
		}
		if len(conflicts) != 0 {
			t.Errorf("expected no conflicts, got %v", conflicts)
		}
	})

	t.Run("cleanup failure emits a warning event", func(t *testing.T) {
		s := runtime.NewScheme()
		if err := inferencev1alpha1.AddToScheme(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
		now := metav1.Now()
		deleting := mr.DeepCopy()
		deleting.DeletionTimestamp = &now
		rec := events.NewFakeRecorder(4)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(deleting).WithStatusSubresource(deleting).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return fmt.Errorf("rbac denied")
			},
		}).Build()
		r := &ModelRouterAgentGatewayReconciler{Client: c, Scheme: s, Recorder: rec}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: testNS},
		}); err == nil {
			t.Fatal("expected an error from a failing cleanup")
		}
		select {
		case ev := <-rec.Events:
			if !strings.Contains(ev, "deletion blocked") {
				t.Errorf("event %q does not say the deletion is blocked", ev)
			}
		default:
			t.Fatal("no event emitted; a stuck deletion would be invisible")
		}
	})
}

// TestModelRouterAgentGateway_CrossNamespacePolicyCleanedUpOnDelete covers the
// finalizer's reason to exist: deleting a router whose extraction policy lives
// in another namespace (no owner reference could GC it) must delete that
// policy and release the router.
func TestModelRouterAgentGateway_CrossNamespacePolicyCleanedUpOnDelete(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	gwNS := newTestNamespace(t, c)
	makeBackendISvcInNamespace(t, c, ns, "crossns-cuda")
	makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})

	mr := crossNamespaceRouterFixture(ns, "crossns-router", gwNS, "crossns-cuda")
	_, policyName := agentGatewayPolicyIdentity(mr)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)
	// Sanity: the policy exists before the delete.
	getPolicyInNamespace(t, c, gwNS, policyName)

	if err := c.Delete(context.Background(), mr); err != nil {
		t.Fatalf("delete modelrouter: %v", err)
	}
	reconcileAgentGatewayRouter(t, r, mr)

	assertNoPolicyInNamespace(t, c, gwNS, policyName)
	// With the finalizer removed, the API server completes the deletion.
	fresh := &inferencev1alpha1.ModelRouter{}
	err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected the router to be gone after finalizer cleanup, get err = %v", err)
	}
	// The pools and route in testNS are owner-referenced and are NOT asserted
	// gone: envtest runs no garbage collector, so owner-ref'd objects linger
	// here while a real cluster reclaims them.
}

// TestModelRouterAgentGateway_CrossNamespaceRefChangePrunes covers a router
// whose agentGatewayRef.namespace moves back to the router's own namespace:
// the policy left in the old Gateway's namespace must be deleted (nothing
// owner-references it there), the finalizer dropped, and the same-namespace
// policy generated owner-referenced instead.
func TestModelRouterAgentGateway_CrossNamespaceRefChangePrunes(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	gwNS := newTestNamespace(t, c)
	makeBackendISvcInNamespace(t, c, ns, "crossns-cuda")
	makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})
	// When the ref moves back to the router's namespace, THIS is the Gateway
	// the router references (the consent check must admit it too).
	makeGateway(t, c, ns, "ai-gateway", nil, nil)

	mr := crossNamespaceRouterFixture(ns, "crossns-router", gwNS, "crossns-cuda")
	_, policyName := agentGatewayPolicyIdentity(mr)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)
	getPolicyInNamespace(t, c, gwNS, policyName)

	// Move the Gateway back into the router's namespace.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	fresh.Spec.AgentGatewayRef.Namespace = ""
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("update modelrouter: %v", err)
	}
	// First reconcile runs the cleanup path (prune + drop the finalizer) and
	// returns; the second compiles the same-namespace policy.
	reconcileAgentGatewayRouter(t, r, fresh)
	reconcileAgentGatewayRouter(t, r, fresh)

	assertNoPolicyInNamespace(t, c, gwNS, policyName)
	sameNSPolicy := getUnstructuredInNamespace(t, c, agentGatewayPolicyGVK(), ns, "crossns-router")
	assertExtractionPolicyShape(t, sameNSPolicy, fresh, true)

	final := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, final); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if controllerutil.ContainsFinalizer(final, modelRouterAgentGatewayFinalizer) {
		t.Errorf("finalizer still present after the Gateway moved into the router's namespace")
	}
}

// TestModelRouterAgentGateway_CrossNamespaceGatewayMovePrunes covers a router
// whose agentGatewayRef.namespace moves from one cross-namespace Gateway to
// another: the policy left in the OLD Gateway's namespace must be deleted by
// the generation-path sweep (the finalizer stays; the new policy is still
// cross-namespace), and the new policy must appear in the new Gateway's
// namespace. Without the sweep the old policy is stranded: nothing
// owner-references it there.
func TestModelRouterAgentGateway_CrossNamespaceGatewayMovePrunes(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	oldGwNS := newTestNamespace(t, c)
	newGwNS := newTestNamespace(t, c)
	makeBackendISvcInNamespace(t, c, ns, "crossns-cuda")
	makeGateway(t, c, oldGwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})
	makeGateway(t, c, newGwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})

	mr := crossNamespaceRouterFixture(ns, "mover-router", oldGwNS, "crossns-cuda")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)
	_, policyName := agentGatewayPolicyIdentity(mr)
	getPolicyInNamespace(t, c, oldGwNS, policyName)

	// Move to a different cross-namespace Gateway.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	fresh.Spec.AgentGatewayRef.Namespace = newGwNS
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("update modelrouter: %v", err)
	}
	reconcileAgentGatewayRouter(t, r, fresh)

	assertNoPolicyInNamespace(t, c, oldGwNS, policyName)
	getPolicyInNamespace(t, c, newGwNS, policyName)

	still := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, still); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if !controllerutil.ContainsFinalizer(still, modelRouterAgentGatewayFinalizer) {
		t.Errorf("finalizer missing while the policy is still cross-namespace")
	}
}

// foreignExtractionPolicy builds an AgentgatewayPolicy the operator does not
// own, targeting the named Gateway in gwNS, with a PreRouting transformation
// (the shadowing shape) or without one (no conflict).
func foreignExtractionPolicy(name, gwNS, gatewayName string, withTransformation bool) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(agentGatewayPolicyGVK())
	p.SetName(name)
	p.SetNamespace(gwNS)
	traffic := map[string]interface{}{"phase": "PreRouting"}
	if withTransformation {
		traffic["transformation"] = map[string]interface{}{
			"request": map[string]interface{}{
				"set": []interface{}{map[string]interface{}{
					"name":  "X-Gateway-Base-Model-Name",
					"value": "json(request.body).model",
				}},
			},
		}
	}
	p.Object["spec"] = map[string]interface{}{
		"targetRefs": []interface{}{map[string]interface{}{
			metadataNameField: gatewayName,
			"kind":            "Gateway",
			"group":           "gateway.networking.k8s.io",
		}},
		"traffic": traffic,
	}
	return p
}

// selectorForeignPolicy builds a foreign AgentgatewayPolicy that attaches to
// Gateways via targetSelectors instead of targetRefs.
func selectorForeignPolicy(name, gwNS string, matchLabels map[string]string, withTransformation bool) *unstructured.Unstructured {
	p := foreignExtractionPolicy(name, gwNS, "unused", withTransformation)
	p.Object["spec"] = map[string]interface{}{
		"targetSelectors": []interface{}{map[string]interface{}{
			"kind":        "Gateway",
			"group":       "gateway.networking.k8s.io",
			"matchLabels": matchLabels,
		}},
		"traffic": map[string]interface{}{
			"phase": "PreRouting",
			"transformation": map[string]interface{}{
				"request": map[string]interface{}{
					"set": []interface{}{map[string]interface{}{
						"name":  "X-Gateway-Base-Model-Name",
						"value": "json(request.body).model",
					}},
				},
			},
		},
	}
	return p
}

// TestModelRouterAgentGateway_ForeignPreRoutingPolicyConflicts covers the
// silent-shadowing hazard: agentgateway merges same-specificity policies
// oldest-wins per whole field and never combines list entries, so a foreign
// PreRouting transformation targeting the same Gateway either silently
// disables the generated model extraction or is silently disabled by it, and
// no condition reports the drop. The reconciler must refuse with a named
// condition instead of reporting Ready. A foreign PreRouting policy WITHOUT a
// transformation (or targeting a different Gateway) does not touch the
// transformation field and must NOT be treated as a conflict.
func TestModelRouterAgentGateway_ForeignPreRoutingPolicyConflicts(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	gwNS := newTestNamespace(t, c)
	makeBackendISvcInNamespace(t, c, ns, "crossns-cuda")
	// The referenced Gateway: admits the router's namespace (All) and carries
	// labels so targetSelectors-based foreign policies have something real to
	// match against.
	makeGateway(t, c, gwNS, "ai-gateway", map[string]string{"team": "infra"}, []interface{}{gatewayListenerAllowingAll()})

	cases := []struct {
		name          string
		foreignPolicy *unstructured.Unstructured
		wantConflict  bool
	}{
		{
			name:          "foreign PreRouting transformation on the same Gateway conflicts",
			foreignPolicy: foreignExtractionPolicy("foreign-extraction", gwNS, "ai-gateway", true),
			wantConflict:  true,
		},
		{
			name:          "foreign PreRouting policy without a transformation does not conflict",
			foreignPolicy: foreignExtractionPolicy("foreign-extauth", gwNS, "ai-gateway", false),
			wantConflict:  false,
		},
		{
			name:          "foreign transformation targeting another Gateway does not conflict",
			foreignPolicy: foreignExtractionPolicy("foreign-elsewhere", gwNS, "other-gateway", true),
			wantConflict:  false,
		},
		{
			name:          "foreign transformation selector-matched to the Gateway conflicts",
			foreignPolicy: selectorForeignPolicy("foreign-selector", gwNS, map[string]string{"team": "infra"}, true),
			wantConflict:  true,
		},
		{
			name:          "foreign transformation selector-matched to other Gateways does not conflict",
			foreignPolicy: selectorForeignPolicy("foreign-selector-other", gwNS, map[string]string{"team": "other"}, true),
			wantConflict:  false,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routerName := fmt.Sprintf("conflict-router-%d", i+1)
			svc := fmt.Sprintf("conflict-svc-%d", i+1)
			makeBackendISvcInNamespace(t, c, ns, svc)
			if err := c.Create(context.Background(), tc.foreignPolicy); err != nil {
				t.Fatalf("create foreign policy: %v", err)
			}
			mr := crossNamespaceRouterFixture(ns, routerName, gwNS, svc)
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatalf("create modelrouter: %v", err)
			}
			r := newModelRouterAgentGatewayReconciler(t, cfg)
			res, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
			})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			fresh := &inferencev1alpha1.ModelRouter{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
				t.Fatalf("get modelrouter: %v", err)
			}
			cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
			_, policyName := agentGatewayPolicyIdentity(mr)
			if tc.wantConflict {
				if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonPolicyConflict {
					t.Fatalf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonPolicyConflict, cond)
				}
				if !strings.Contains(cond.Message, tc.foreignPolicy.GetName()) {
					t.Errorf("condition message %q does not name the conflicting policy", cond.Message)
				}
				// Conflicted routers recheck too, so the condition clears by
				// itself when the foreign policy is deleted (the round-2
				// review saw it stick until an unrelated poke).
				if res.RequeueAfter != agentGatewayConflictRequeueAfter {
					t.Errorf("requeue after = %v, want %v (the conflict recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
				}
				assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, svc)
				assertNoPolicyInNamespace(t, c, gwNS, policyName)
				assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, routerName)
				// Remove the foreign policy: the next reconcile must go Ready
				// and the condition must be gone.
				if err := c.Delete(context.Background(), tc.foreignPolicy); err != nil {
					t.Fatalf("delete foreign policy: %v", err)
				}
				reconcileAgentGatewayRouter(t, r, mr)
				cleared := &inferencev1alpha1.ModelRouter{}
				if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, cleared); err != nil {
					t.Fatalf("get modelrouter after foreign policy deleted: %v", err)
				}
				clearedCond := apimeta.FindStatusCondition(cleared.Status.Conditions, ModelRouterAgentGatewayConditionReady)
				if clearedCond == nil || clearedCond.Status != metav1.ConditionTrue {
					t.Errorf("AgentGatewayReady did not clear after the foreign policy was deleted, got %+v", clearedCond)
				}
				getPolicyInNamespace(t, c, gwNS, policyName)
				return
			}
			if cond == nil || cond.Status != metav1.ConditionTrue {
				t.Fatalf("expected AgentGatewayReady=True for a non-conflicting foreign policy, got %+v", cond)
			}
			getPolicyInNamespace(t, c, gwNS, policyName)
		})
	}
}

// TestModelRouterAgentGateway_PolicyNameCollisionFailsLoud covers the
// deterministic cross-namespace policy name: it is the sanitized concatenation
// of the router's namespace and name, which is not injective. Two routers
// whose concatenations collide must not adopt each other's policy (that would
// silently repoint the targetRef); the second router is refused with a named
// condition, not an apply error.
func TestModelRouterAgentGateway_PolicyNameCollisionFailsLoud(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	gwNS := newTestNamespace(t, c)
	makeNamespace(t, c, "a-b")
	makeNamespace(t, c, "a")
	makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})

	// ns "a-b" + name "c" and ns "a" + name "b-c" both sanitize to "a-b-c".
	routers := []struct{ ns, name, svc string }{
		{"a-b", "c", "svc1"},
		{"a", "b-c", "svc1"},
	}
	for _, r := range routers {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: r.svc, Namespace: r.ns},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: r.svc,
				Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
			},
		}
		if err := c.Create(context.Background(), isvc); err != nil {
			t.Fatalf("create isvc in %s: %v", r.ns, err)
		}
	}

	var first *inferencev1alpha1.ModelRouter
	for i, r := range routers {
		mr := &inferencev1alpha1.ModelRouter{
			ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: r.ns},
			Spec: inferencev1alpha1.ModelRouterSpec{
				DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
				AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
					Name: "ai-gateway", Namespace: gwNS, EndpointPicker: "epp", EndpointPickerPort: 9002,
				},
				Backends: []inferencev1alpha1.RouterBackend{
					{Name: r.svc, InferenceServiceRef: corev1LocalRef(r.svc)},
				},
				Rules: []inferencev1alpha1.RouterRule{
					{Name: "r", Route: inferencev1alpha1.RuleRoute{Backends: []string{r.svc}}},
				},
			},
		}
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter %d: %v", i, err)
		}
		if i == 0 {
			first = mr
		}
	}

	rec := newModelRouterAgentGatewayReconciler(t, cfg)
	_, err := rec.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: routers[0].name, Namespace: routers[0].ns},
	})
	if err != nil {
		t.Fatalf("first router reconcile: %v", err)
	}
	_, err = rec.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: routers[1].name, Namespace: routers[1].ns},
	})
	if err != nil {
		t.Fatalf("second router's reconcile must refuse, not error: %v", err)
	}

	// The policy still belongs to the FIRST router; the second must not have
	// adopted or overwritten it.
	policy := getPolicyInNamespace(t, c, gwNS, "a-b-c")
	firstFresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: first.Name, Namespace: first.Namespace}, firstFresh); err != nil {
		t.Fatalf("get first router: %v", err)
	}
	if got := policy.GetLabels()[agentGatewayRouterUIDLabel]; got != string(firstFresh.UID) {
		t.Errorf("policy uid label = %q, want the first router's uid %q", got, string(firstFresh.UID))
	}

	secondFresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: routers[1].name, Namespace: routers[1].ns}, secondFresh); err != nil {
		t.Fatalf("get second router: %v", err)
	}
	cond := apimeta.FindStatusCondition(secondFresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonResourceNameTaken {
		t.Errorf("expected AgentGatewayReady=False/%s on the colliding router, got %+v", modelRouterAgentGatewayReasonResourceNameTaken, cond)
	}
	if !strings.Contains(cond.Message, "already managed by another ModelRouter") {
		t.Errorf("condition message %q does not name the collision", cond.Message)
	}
	// The refusal fires before any generation: the second router must have
	// produced no pool and no route of its own.
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), "a", "svc1")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), "a", "b-c")
}

// TestModelRouterAgentGateway_ForeignGeneratedNameNotAdopted covers the
// never-adopt invariant for EVERY generated kind: a pre-existing object with
// a generated name (the AgentgatewayPolicy, an InferencePool named after the
// InferenceService, or the HTTPRoute named after the router) and NO
// router-UID label was silently overwritten (spec replaced, label stamped,
// owner reference added) and then deleted when the router went away, by the
// finalizer sweep cross-namespace, by the leaving sweep, and by owner-ref
// adoption plus GC in the same namespace. The reconciler must refuse with
// GeneratedResourceNameTaken and leave the foreign object byte-for-byte
// untouched.

// assertForeignObjectUntouched asserts a pre-existing object survived the
// reconcile without a single write (resourceVersion unchanged) and without
// adoption (no router-UID label, no owner references).
func assertForeignObjectUntouched(t *testing.T, c client.Client, before *unstructured.Unstructured) {
	t.Helper()
	var after unstructured.Unstructured
	after.SetGroupVersionKind(before.GroupVersionKind())
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: before.GetNamespace(), Name: before.GetName()}, &after); err != nil {
		t.Fatalf("get foreign object %s/%s: %v", before.GetNamespace(), before.GetName(), err)
	}
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Errorf("foreign object was written during reconcile (rv %q -> %q)",
			before.GetResourceVersion(), after.GetResourceVersion())
	}
	if got := after.GetLabels()[agentGatewayRouterUIDLabel]; got != "" {
		t.Errorf("foreign object grew the router-UID label %q; it was adopted", got)
	}
	if len(after.GetOwnerReferences()) != 0 {
		t.Errorf("foreign object grew owner references; it was adopted: %+v", after.GetOwnerReferences())
	}
}

// assertForeignPolicyMarkerUnchanged asserts the foreign policy's own
// transformation header survived; the generated policy would have replaced it
// with LLMKube's model header.
func assertForeignPolicyMarkerUnchanged(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	p := getPolicyInNamespace(t, c, ns, name)
	sets, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "transformation", "request", "set")
	if len(sets) == 0 {
		t.Errorf("foreign policy lost its transformation set: %+v", p.Object["spec"])
	} else if s0, _ := sets[0].(map[string]interface{}); s0["name"] != "X-Gateway-Base-Model-Name" {
		t.Errorf("foreign policy's transformation header is now %v; it was overwritten", s0["name"])
	}
}

func TestModelRouterAgentGateway_ForeignGeneratedNameNotAdopted_CrossNamespace(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	gwNS := newTestNamespace(t, c)
	makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})
	makeBackendISvcInNamespace(t, c, ns, "collide-cuda")

	mr := crossNamespaceRouterFixture(ns, "collide-router", gwNS, "collide-cuda")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	policyNS, policyName := agentGatewayPolicyIdentity(mr)
	foreign := foreignExtractionPolicy(policyName, gwNS, "ai-gateway", true)
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create foreign policy: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile must refuse, not error: %v", err)
	}
	if res.RequeueAfter != agentGatewayConflictRequeueAfter {
		t.Errorf("requeue after = %v, want %v (the name-taken recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonResourceNameTaken {
		t.Fatalf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonResourceNameTaken, cond)
	}
	if !strings.Contains(cond.Message, policyNS+"/"+policyName) {
		t.Errorf("condition message %q does not name the foreign object", cond.Message)
	}
	assertForeignObjectUntouched(t, c, foreign)
	assertForeignPolicyMarkerUnchanged(t, c, gwNS, policyName)
	// Nothing else may be generated for a refused router.
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "collide-cuda")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, mr.Name)
	if controllerutil.ContainsFinalizer(fresh, modelRouterAgentGatewayFinalizer) {
		t.Errorf("finalizer present on a refused router; nothing was generated")
	}

	// Deleting the router must not delete the owner's policy.
	if err := c.Delete(context.Background(), mr); err != nil {
		t.Fatalf("delete modelrouter: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
	}); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	assertForeignObjectUntouched(t, c, foreign)
	assertForeignPolicyMarkerUnchanged(t, c, gwNS, policyName)
}

func TestModelRouterAgentGateway_ForeignGeneratedNameNotAdopted_SameNamespace(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	// Same-namespace Gateway: the default from=Same admits the router.
	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "collide2-cuda")

	mr := sameNamespaceRouterFixture(ns, "collide-router2", "collide2-cuda")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	_, name := agentGatewayPolicyIdentity(mr)
	foreign := foreignExtractionPolicy(name, ns, "ai-gateway", true)
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create foreign policy: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile must refuse, not error: %v", err)
	}
	if res.RequeueAfter != agentGatewayConflictRequeueAfter {
		t.Errorf("requeue after = %v, want %v (the name-taken recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonResourceNameTaken {
		t.Fatalf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonResourceNameTaken, cond)
	}
	assertForeignObjectUntouched(t, c, foreign)
	assertForeignPolicyMarkerUnchanged(t, c, ns, name)
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "collide2-cuda")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, mr.Name)

	// The mutator guard backs the pre-check against a create race; it must
	// refuse an unlabeled object in the router's own namespace too.
	if err := r.applyAgentGatewayResource(context.Background(), mr, newAgentGatewayModelExtractionPolicy(mr, mr.Spec.AgentGatewayRef)); err == nil {
		t.Fatal("applyAgentGatewayResource must refuse to adopt the unlabeled policy")
	}
	assertForeignObjectUntouched(t, c, foreign)

	if err := c.Delete(context.Background(), mr); err != nil {
		t.Fatalf("delete modelrouter: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	assertForeignObjectUntouched(t, c, foreign)
}

func TestModelRouterAgentGateway_ForeignGeneratedNameNotAdopted_UnownedInferencePool(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "collide3-cuda")

	mr := sameNamespaceRouterFixture(ns, "collide-router3", "collide3-cuda")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	poolName := sanitizeDNSName("collide3-cuda")
	handmade := &unstructured.Unstructured{}
	handmade.SetGroupVersionKind(inferencePoolGVK())
	handmade.SetName(poolName)
	handmade.SetNamespace(ns)
	handmade.Object["spec"] = map[string]interface{}{
		"selector":          map[string]interface{}{"matchLabels": map[string]interface{}{"app": "handmade"}},
		"targetPorts":       []interface{}{map[string]interface{}{"number": int64(9999)}},
		"endpointPickerRef": map[string]interface{}{"name": "handmade-epp", "port": map[string]interface{}{"number": int64(9001)}},
	}
	if err := c.Create(context.Background(), handmade); err != nil {
		t.Fatalf("create handmade pool: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile must refuse, not error: %v", err)
	}
	if res.RequeueAfter != agentGatewayConflictRequeueAfter {
		t.Errorf("requeue after = %v, want %v (the name-taken recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonResourceNameTaken {
		t.Fatalf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonResourceNameTaken, cond)
	}
	if !strings.Contains(cond.Message, "InferencePool "+ns+"/"+poolName) {
		t.Errorf("condition message %q does not name the foreign pool", cond.Message)
	}
	// The refusal fires before ANY write: the pool is untouched and the
	// policy and route were never created.
	assertForeignObjectUntouched(t, c, handmade)
	after := getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, poolName)
	if ports, _, _ := unstructured.NestedSlice(after.Object, "spec", "targetPorts"); len(ports) != 1 || fmt.Sprint(ports[0].(map[string]interface{})["number"]) != "9999" {
		t.Errorf("handmade pool's targetPorts were overwritten: %v", ports)
	}
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, mr.Name)
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, mr.Name)

	// A switch to Proxy must not sweep the unowned pool, and deleting the
	// router must not delete it: only labeled objects are ours.
	fresh = &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	fresh.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneProxy
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("switch dataPlane: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile after switch: %v", err)
	}
	getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, poolName)
	if err := c.Delete(context.Background(), mr); err != nil {
		t.Fatalf("delete modelrouter: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, poolName)
}

func TestModelRouterAgentGateway_ForeignGeneratedNameNotAdopted_UnownedHTTPRoute(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "collide4-cuda")

	mr := sameNamespaceRouterFixture(ns, "collide-router4", "collide4-cuda")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	handmade := &unstructured.Unstructured{}
	handmade.SetGroupVersionKind(gatewayHTTPRouteGVK())
	handmade.SetName(mr.Name)
	handmade.SetNamespace(ns)
	handmade.Object["spec"] = map[string]interface{}{
		"parentRefs": []interface{}{map[string]interface{}{"name": "ai-gateway"}},
	}
	if err := c.Create(context.Background(), handmade); err != nil {
		t.Fatalf("create handmade route: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile must refuse, not error: %v", err)
	}
	if res.RequeueAfter != agentGatewayConflictRequeueAfter {
		t.Errorf("requeue after = %v, want %v (the name-taken recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonResourceNameTaken {
		t.Fatalf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonResourceNameTaken, cond)
	}
	if !strings.Contains(cond.Message, "HTTPRoute "+ns+"/"+mr.Name) {
		t.Errorf("condition message %q does not name the foreign route", cond.Message)
	}
	// The pre-check runs before ANY apply, so the refusal left the pool and
	// the policy uncreated too.
	assertForeignObjectUntouched(t, c, handmade)
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, mr.Name)
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "collide4-cuda")
	if err := c.Delete(context.Background(), mr); err != nil {
		t.Fatalf("delete modelrouter: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, mr.Name)
}

// TestModelRouterAgentGateway_NameTakenRecovers pins the name-taken requeue:
// the condition message tells the user to delete or rename the foreign object,
// and after they do, the router must go Ready on its own - no unrelated event
// required. The two subtests cover both refusal sites: the unowned policy
// refused by the guard-chain pre-check, and the unowned pool refused inside
// resource generation.
func TestModelRouterAgentGateway_NameTakenRecovers(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()

	ns := newTestNamespace(t, c)
	gwNS := newTestNamespace(t, c)

	t.Run("unowned policy", func(t *testing.T) {
		makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})
		makeBackendISvcInNamespace(t, c, ns, "recov-cuda")

		mr := crossNamespaceRouterFixture(ns, "recov-router", gwNS, "recov-cuda")
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter: %v", err)
		}
		_, policyName := agentGatewayPolicyIdentity(mr)
		foreign := foreignExtractionPolicy(policyName, gwNS, "ai-gateway", true)
		if err := c.Create(context.Background(), foreign); err != nil {
			t.Fatalf("create foreign policy: %v", err)
		}

		r := newModelRouterAgentGatewayReconciler(t, cfg)
		res, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
		})
		if err != nil {
			t.Fatalf("reconcile must refuse, not error: %v", err)
		}
		if res.RequeueAfter != agentGatewayConflictRequeueAfter {
			t.Errorf("requeue after = %v, want %v (the name-taken recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
		}

		// The owner removes the foreign object: the requeued reconcile goes
		// Ready and generates the router's own policy.
		if err := c.Delete(context.Background(), foreign); err != nil {
			t.Fatalf("delete foreign policy: %v", err)
		}
		reconcileAgentGatewayRouter(t, r, mr)
		getPolicyInNamespace(t, c, gwNS, policyName)
	})

	t.Run("unowned pool", func(t *testing.T) {
		makeGateway(t, c, ns, "ai-gateway", nil, nil)
		makeBackendISvcInNamespace(t, c, ns, "recov2-cuda")

		mr := sameNamespaceRouterFixture(ns, "recov2-router", "recov2-cuda")
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter: %v", err)
		}
		poolName := sanitizeDNSName("recov2-cuda")
		handmade := &unstructured.Unstructured{}
		handmade.SetGroupVersionKind(inferencePoolGVK())
		handmade.SetName(poolName)
		handmade.SetNamespace(ns)
		handmade.Object["spec"] = map[string]interface{}{
			"selector":          map[string]interface{}{"matchLabels": map[string]interface{}{"app": "handmade"}},
			"targetPorts":       []interface{}{map[string]interface{}{"number": int64(9999)}},
			"endpointPickerRef": map[string]interface{}{"name": "handmade-epp", "port": map[string]interface{}{"number": int64(9001)}},
		}
		if err := c.Create(context.Background(), handmade); err != nil {
			t.Fatalf("create handmade pool: %v", err)
		}

		r := newModelRouterAgentGatewayReconciler(t, cfg)
		res, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
		})
		if err != nil {
			t.Fatalf("reconcile must refuse, not error: %v", err)
		}
		if res.RequeueAfter != agentGatewayConflictRequeueAfter {
			t.Errorf("requeue after = %v, want %v (the name-taken recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
		}

		// The owner deletes the hand-made pool: the requeued reconcile goes
		// Ready and generates the router's own pool.
		if err := c.Delete(context.Background(), handmade); err != nil {
			t.Fatalf("delete handmade pool: %v", err)
		}
		reconcileAgentGatewayRouter(t, r, mr)
		getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, poolName)
	})
}

// countingListClient counts List calls made through it, so a test can assert
// exactly how many the reconciler performs. Reconciles run sequentially in
// these tests, so no locking is needed.
type countingListClient struct {
	client.Client
	lists int
}

func (c *countingListClient) List(ctx context.Context, l client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Client.List(ctx, l, opts...)
}

// newCountingModelRouterAgentGatewayReconciler is
// newModelRouterAgentGatewayReconciler with a List-counting client.
func newCountingModelRouterAgentGatewayReconciler(t *testing.T, cfg *rest.Config) (*ModelRouterAgentGatewayReconciler, *countingListClient) {
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
	wrapped := &countingListClient{Client: c}
	return &ModelRouterAgentGatewayReconciler{Client: wrapped, Scheme: scheme.Scheme}, wrapped
}

// TestModelRouterAgentGateway_SweepOnlyForUsedRouters pins the evidence gate
// on the leave-AgentGateway sweep: the sweep runs three uncached Lists
// (cluster-wide policies, pools, routes), so a router that never used
// AgentGateway mode must make none, and a router that switches away must be
// swept exactly once - after the sweep clears the plane's status, later
// reconciles find no evidence and skip it.
func TestModelRouterAgentGateway_SweepOnlyForUsedRouters(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)

	t.Run("never-AgentGateway Proxy router makes zero Lists", func(t *testing.T) {
		makeBackendISvcInNamespace(t, c, ns, "sweep0-cuda")
		mr := &inferencev1alpha1.ModelRouter{
			ObjectMeta: metav1.ObjectMeta{Name: "sweep0-router", Namespace: ns},
			Spec: inferencev1alpha1.ModelRouterSpec{
				DataPlane: inferencev1alpha1.ModelRouterDataPlaneProxy,
				Backends: []inferencev1alpha1.RouterBackend{
					{Name: "sweep0-cuda", InferenceServiceRef: corev1LocalRef("sweep0-cuda")},
				},
				DefaultRoute: "sweep0-cuda",
			},
		}
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter: %v", err)
		}
		r, counter := newCountingModelRouterAgentGatewayReconciler(t, cfg)
		for i := 0; i < 2; i++ {
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
			}); err != nil {
				t.Fatalf("reconcile %d: %v", i+1, err)
			}
		}
		if counter.lists != 0 {
			t.Errorf("a never-AgentGateway Proxy router made %d List calls across 2 reconciles, want 0", counter.lists)
		}
	})

	t.Run("a router that switches away is swept exactly once", func(t *testing.T) {
		makeBackendISvcInNamespace(t, c, ns, "sweep1-cuda")
		mr := sameNamespaceRouterFixture(ns, "sweep1-router", "sweep1-cuda")
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter: %v", err)
		}
		r, counter := newCountingModelRouterAgentGatewayReconciler(t, cfg)
		reconcileAgentGatewayRouter(t, r, mr)
		getPolicyInNamespace(t, c, ns, mr.Name)
		before := counter.lists

		fresh := &inferencev1alpha1.ModelRouter{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
			t.Fatalf("get modelrouter: %v", err)
		}
		fresh.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneProxy
		if err := c.Update(context.Background(), fresh); err != nil {
			t.Fatalf("switch dataPlane: %v", err)
		}

		// The switch reconcile sweeps: one List each for policies
		// (cluster-wide), pools, and routes.
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
		}); err != nil {
			t.Fatalf("reconcile after switch: %v", err)
		}
		if got := counter.lists - before; got != 3 {
			t.Errorf("the switch reconcile made %d List calls, want exactly 3 (one per swept kind)", got)
		}
		assertAllGoneLike(t, c, ns, mr.Name, "sweep1-cuda")

		// The sweep cleared the plane's status, so later reconciles find no
		// evidence and make no Lists.
		for i := 0; i < 2; i++ {
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
			}); err != nil {
				t.Fatalf("reconcile %d after sweep: %v", i+1, err)
			}
		}
		if got := counter.lists - before - 3; got != 0 {
			t.Errorf("later reconciles made %d List calls after the sweep, want 0", got)
		}
	})
	t.Run("a router that generated and then lost its ref is swept exactly once", func(t *testing.T) {
		makeBackendISvcInNamespace(t, c, ns, "sweep2-cuda")
		mr := sameNamespaceRouterFixture(ns, "sweep2-router", "sweep2-cuda")
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter: %v", err)
		}
		r, counter := newCountingModelRouterAgentGatewayReconciler(t, cfg)
		reconcileAgentGatewayRouter(t, r, mr)
		getPolicyInNamespace(t, c, ns, mr.Name)
		before := counter.lists

		fresh := &inferencev1alpha1.ModelRouter{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
			t.Fatalf("get modelrouter: %v", err)
		}
		fresh.Spec.AgentGatewayRef = nil
		if err := c.Update(context.Background(), fresh); err != nil {
			t.Fatalf("remove agentGatewayRef: %v", err)
		}

		// The ref-removal reconcile sweeps: one List each for policies
		// (cluster-wide), pools, and routes.
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
		}); err != nil {
			t.Fatalf("reconcile after ref removal: %v", err)
		}
		if got := counter.lists - before; got != 3 {
			t.Errorf("the ref-removal reconcile made %d List calls, want exactly 3 (one per swept kind)", got)
		}
		assertAllGoneLike(t, c, ns, mr.Name, "sweep2-cuda")

		// The sweep cleared the evidence; later reconciles make no Lists and
		// the router settles on AgentGatewayRefMissing.
		for i := 0; i < 2; i++ {
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
			}); err != nil {
				t.Fatalf("reconcile %d after sweep: %v", i+1, err)
			}
		}
		if got := counter.lists - before - 3; got != 0 {
			t.Errorf("later reconciles made %d List calls after the sweep, want 0", got)
		}
		refless := &inferencev1alpha1.ModelRouter{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, refless); err != nil {
			t.Fatalf("get modelrouter: %v", err)
		}
		cond := apimeta.FindStatusCondition(refless.Status.Conditions, ModelRouterAgentGatewayConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonNoRef {
			t.Errorf("expected AgentGatewayReady=False/%s, got %+v", modelRouterAgentGatewayReasonNoRef, cond)
		}
	})
}

// TestModelRouterAgentGateway_RefUnsetRouterConverges pins the ref-unset
// convergence: a router in AgentGateway mode with no agentGatewayRef (admitted
// while the validating webhook was off or not serving) used to alternate
// forever - the refusal allocated status.agentGateway, the status event
// re-enqueued, the sweep ran three uncached Lists and cleared the status, the
// next reconcile found no evidence and refused again (312 reconciles in 20s
// under a real manager). After the fix the refusal never fabricates the
// sweep gate's evidence, so the router settles on AgentGatewayRefMissing with
// no writes and no Lists after the first reconcile.
func TestModelRouterAgentGateway_RefUnsetRouterConverges(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "refless-router", Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			// No agentGatewayRef: the webhook would reject this at admission,
			// but envtest has no webhook and real clusters admit it whenever
			// the webhook is disabled or failing open.
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: "refless-cuda", InferenceServiceRef: corev1LocalRef("refless-cuda")},
			},
		},
	}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r, counter := newCountingModelRouterAgentGatewayReconciler(t, cfg)
	lastRV := ""
	for i := 1; i <= 4; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
		}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		fresh := &inferencev1alpha1.ModelRouter{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
			t.Fatalf("get modelrouter after reconcile %d: %v", i, err)
		}
		cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonNoRef {
			t.Errorf("reconcile %d: expected AgentGatewayReady=False/%s, got %+v",
				i, modelRouterAgentGatewayReasonNoRef, cond)
		}
		if i >= 2 && fresh.ResourceVersion != lastRV {
			t.Errorf("reconcile %d: resourceVersion changed from %q to %q; the router is written on every reconcile",
				i, lastRV, fresh.ResourceVersion)
		}
		lastRV = fresh.ResourceVersion
		if fresh.Status.AgentGateway != nil {
			t.Errorf("reconcile %d: status.agentGateway is %+v; a refusal must not fabricate the sweep gate's evidence",
				i, fresh.Status.AgentGateway)
		}
	}
	if counter.lists != 0 {
		t.Errorf("the ref-unset router made %d List calls across 4 reconciles, want 0", counter.lists)
	}
}

func assertAllGoneLike(t *testing.T, c client.Client, ns, name, svc string) {
	t.Helper()
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, name)
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, svc)
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, name)
}

// TestModelRouterAgentGateway_ProxyModeProducesNothing covers the no-op path: a
// dataPlane: Proxy ModelRouter generates no agentgateway resources (the
// agentgateway reconciler no-ops; the proxy path is owned by
// ModelRouterReconciler and is unaffected).
func TestModelRouterAgentGateway_ProxyModeProducesNothing(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeBackendISvcInNamespace(t, c, ns, "proxy-cuda")

	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-router", Namespace: ns},
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

	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "proxy-cuda")
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, "proxy-router")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "proxy-router")

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "proxy-router", Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if fresh.Status.AgentGateway != nil {
		t.Errorf("expected nil status.agentGateway in Proxy mode, got %+v", fresh.Status.AgentGateway)
	}
}

// TestModelRouterAgentGateway_LeavingAgentGatewayModeCleansUp covers the
// round-3 MEDIUM bug: the cleanup sweep ran only when the finalizer was
// present, and the finalizer is only ever added for cross-namespace policies,
// so a same-namespace router that switched dataPlane to Proxy or dropped
// agentGatewayRef kept its Gateway-wide body-extraction policy, its pools, and
// its route until the router itself was deleted - the route kept serving
// traffic through the Gateway the whole time (reproduced live). The sweep must
// run with or without the finalizer, and take the pools and route with it.
func TestModelRouterAgentGateway_LeavingAgentGatewayModeCleansUp(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	// Same-namespace Gateway: the default from=Same admits the router, and a
	// same-namespace router never gets the finalizer.
	makeGateway(t, c, ns, "ai-gateway", nil, nil)

	startRouter := func(t *testing.T, name, svc string) *inferencev1alpha1.ModelRouter {
		t.Helper()
		makeBackendISvcInNamespace(t, c, ns, svc)
		mr := &inferencev1alpha1.ModelRouter{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: inferencev1alpha1.ModelRouterSpec{
				DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
				AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
					Name: "ai-gateway", EndpointPicker: "epp", EndpointPickerPort: 9002,
				},
				Backends: []inferencev1alpha1.RouterBackend{
					{Name: svc, InferenceServiceRef: corev1LocalRef(svc)},
				},
				Rules: []inferencev1alpha1.RouterRule{
					{Name: "r", Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen35-27b"}}, Route: inferencev1alpha1.RuleRoute{Backends: []string{svc}}},
				},
			},
		}
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter: %v", err)
		}
		r := newModelRouterAgentGatewayReconciler(t, cfg)
		reconcileAgentGatewayRouter(t, r, mr)
		// The resources exist and the router is Ready before the mode change.
		getPolicyInNamespace(t, c, ns, name)
		getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, svc)
		getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, name)
		return mr
	}

	assertAllGone := func(t *testing.T, name, svc string) {
		t.Helper()
		assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, name)
		assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, svc)
		assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, name)
	}

	t.Run("switching dataPlane to Proxy", func(t *testing.T) {
		mr := startRouter(t, "leave-router", "leave-cuda")

		fresh := &inferencev1alpha1.ModelRouter{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
			t.Fatalf("get modelrouter: %v", err)
		}
		fresh.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneProxy
		if err := c.Update(context.Background(), fresh); err != nil {
			t.Fatalf("switch dataPlane: %v", err)
		}

		r := newModelRouterAgentGatewayReconciler(t, cfg)
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertAllGone(t, mr.Name, "leave-cuda")
	})

	t.Run("removing agentGatewayRef", func(t *testing.T) {
		mr := startRouter(t, "leave2-router", "leave2-cuda")

		fresh := &inferencev1alpha1.ModelRouter{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
			t.Fatalf("get modelrouter: %v", err)
		}
		fresh.Spec.AgentGatewayRef = nil
		if err := c.Update(context.Background(), fresh); err != nil {
			t.Fatalf("remove agentGatewayRef: %v", err)
		}

		r := newModelRouterAgentGatewayReconciler(t, cfg)
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertAllGone(t, mr.Name, "leave2-cuda")

		refless := &inferencev1alpha1.ModelRouter{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, refless); err != nil {
			t.Fatalf("get modelrouter: %v", err)
		}
		// The router had artifacts, so the sweep cleared status.agentGateway;
		// the ref-unset refusal then reports AgentGatewayRefMissing (and
		// converges: the refusal does not re-allocate the evidence).
		if cond := apimeta.FindStatusCondition(refless.Status.Conditions, ModelRouterAgentGatewayConditionReady); cond == nil ||
			cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonNoRef {
			t.Errorf("expected AgentGatewayReady=False/%s after leaving the mode via ref removal, got %+v",
				modelRouterAgentGatewayReasonNoRef, cond)
		}
		if refless.Status.AgentGateway != nil {
			t.Errorf("expected status.agentGateway to be cleared after leaving the mode, got %+v", refless.Status.AgentGateway)
		}
	})
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
	ns := newTestNamespace(t, c)

	makeBackendISvcInNamespace(t, c, ns, "local-cuda")
	makeGateway(t, c, ns, "ai-gateway", nil, nil)

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
				ObjectMeta: metav1.ObjectMeta{Name: tc.slug, Namespace: ns},
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
			assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "local-cuda")
			assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, tc.slug)
			assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, tc.slug)

			fresh := &inferencev1alpha1.ModelRouter{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
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
