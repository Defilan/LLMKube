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

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestAgentGatewayModelProvider(t *testing.T) {
	chatVLLM := []string{"Completions", "Messages", "AnthropicTokenCount", "Responses"}
	tests := []struct {
		name         string
		spec         inferencev1alpha1.InferenceServiceSpec
		wantProvider string
		wantFormats  []string
		wantOK       bool
	}{
		{"unset runtime is llama.cpp chat", inferencev1alpha1.InferenceServiceSpec{}, "llamacpp", []string{"Completions"}, true},
		{"llamacpp chat", inferencev1alpha1.InferenceServiceSpec{Runtime: "llamacpp"}, "llamacpp", []string{"Completions"}, true},
		{"llamacpp embedding mode", inferencev1alpha1.InferenceServiceSpec{Mode: "embedding"}, "llamacpp", []string{"Embeddings"}, true},
		{"llamacpp rerank inferred from --reranking", inferencev1alpha1.InferenceServiceSpec{
			ExtraArgs: []string{"--reranking", "--embedding"},
		}, "llamacpp", []string{"Rerank"}, true},
		{"llamacpp embedding inferred from endpoint path", inferencev1alpha1.InferenceServiceSpec{
			Endpoint: &inferencev1alpha1.EndpointSpec{Path: "/v1/embeddings"},
		}, "llamacpp", []string{"Embeddings"}, true},
		{"vllm chat", inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeVLLM}, "vllm", chatVLLM, true},
		{"vllm embedding", inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeVLLM, Mode: "embedding"}, "vllm", []string{"Embeddings"}, true},
		{"vllm rerank", inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeVLLM, Mode: "rerank"}, "vllm", []string{"Rerank"}, true},
		{"sglang is unmapped", inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeSGLANG}, "", nil, false},
		{"tgi is unmapped", inferencev1alpha1.InferenceServiceSpec{Runtime: "tgi"}, "", nil, false},
		{"llamacpp-router is unmapped", inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeLlamaCppRouter}, "", nil, false},
		{"generic is unmapped", inferencev1alpha1.InferenceServiceSpec{Runtime: "generic"}, "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, formats, ok := agentGatewayModelProvider(&inferencev1alpha1.InferenceService{Spec: tt.spec})
			if ok != tt.wantOK || provider != tt.wantProvider || !reflect.DeepEqual(formats, tt.wantFormats) {
				t.Errorf("agentGatewayModelProvider = (%q, %v, %v), want (%q, %v, %v)",
					provider, formats, ok, tt.wantProvider, tt.wantFormats, tt.wantOK)
			}
		})
	}
}

func TestVLLMServesModel(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"no flag", nil, false},
		{"inline", []string{"--served-model-name=chat"}, true},
		{"inline, other name", []string{"--served-model-name=other"}, false},
		{"one of several names", []string{"--served-model-name", "other", "chat", "--port", "8000"}, true},
		{"name after the next flag", []string{"--served-model-name", "other", "--port", "chat"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{ExtraArgs: tt.args}}
			if got := vllmServesModel(isvc, "chat"); got != tt.want {
				t.Errorf("vllmServesModel(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}

	// spec.args reaches vLLM only with a command override (setCommandAndArgs).
	flag := []string{"--served-model-name=chat"}
	for _, tt := range []struct {
		name    string
		command []string
		want    bool
	}{
		{"args without command", nil, false},
		{"args with command", []string{"vllm", "serve"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{Command: tt.command, Args: flag}}
			if got := vllmServesModel(isvc, "chat"); got != tt.want {
				t.Errorf("vllmServesModel(command %v, args %v) = %v, want %v", tt.command, flag, got, tt.want)
			}
		})
	}
}

func TestAgentGatewayModelName(t *testing.T) {
	router := func(name string) *inferencev1alpha1.ModelRouter {
		return &inferencev1alpha1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	long := strings.Repeat("r", 60)

	tests := []struct {
		name, router, backend, want string
	}{
		{"short names join", "llm-router", "chat-llama", "llm-router-chat-llama"},
		{"dots in the router name are sanitized", "ai.models", "chat-llama", "ai-models-chat-llama"},
		{"a long pair is cut and hashed", long, "chat-llama", ""},
		{"a trailing dash is hashed away", "llm-router", "chat-", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agentGatewayModelName(router(tt.router), tt.backend)
			if errs := validation.IsDNS1123Label(got); len(errs) > 0 {
				t.Fatalf("agentGatewayModelName(%q, %q) = %q, not a DNS-1123 label: %v", tt.router, tt.backend, got, errs)
			}
			if tt.want != "" && got != tt.want {
				t.Errorf("agentGatewayModelName(%q, %q) = %q, want %q", tt.router, tt.backend, got, tt.want)
			}
			if again := agentGatewayModelName(router(tt.router), tt.backend); again != got {
				t.Errorf("name is not deterministic: %q then %q", got, again)
			}
		})
	}

	// Two routers in one namespace serving the same backend name get two
	// objects; so do two long pairs that only differ past the cut.
	if a, b := agentGatewayModelName(router("team-a"), "chat-llama"), agentGatewayModelName(router("team-b"), "chat-llama"); a == b {
		t.Errorf("two routers derived the same model name %q", a)
	}
	if a, b := agentGatewayModelName(router(long), "backend-one"), agentGatewayModelName(router(long), "backend-two"); a == b {
		t.Errorf("two long pairs that differ past the cut derived the same name %q", a)
	}
	// "chat" and "chat-" are distinct backend names and must stay distinct.
	if a, b := agentGatewayModelName(router("llm-router"), "chat"), agentGatewayModelName(router("llm-router"), "chat-"); a == b {
		t.Errorf("backends chat and chat- derived the same name %q", a)
	}
}

// modelRoutingRouter attaches to ai-gateway's http listener, one backend per
// InferenceService.
func modelRoutingRouter(ns, name string, isvcs ...string) *inferencev1alpha1.ModelRouter {
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
			AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
				Name:        "ai-gateway",
				Routing:     inferencev1alpha1.AgentGatewayRoutingModel,
				SectionName: "http",
			},
		},
	}
	for _, isvc := range isvcs {
		mr.Spec.Backends = append(mr.Spec.Backends,
			inferencev1alpha1.RouterBackend{Name: isvc, InferenceServiceRef: corev1LocalRef(isvc)})
	}
	return mr
}

func TestAgentGatewayModelRoutingMessage(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*inferencev1alpha1.ModelRouter)
		want   string
	}{
		{"clean", func(*inferencev1alpha1.ModelRouter) {}, ""},
		{"rules", func(mr *inferencev1alpha1.ModelRouter) {
			mr.Spec.Rules = []inferencev1alpha1.RouterRule{{
				Name:  "by-name",
				Match: &inferencev1alpha1.RuleMatch{Models: []string{"a"}},
				Route: inferencev1alpha1.RuleRoute{Backends: []string{"a"}},
			}}
		}, `rule "by-name"`},
		{"defaultRoute", func(mr *inferencev1alpha1.ModelRouter) { mr.Spec.DefaultRoute = "a" }, "spec.defaultRoute"},
		{"external backend", func(mr *inferencev1alpha1.ModelRouter) {
			mr.Spec.Backends = append(mr.Spec.Backends, inferencev1alpha1.RouterBackend{
				Name: "cloud", External: &inferencev1alpha1.ExternalProvider{Provider: "openai", Model: "gpt-5"},
			})
		}, `backend "cloud" is external`},
		{"resolution endpoint", func(mr *inferencev1alpha1.ModelRouter) {
			mr.Spec.Backends[0].Resolution = inferencev1alpha1.RouterBackendResolutionEndpoint
		}, "resolution: endpoint"},
		{"backend endpointPicker", func(mr *inferencev1alpha1.ModelRouter) {
			mr.Spec.Backends[0].EndpointPicker = &inferencev1alpha1.RouterEndpointPicker{
				Service: &inferencev1alpha1.EndpointPickerService{Name: "epp", Port: 9002},
			}
		}, "sets endpointPicker"},
		{"wildcard served name", func(mr *inferencev1alpha1.ModelRouter) { mr.Spec.Backends[0].DisplayName = "chat-*" }, "wildcard"},
		{"served name over match.model's limit", func(mr *inferencev1alpha1.ModelRouter) {
			mr.Spec.Backends[0].DisplayName = strings.Repeat("m", agentGatewayModelMatchMaxLength+1)
		}, "at most 1024"},
		{"duplicate served name", func(mr *inferencev1alpha1.ModelRouter) { mr.Spec.Backends[1].DisplayName = "a" }, `both serve model "a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := modelRoutingRouter("ns", "router", "a", "b")
			tt.mutate(mr)
			got := agentGatewayModelRoutingMessage(mr)
			if tt.want == "" && got != "" {
				t.Fatalf("clean router refused: %s", got)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("agentGatewayModelRoutingMessage = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestModelRouterValidator_AgentGatewayModelRouting(t *testing.T) {
	v := &ModelRouterValidator{}

	if _, err := v.ValidateCreate(context.Background(), modelRoutingRouter("ns", "router", "a", "b")); err != nil {
		t.Errorf("a routing: Model router without pickers must be admitted: %v", err)
	}

	pool := modelRoutingRouter("ns", "router", "a", "b")
	pool.Spec.AgentGatewayRef.Routing = inferencev1alpha1.AgentGatewayRoutingInferencePool
	pool.Spec.AgentGatewayRef.SectionName = ""
	if _, err := v.ValidateCreate(context.Background(), pool); err == nil || !strings.Contains(err.Error(), "Endpoint Picker") {
		t.Errorf("routing: InferencePool without a picker: got %v, want the EndpointPickerRequired message", err)
	}

	withRule := modelRoutingRouter("ns", "router", "a")
	withRule.Spec.Rules = []inferencev1alpha1.RouterRule{{
		Name:  "r",
		Match: &inferencev1alpha1.RuleMatch{Models: []string{"a"}},
		Route: inferencev1alpha1.RuleRoute{Backends: []string{"a"}},
	}}
	if _, err := v.ValidateCreate(context.Background(), withRule); err == nil || !strings.Contains(err.Error(), "routing: Model") {
		t.Errorf("routing: Model with rules: got %v, want the routing: Model refusal", err)
	}
}

// An empty want means the router must be admitted.
func TestModelRouterAgentGateway_ModelRoutingCEL(t *testing.T) {
	c, _, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	tests := []struct {
		name, want string
		mutate     func(*inferencev1alpha1.AgentGatewayReference)
	}{
		{"sectionName", "", func(*inferencev1alpha1.AgentGatewayReference) {}},
		{"hostnames only", "", func(ref *inferencev1alpha1.AgentGatewayReference) {
			ref.SectionName = ""
			ref.Hostnames = []string{"llm.example.com"}
		}},
		{"no listener and no hostnames", "requires sectionName or hostnames", func(ref *inferencev1alpha1.AgentGatewayReference) {
			ref.SectionName = ""
		}},
		{"router-wide picker", "apply only to routing: InferencePool", func(ref *inferencev1alpha1.AgentGatewayReference) {
			ref.EndpointPicker, ref.EndpointPickerPort = "epp", 9002
		}},
		{"sectionName on InferencePool", "apply only to routing: Model", func(ref *inferencev1alpha1.AgentGatewayReference) {
			ref.Routing = inferencev1alpha1.AgentGatewayRoutingInferencePool
		}},
		{"hostnames on InferencePool", "apply only to routing: Model", func(ref *inferencev1alpha1.AgentGatewayReference) {
			ref.Routing, ref.SectionName = inferencev1alpha1.AgentGatewayRoutingInferencePool, ""
			ref.Hostnames = []string{"llm.example.com"}
		}},
		{"sectionName that is not a Gateway API section name", "spec.agentGatewayRef.sectionName", func(ref *inferencev1alpha1.AgentGatewayReference) {
			ref.SectionName = "HTTP"
		}},
		{"hostname that is not a Gateway API hostname", "spec.agentGatewayRef.hostnames[0]", func(ref *inferencev1alpha1.AgentGatewayReference) {
			ref.Hostnames = []string{"LLM.example.com"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := modelRoutingRouter(ns, "", "a")
			mr.GenerateName = "cel-"
			tt.mutate(mr.Spec.AgentGatewayRef)
			err := c.Create(context.Background(), mr)
			if tt.want == "" {
				if err != nil {
					t.Errorf("create must be admitted: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("create: got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// A router written before routing existed compiles exactly as before.
func TestModelRouterAgentGateway_InferencePoolStaysDefault(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "legacy-cuda")
	mr := sameNamespaceRouterFixture(ns, "legacy", "legacy-cuda")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("a router without routing must be admitted: %v", err)
	}
	if got := mr.Spec.AgentGatewayRef.Routing; got != inferencev1alpha1.AgentGatewayRoutingInferencePool {
		t.Errorf("routing defaulted to %q, want InferencePool", got)
	}
	reconcileAgentGatewayRouter(t, newModelRouterAgentGatewayReconciler(t, cfg), mr)

	getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, "legacy-cuda")
	getPolicyInNamespace(t, c, ns, "legacy")
	route := getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "legacy")
	parentRefs, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	wantParents := []interface{}{map[string]interface{}{
		"name": "ai-gateway", "kind": "Gateway", "group": "gateway.networking.k8s.io",
	}}
	if !reflect.DeepEqual(parentRefs, wantParents) {
		t.Errorf("route parentRefs = %v, want %v", parentRefs, wantParents)
	}
	if _, has, _ := unstructured.NestedSlice(route.Object, "spec", "hostnames"); has {
		t.Errorf("route grew hostnames: %v", route.Object["spec"])
	}
	if models := listGeneratedNames(t, c, mr, agentGatewayModelGVK()); len(models) != 0 {
		t.Errorf("routing: InferencePool generated AgentgatewayModels %v", models)
	}
}

func makeModelISvc(t *testing.T, c client.Client, ns, name, rt, mode string, port int32, extraArgs ...string) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:  name,
			Runtime:   rt,
			Mode:      mode,
			Endpoint:  &inferencev1alpha1.EndpointSpec{Port: port},
			ExtraArgs: extraArgs,
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create isvc %s: %v", name, err)
	}
	setISvcReadyReplicasInNamespace(t, c, ns, name, 1)
}

func agentGatewayReadyCondition(t *testing.T, c client.Client, mr *inferencev1alpha1.ModelRouter) (*metav1.Condition, *inferencev1alpha1.ModelRouter) {
	t.Helper()
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	return apimeta.FindStatusCondition(fresh.Status.Conditions, ModelRouterAgentGatewayConditionReady), fresh
}

func listGeneratedNames(t *testing.T, c client.Client, mr *inferencev1alpha1.ModelRouter, gvk schema.GroupVersionKind) []string {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)
	if err := c.List(context.Background(), list, client.InNamespace(mr.Namespace),
		client.MatchingLabels{agentGatewayRouterUIDLabel: string(mr.UID)}); err != nil {
		t.Fatalf("list %s: %v", gvk.Kind, err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names
}

// One model per runtime and format, validated by the upstream CRD.
func TestModelRouterAgentGateway_ModelRoutingCompiles(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeModelISvc(t, c, ns, "chat-vllm", RuntimeVLLM, "", 8000, "--served-model-name=chat-large")
	makeBackendISvcInNamespace(t, c, ns, "chat-llama")
	makeModelISvc(t, c, ns, "reranker", "llamacpp", "rerank", 8080)
	makeModelISvc(t, c, ns, "embedder", "", "embedding", 8080)

	mr := modelRoutingRouter(ns, "llm-router", "chat-vllm", "chat-llama", "reranker", "embedder")
	mr.Spec.Backends[0].DisplayName = "chat-large"
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)

	cond, fresh := agentGatewayReadyCondition(t, c, mr)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "compiled 4 model(s)") {
		t.Fatalf("expected AgentGatewayReady=True compiling 4 models, got %+v", cond)
	}
	if fresh.Status.AgentGateway == nil || fresh.Status.AgentGateway.PoolReady {
		t.Errorf("status.agentGateway = %+v, want set with poolReady false (no pool in routing: Model)", fresh.Status.AgentGateway)
	}

	route := getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "llm-router")
	assertOwnedByRouter(t, route, fresh)
	wantRoute := map[string]interface{}{
		"parentRefs": []interface{}{map[string]interface{}{
			"name": "ai-gateway", "kind": "Gateway", "group": "gateway.networking.k8s.io", "sectionName": "http",
		}},
		"rules": []interface{}{map[string]interface{}{
			"name": "models",
			"backendRefs": []interface{}{map[string]interface{}{
				"name": "*", "kind": "AgentgatewayModel", "group": "agentgateway.dev",
			}},
		}},
	}
	if !reflect.DeepEqual(route.Object["spec"], wantRoute) {
		t.Errorf("route spec = %#v\nwant %#v", route.Object["spec"], wantRoute)
	}

	model := func(served, service string, port int64, provider string, formats ...string) map[string]interface{} {
		fs := make([]interface{}, 0, len(formats))
		for _, f := range formats {
			fs = append(fs, map[string]interface{}{"type": f})
		}
		return map[string]interface{}{
			"parentRefs": []interface{}{map[string]interface{}{
				"name": "llm-router", "kind": "HTTPRoute", "group": "gateway.networking.k8s.io", "sectionName": "models",
			}},
			"match":    map[string]interface{}{"model": served},
			"provider": "Custom",
			"custom": map[string]interface{}{
				"backendRef":       map[string]interface{}{"name": service, "port": port},
				"providerOverride": provider,
				"formats":          fs,
			},
		}
	}
	want := map[string]map[string]interface{}{
		"llm-router-chat-vllm": model("chat-large", "chat-vllm", 8000, "vllm",
			"Completions", "Messages", "AnthropicTokenCount", "Responses"),
		"llm-router-chat-llama": model("chat-llama", "chat-llama", 8080, "llamacpp", "Completions"),
		"llm-router-reranker":   model("reranker", "reranker", 8080, "llamacpp", "Rerank"),
		"llm-router-embedder":   model("embedder", "embedder", 8080, "llamacpp", "Embeddings"),
	}
	for name, spec := range want {
		got := getUnstructuredInNamespace(t, c, agentGatewayModelGVK(), ns, name)
		assertOwnedByRouter(t, got, fresh)
		if label := got.GetLabels()[agentGatewayRouterUIDLabel]; label != string(fresh.UID) {
			t.Errorf("%s uid label = %q, want %q", name, label, fresh.UID)
		}
		if !reflect.DeepEqual(got.Object["spec"], spec) {
			t.Errorf("%s spec = %#v\nwant %#v", name, got.Object["spec"], spec)
		}
	}

	if pools := listGeneratedNames(t, c, fresh, inferencePoolGVK()); len(pools) != 0 {
		t.Errorf("routing: Model generated InferencePools %v", pools)
	}
	assertNotExistsInNamespace(t, c, agentGatewayPolicyGVK(), ns, "llm-router")
}

// Served names that are not object names, from two routers in one namespace.
func TestModelRouterAgentGateway_ModelRoutingServedNames(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "chat-llama")
	makeModelISvc(t, c, ns, "org-model", RuntimeVLLM, "", 8000, "--served-model-name", "org/model")

	r := newModelRouterAgentGatewayReconciler(t, cfg)
	for _, name := range []string{"team-a", "team-b"} {
		mr := modelRoutingRouter(ns, name, "chat-llama", "org-model")
		mr.Spec.Backends[0].DisplayName = "Qwen3.5-2B"
		mr.Spec.Backends[1].DisplayName = "org/model"
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter %s: %v", name, err)
		}
		reconcileAgentGatewayRouter(t, r, mr)
		if cond, _ := agentGatewayReadyCondition(t, c, mr); cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("router %s: expected AgentGatewayReady=True, got %+v", name, cond)
		}

		for backend, served := range map[string]string{"chat-llama": "Qwen3.5-2B", "org-model": "org/model"} {
			m := getUnstructuredInNamespace(t, c, agentGatewayModelGVK(), ns, name+"-"+backend)
			if got, _, _ := unstructured.NestedString(m.Object, "spec", "match", "model"); got != served {
				t.Errorf("%s match.model = %q, want %q", m.GetName(), got, served)
			}
		}
	}
}

func TestModelRouterAgentGateway_ModelRoutingHostnames(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "chat-llama")
	mr := modelRoutingRouter(ns, "by-host", "chat-llama")
	mr.Spec.AgentGatewayRef.SectionName = ""
	mr.Spec.AgentGatewayRef.Hostnames = []string{"llm.example.com", "*.llm.example.com"}
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	reconcileAgentGatewayRouter(t, newModelRouterAgentGatewayReconciler(t, cfg), mr)

	route := getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "by-host")
	hostnames, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	if want := []string{"llm.example.com", "*.llm.example.com"}; !reflect.DeepEqual(hostnames, want) {
		t.Errorf("route hostnames = %v, want %v", hostnames, want)
	}
	parentRefs, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if len(parentRefs) != 1 || parentRefs[0].(map[string]interface{})["sectionName"] != nil {
		t.Errorf("route parentRefs = %v, want one parentRef without sectionName", parentRefs)
	}
}

func TestModelRouterAgentGateway_ModelRoutingPrunesRemovedBackend(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "keep")
	makeBackendISvcInNamespace(t, c, ns, "drop")

	mr := modelRoutingRouter(ns, "prune", "keep", "drop")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)
	getUnstructuredInNamespace(t, c, agentGatewayModelGVK(), ns, "prune-drop")

	_, fresh := agentGatewayReadyCondition(t, c, mr)
	fresh.Spec.Backends = fresh.Spec.Backends[:1]
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("remove backend: %v", err)
	}
	reconcileAgentGatewayRouter(t, r, fresh)

	if got := listGeneratedNames(t, c, fresh, agentGatewayModelGVK()); !reflect.DeepEqual(got, []string{"prune-keep"}) {
		t.Errorf("models after removing backend drop = %v, want [prune-keep]", got)
	}
}

// InferencePool -> Model -> InferencePool -> Model -> Proxy, with a
// cross-namespace Gateway so the extraction policy carries a finalizer.
func TestModelRouterAgentGateway_ModelRoutingSwitchesCleanUp(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)
	gwNS := newTestNamespace(t, c)
	ctx := context.Background()

	makeGateway(t, c, gwNS, "ai-gateway", nil, []interface{}{gatewayListenerAllowingAll()})
	makeBackendISvcInNamespace(t, c, ns, "switch-cuda")

	mr := crossNamespaceRouterFixture(ns, "switcher", gwNS, "switch-cuda")
	if err := c.Create(ctx, mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	reconcileAgentGatewayRouter(t, r, mr)
	policyName := ns + "-switcher"
	getPolicyInNamespace(t, c, gwNS, policyName)
	getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, "switch-cuda")

	update := func(mutate func(*inferencev1alpha1.ModelRouter)) *inferencev1alpha1.ModelRouter {
		t.Helper()
		_, fresh := agentGatewayReadyCondition(t, c, mr)
		mutate(fresh)
		if err := c.Update(ctx, fresh); err != nil {
			t.Fatalf("update modelrouter: %v", err)
		}
		reconcileAgentGatewayRouter(t, r, fresh)
		return fresh
	}

	// InferencePool -> Model.
	toModel := func(mr *inferencev1alpha1.ModelRouter) {
		mr.Spec.Rules = nil
		mr.Spec.AgentGatewayRef.Routing = inferencev1alpha1.AgentGatewayRoutingModel
		mr.Spec.AgentGatewayRef.SectionName = "http"
		mr.Spec.AgentGatewayRef.EndpointPicker, mr.Spec.AgentGatewayRef.EndpointPickerPort = "", 0
	}
	update(toModel)
	cond, fresh := agentGatewayReadyCondition(t, c, mr)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("after switching to routing: Model: expected AgentGatewayReady=True, got %+v", cond)
	}
	assertNoPolicyInNamespace(t, c, gwNS, policyName)
	assertNotExistsInNamespace(t, c, inferencePoolGVK(), ns, "switch-cuda")
	if controllerutil.ContainsFinalizer(fresh, modelRouterAgentGatewayFinalizer) {
		t.Errorf("routing: Model kept the cross-namespace policy finalizer")
	}
	getUnstructuredInNamespace(t, c, agentGatewayModelGVK(), ns, "switcher-switch-cuda")
	route := getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "switcher")
	if rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules"); len(rules) != 1 ||
		rules[0].(map[string]interface{})["name"] != agentGatewayModelRuleName {
		t.Errorf("route was not rewritten to the model router rule: %v", rules)
	}

	// Model -> InferencePool.
	update(func(mr *inferencev1alpha1.ModelRouter) {
		back := crossNamespaceRouterFixture(ns, "switcher", gwNS, "switch-cuda")
		mr.Spec.AgentGatewayRef = back.Spec.AgentGatewayRef
		mr.Spec.Rules = back.Spec.Rules
	})
	if cond, _ := agentGatewayReadyCondition(t, c, mr); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("after switching back to InferencePool: expected AgentGatewayReady=True, got %+v", cond)
	}
	assertNotExistsInNamespace(t, c, agentGatewayModelGVK(), ns, "switcher-switch-cuda")
	getPolicyInNamespace(t, c, gwNS, policyName)
	getUnstructuredInNamespace(t, c, inferencePoolGVK(), ns, "switch-cuda")

	// Model -> Proxy: leaving the plane sweeps the route and the models.
	update(toModel)
	getUnstructuredInNamespace(t, c, agentGatewayModelGVK(), ns, "switcher-switch-cuda")
	update(func(mr *inferencev1alpha1.ModelRouter) {
		mr.Spec.DataPlane = inferencev1alpha1.ModelRouterDataPlaneProxy
	})
	assertNotExistsInNamespace(t, c, agentGatewayModelGVK(), ns, "switcher-switch-cuda")
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "switcher")
}

func TestModelRouterAgentGateway_ModelRoutingRefusesUnownedModel(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "chat-llama")

	handmade := &unstructured.Unstructured{}
	handmade.SetGroupVersionKind(agentGatewayModelGVK())
	handmade.SetName("llm-router-chat-llama")
	handmade.SetNamespace(ns)
	handmade.Object["spec"] = map[string]interface{}{
		"parentRefs": []interface{}{map[string]interface{}{"kind": "HTTPRoute", "name": "hand-written"}},
		"provider":   "Custom",
		"custom": map[string]interface{}{
			"backendRef": map[string]interface{}{"name": "chat-llama", "port": int64(8080)},
			"formats":    []interface{}{map[string]interface{}{"type": "Completions"}},
		},
	}
	if err := c.Create(context.Background(), handmade); err != nil {
		t.Fatalf("create handmade model: %v", err)
	}

	mr := modelRoutingRouter(ns, "llm-router", "chat-llama")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	r := newModelRouterAgentGatewayReconciler(t, cfg)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns}})
	if err != nil {
		t.Fatalf("reconcile must refuse, not error: %v", err)
	}
	if res.RequeueAfter != agentGatewayConflictRequeueAfter {
		t.Errorf("requeue after = %v, want %v (the name-taken recheck)", res.RequeueAfter, agentGatewayConflictRequeueAfter)
	}
	cond, _ := agentGatewayReadyCondition(t, c, mr)
	if cond == nil || cond.Reason != modelRouterAgentGatewayReasonResourceNameTaken ||
		!strings.Contains(cond.Message, "AgentgatewayModel "+ns+"/llm-router-chat-llama") {
		t.Fatalf("expected AgentGatewayReady=False/%s naming the model, got %+v", modelRouterAgentGatewayReasonResourceNameTaken, cond)
	}
	assertForeignObjectUntouched(t, c, handmade)
	assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, "llm-router")
}

func TestModelRouterAgentGateway_ModelRoutingRefusals(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeBackendISvcInNamespace(t, c, ns, "llama")
	makeModelISvc(t, c, ns, "sglang", RuntimeSGLANG, "", 8080)
	makeModelISvc(t, c, ns, "bare-vllm", RuntimeVLLM, "", 8000)
	r := newModelRouterAgentGatewayReconciler(t, cfg)

	tests := []struct {
		name, isvc, reason, message string
		mutate                      func(*inferencev1alpha1.ModelRouter)
	}{
		{"default-route", "llama", modelRouterAgentGatewayReasonUnsupportedModelRouting, "spec.defaultRoute",
			func(mr *inferencev1alpha1.ModelRouter) { mr.Spec.DefaultRoute = "llama" }},
		{"unmapped-runtime", "sglang", modelRouterAgentGatewayReasonUnsupportedRuntime, `runtime "sglang"`,
			func(*inferencev1alpha1.ModelRouter) {}},
		// vLLM serves its modelRef, bare-vllm, so a different served name is
		// refused (#1988).
		{"vllm-serves-another-name", "bare-vllm", modelRouterAgentGatewayReasonUnsupportedRuntime,
			`"--served-model-name=chat-large"`,
			func(mr *inferencev1alpha1.ModelRouter) { mr.Spec.Backends[0].DisplayName = "chat-large" }},
		{"unknown-listener", "llama", modelRouterAgentGatewayReasonNotAllowed, `no listener named "models"`,
			func(mr *inferencev1alpha1.ModelRouter) { mr.Spec.AgentGatewayRef.SectionName = "models" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := modelRoutingRouter(ns, "refused-"+tt.name, tt.isvc)
			tt.mutate(mr)
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatalf("create modelrouter: %v", err)
			}
			reconcileAgentGatewayRouter(t, r, mr)
			cond, _ := agentGatewayReadyCondition(t, c, mr)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tt.reason || !strings.Contains(cond.Message, tt.message) {
				t.Fatalf("expected AgentGatewayReady=False/%s containing %q, got %+v", tt.reason, tt.message, cond)
			}
			assertNotExistsInNamespace(t, c, gatewayHTTPRouteGVK(), ns, mr.Name)
			assertNotExistsInNamespace(t, c, agentGatewayModelGVK(), ns, mr.Name+"-"+tt.isvc)
		})
	}
}

// vLLM serves its modelRef with no extraArgs (#1988), so a backend whose
// served name is the modelRef compiles.
func TestModelRouterAgentGateway_ModelRoutingVLLMServesModelRef(t *testing.T) {
	c, cfg, stop := startAgentGatewayTestEnv(t, true)
	defer stop()
	ns := newTestNamespace(t, c)

	makeGateway(t, c, ns, "ai-gateway", nil, nil)
	makeModelISvc(t, c, ns, "bare-vllm", RuntimeVLLM, "", 8000)
	mr := modelRoutingRouter(ns, "by-ref", "bare-vllm")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	reconcileAgentGatewayRouter(t, newModelRouterAgentGatewayReconciler(t, cfg), mr)
	if cond, _ := agentGatewayReadyCondition(t, c, mr); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected AgentGatewayReady=True, got %+v", cond)
	}
	m := getUnstructuredInNamespace(t, c, agentGatewayModelGVK(), ns, "by-ref-bare-vllm")
	if got, _, _ := unstructured.NestedString(m.Object, "spec", "match", "model"); got != "bare-vllm" {
		t.Errorf("match.model = %q, want bare-vllm", got)
	}
}

// routing: Model needs the AgentgatewayModel CRD and nothing else.
func TestModelRouterAgentGateway_ModelRoutingCRDGate(t *testing.T) {
	t.Run("without the AgentgatewayModel CRD", func(t *testing.T) {
		// The gate runs before the Gateway lookup, so the base-only env needs
		// no Gateway.
		c, cfg, stop := startAgentGatewayTestEnv(t, false)
		defer stop()
		ns := newTestNamespace(t, c)
		makeBackendISvcInNamespace(t, c, ns, "gate-llama")
		mr := modelRoutingRouter(ns, "gate", "gate-llama")
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter: %v", err)
		}
		reconcileAgentGatewayRouter(t, newModelRouterAgentGatewayReconciler(t, cfg), mr)
		cond, _ := agentGatewayReadyCondition(t, c, mr)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != modelRouterAgentGatewayReasonModelCRDMissing ||
			!strings.Contains(cond.Message, "AgentgatewayModel (agentgateway.dev/v1alpha1)") {
			t.Errorf("expected AgentGatewayReady=False/%s naming the kind, got %+v", modelRouterAgentGatewayReasonModelCRDMissing, cond)
		}
	})

	t.Run("with only the AgentgatewayModel CRD", func(t *testing.T) {
		// A RESTMapper that knows only this package's types, the Gateway API
		// kinds the reconcile touches, and AgentgatewayModel: no InferencePool
		// or AgentgatewayPolicy.
		s := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
		if err := inferencev1alpha1.AddToScheme(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
		mapper := apimeta.NewDefaultRESTMapper(nil)
		for gvk := range s.AllKnownTypes() {
			mapper.Add(gvk, apimeta.RESTScopeNamespace)
		}
		for _, gvk := range []schema.GroupVersionKind{
			{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"}, gatewayHTTPRouteGVK(), agentGatewayModelGVK(),
		} {
			mapper.Add(gvk, apimeta.RESTScopeNamespace)
		}
		c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).
			WithStatusSubresource(&inferencev1alpha1.ModelRouter{}, &inferencev1alpha1.InferenceService{}).Build()

		makeGateway(t, c, testNS, "ai-gateway", nil, nil)
		makeBackendISvcInNamespace(t, c, testNS, "gate-llama")
		mr := modelRoutingRouter(testNS, "gate", "gate-llama")
		if err := c.Create(context.Background(), mr); err != nil {
			t.Fatalf("create modelrouter: %v", err)
		}
		reconcileAgentGatewayRouter(t, &ModelRouterAgentGatewayReconciler{Client: c, Scheme: s}, mr)
		if cond, _ := agentGatewayReadyCondition(t, c, mr); cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("expected AgentGatewayReady=True, got %+v", cond)
		}
		getUnstructuredInNamespace(t, c, agentGatewayModelGVK(), testNS, "gate-gate-llama")
		getUnstructuredInNamespace(t, c, gatewayHTTPRouteGVK(), testNS, "gate")
	})
}
