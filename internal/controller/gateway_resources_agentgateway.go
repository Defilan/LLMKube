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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// This file is the ModelRouter dataPlane: AgentGateway compiler. It compiles a
// ModelRouter's backends + rules onto an agentgateway data plane via the
// Gateway API Inference Extension:
//
//   - one InferencePool per InferenceServiceRef backend (selecting that
//     InferenceService's pods by label, referencing the operator's Endpoint
//     Picker),
//   - one AgentgatewayPolicy per ModelRouter (traffic.phase: PreRouting) that
//     extracts the OpenAI "model" field from the request body into the
//     agentGatewayHTTPModelHeader before route selection, because agentgateway
//     populates no model header natively,
//   - one multi-rule HTTPRoute (a rule per spec.rules entry, matching on the
//     extracted model header, plus header matches) whose backendRefs reference
//     the generated InferencePools directly.
//
// Shapes follow the Gateway API Inference Extension v1 API
// (inference.networking.k8s.io/v1) and agentgateway's inference-routing docs.
// GIE removed InferenceModel (renamed InferenceObjective, then dropped from
// GIE entirely in v1.6.0); the v1 routing path is HTTPRoute -> InferencePool
// with model-name matching in the gateway layer (#1725). As with the Envoy
// plane we build *unstructured.Unstructured (no external module dependency)
// and own everything via the ModelRouter owner ref.

const (
	// inferenceExtensionGroup is the Gateway API Inference Extension API group.
	inferenceExtensionGroup   = "inference.networking.k8s.io"
	inferenceExtensionVersion = "v1"
	inferencePoolKind         = "InferencePool"

	// The agentgateway.dev group holds the AgentgatewayPolicy CRD the operator
	// generates for model-name extraction. The InferencePool lives in the
	// Inference Extension's group; the two cannot share G/V constants.
	agentGatewayPolicyGroup   = "agentgateway.dev"
	agentGatewayPolicyVersion = "v1alpha1"
	agentGatewayPolicyKind    = "AgentgatewayPolicy"

	// agentGatewayHTTPModelHeader is the header the generated AgentgatewayPolicy
	// extracts the OpenAI "model" field into, and the header the generated
	// HTTPRoute rules match on. This is the documented agentgateway BBR pattern
	// (traffic.phase: PreRouting transformation + HTTPRoute header matches); a
	// model match is an Exact header match on it.
	agentGatewayHTTPModelHeader = "X-Gateway-Model-Name"

	// agentGatewayModelExtractionExpression is the CEL expression the generated
	// AgentgatewayPolicy evaluates to copy the request body "model" field into
	// agentGatewayHTTPModelHeader. Pinned to the agentgateway docs' form.
	agentGatewayModelExtractionExpression = "json(request.body).model"
)

// inferencePoolGVK / agentGatewayPolicyGVK are the GVKs of the generated
// resources. Exposed as functions so the reconciler and tests share a single
// source of truth.
func inferencePoolGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: inferenceExtensionGroup, Version: inferenceExtensionVersion, Kind: inferencePoolKind}
}

func agentGatewayPolicyGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: agentGatewayPolicyGroup, Version: agentGatewayPolicyVersion, Kind: agentGatewayPolicyKind}
}

// modelRouterAgentGatewayResourceName is the shared name for the HTTPRoute and
// the model-extraction AgentgatewayPolicy of a ModelRouter (different kinds, so
// the shared name cannot collide). Per-backend InferencePool resources are
// named after the InferenceService instead (so route backendRefs can reference
// them), see newAgentGatewayInferencePool.
func modelRouterAgentGatewayResourceName(mr *inferencev1alpha1.ModelRouter) string {
	return sanitizeDNSName(mr.Name)
}

// inferencePoolSelectorLabels returns the label set an InferencePool uses to
// select a backend's pods. It is the SAME selector the InferenceService's
// Deployment/Service uses (deploymentSelectorLabels), so the pool selects the
// right model-server pods. Returned as map[string]interface{} for direct
// embedding into an unstructured spec.
func inferencePoolSelectorLabels(isvcName string) map[string]interface{} {
	return map[string]interface{}{
		"app":                           isvcName,
		"inference.llmkube.dev/service": isvcName,
	}
}

// newAgentGatewayInferencePool builds the inference.networking.k8s.io
// InferencePool for one backend. It selects the referenced InferenceService's
// pods by label and references the backend's resolved Endpoint Picker so agentgateway
// can select a model-server endpoint per request. Lives in the ModelRouter
// namespace.
func newAgentGatewayInferencePool(mr *inferencev1alpha1.ModelRouter, b agentGatewayBackendResource) *unstructured.Unstructured {
	epRef := map[string]interface{}{
		metadataNameField: b.Picker.Name,
		"port": map[string]interface{}{
			"number": int64(b.Picker.Port),
		},
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(inferencePoolGVK())
	u.SetName(b.PoolName)
	u.SetNamespace(mr.Namespace)
	// The UID label marks this pool as generated by this router so the
	// leaving-AgentGateway sweep can find it (mirrors the extraction policy).
	u.SetLabels(map[string]string{agentGatewayRouterUIDLabel: string(mr.UID)})
	u.Object["spec"] = map[string]interface{}{
		// The v1 InferencePool selector is a LabelSelector-shaped object with a
		// required matchLabels field; a flat map is rejected server-side.
		"selector": map[string]interface{}{
			"matchLabels": inferencePoolSelectorLabels(b.PoolName),
		},
		"targetPorts": []interface{}{
			map[string]interface{}{
				"number": b.Port,
			},
		},
		"endpointPickerRef": epRef,
	}
	return u
}

// agentGatewayPolicyIdentity resolves where the model-extraction
// AgentgatewayPolicy lives and what it is called. Same-namespace Gateway: the
// policy lives beside the router, named after it, and is owner-referenced for
// GC. Cross-namespace Gateway: the policy must live in the Gateway's namespace
// (same-namespace targetRefs), where it cannot be owner-referenced, so it is
// named after the router's namespace and name and managed by the ModelRouter
// finalizer. The concatenation behind that name is not injective; the apply
// refuses a name another router's policy already holds.
func agentGatewayPolicyIdentity(mr *inferencev1alpha1.ModelRouter) (namespace, name string) {
	ns := agentGatewayPolicyNamespace(mr)
	if ns == mr.Namespace {
		return ns, modelRouterAgentGatewayResourceName(mr)
	}
	return ns, sanitizeDNSName(mr.Namespace + "-" + mr.Name)
}

// newAgentGatewayModelExtractionPolicy builds the one-per-ModelRouter
// AgentgatewayPolicy (agentgateway.dev/v1alpha1) that extracts the OpenAI
// "model" field from the request body into agentGatewayHTTPModelHeader before
// route selection, so the generated HTTPRoute rules can match on it. The phase
// must be PreRouting (extraction decides routing), and the AgentgatewayPolicy
// CRD's CEL rules restrict PreRouting policies to Gateway/ListenerSet targets
// in the policy's own namespace. The policy targets the referenced Gateway,
// not the route, and lives in the Gateway's namespace (see
// agentGatewayPolicyIdentity). The UID label marks it as this router's policy
// for conflict detection, cross-namespace cleanup, and collision refusal.
func newAgentGatewayModelExtractionPolicy(mr *inferencev1alpha1.ModelRouter, ref *inferencev1alpha1.AgentGatewayReference) *unstructured.Unstructured {
	ns, name := agentGatewayPolicyIdentity(mr)
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(agentGatewayPolicyGVK())
	u.SetName(name)
	u.SetNamespace(ns)
	u.SetLabels(map[string]string{agentGatewayRouterUIDLabel: string(mr.UID)})
	u.Object["spec"] = map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				metadataNameField: ref.Name,
				"kind":            "Gateway",
				"group":           gatewayBackendRefGroupAPI,
			},
		},
		"traffic": map[string]interface{}{
			"phase": "PreRouting",
			"transformation": map[string]interface{}{
				"request": map[string]interface{}{
					"set": []interface{}{
						map[string]interface{}{
							"name":  agentGatewayHTTPModelHeader,
							"value": agentGatewayModelExtractionExpression,
						},
					},
				},
			},
		},
	}
	return u
}

// newAgentGatewayHTTPRoute builds the one-per-ModelRouter multi-rule HTTPRoute
// attached to the referenced agentgateway Gateway. Each resolved rule becomes
// one spec.rules entry whose matches OR over the rule's model names (each ANDed
// with the rule's header matches) and whose backendRefs reference the generated
// InferencePools directly. Mirrors the Gateway plane's route shape.
func newAgentGatewayHTTPRoute(
	mr *inferencev1alpha1.ModelRouter,
	ref *inferencev1alpha1.AgentGatewayReference,
	rules []agentGatewayRuleResource,
	backends []agentGatewayBackendResource,
) *unstructured.Unstructured {
	compiledRules := make([]interface{}, 0, len(rules))
	poolFor := make(map[string]string, len(backends))
	for _, b := range backends {
		poolFor[b.Name] = b.PoolName
	}
	for _, rule := range rules {
		compiledRules = append(compiledRules, compileAgentGatewayRouteRule(rule, poolFor))
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gatewayHTTPRouteGVK())
	u.SetName(modelRouterAgentGatewayResourceName(mr))
	u.SetNamespace(mr.Namespace)
	// The UID label marks this route as generated by this router so the
	// leaving-AgentGateway sweep can find it (mirrors the extraction policy).
	u.SetLabels(map[string]string{agentGatewayRouterUIDLabel: string(mr.UID)})
	u.Object["spec"] = map[string]interface{}{
		"parentRefs": []interface{}{agentGatewayRouteParentRef(ref)},
		"rules":      compiledRules,
	}
	return u
}

// agentGatewayRouteParentRef only carries a sectionName with routing: Model,
// where the CRD allows it, so the InferencePool route renders unchanged.
func agentGatewayRouteParentRef(ref *inferencev1alpha1.AgentGatewayReference) map[string]interface{} {
	parentRef := map[string]interface{}{
		metadataNameField: ref.Name,
		"kind":            "Gateway",
		"group":           gatewayBackendRefGroupAPI,
	}
	if ref.Namespace != "" {
		parentRef["namespace"] = ref.Namespace
	}
	if ref.SectionName != "" {
		parentRef["sectionName"] = ref.SectionName
	}
	return parentRef
}

// compileAgentGatewayRouteRule builds one HTTPRoute rule: the matches block (a
// match per model name, ANDed with header matches; a header-only match when the
// rule declares no models, e.g. a catch-all) and the backendRefs block.
func compileAgentGatewayRouteRule(rule agentGatewayRuleResource, poolFor map[string]string) map[string]interface{} {
	return map[string]interface{}{
		"matches":     compileAgentGatewayRuleMatches(rule),
		"backendRefs": compileAgentGatewayRouteBackendRefs(rule.BackendRefs, poolFor),
	}
}

// compileAgentGatewayRuleMatches turns a rule's model names + headers into
// HTTPRoute match entries. The generated AgentgatewayPolicy extracts the
// request body "model" field into agentGatewayHTTPModelHeader before route
// selection, so a model match is an Exact header match on that header; user
// headers are ANDed into every model match. Match entries are ORed; conditions
// within an entry are ANDed.
func compileAgentGatewayRuleMatches(rule agentGatewayRuleResource) []interface{} {
	headerMatches := sortedHeaderMatches(rule.Headers)

	modelOpts := rule.Models
	if len(modelOpts) == 0 {
		modelOpts = []string{""} // a single placeholder => header-only / catch-all match
	}

	matches := make([]interface{}, 0, len(modelOpts))
	for _, model := range modelOpts {
		headers := make([]interface{}, 0, 1+len(headerMatches))
		if model != "" {
			headers = append(headers, exactHeaderMatch(agentGatewayHTTPModelHeader, model))
		}
		headers = append(headers, headerMatches...)
		matches = append(matches, map[string]interface{}{
			"headers": headers,
		})
	}
	return matches
}

// compileAgentGatewayRouteBackendRefs turns the resolved backend refs into
// HTTPRoute backendRefs, each referencing the backend's generated InferencePool
// directly (the GIE v1 routing path; there is no InferenceModel indirection).
// poolFor maps RouterBackend.Name to the pool name. Two backends may share one
// pool (same referenced InferenceService), so refs are deduped by pool name in
// first-appearance order; ejection already filtered by backend name upstream.
// Only the first resolved pool is emitted: see the primary-fallback note in the
// body.
func compileAgentGatewayRouteBackendRefs(refs []agentGatewayBackendRef, poolFor map[string]string) []interface{} {
	out := make([]interface{}, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		pool, ok := poolFor[ref.Name]
		if !ok || seen[pool] {
			continue
		}
		seen[pool] = true
		out = append(out, map[string]interface{}{
			metadataNameField: pool,
			"kind":            inferencePoolKind,
			"group":           inferenceExtensionGroup,
		})
	}
	// primary-fallback routes to the first healthy backend only. Gateway API
	// backendRef weights load-balance, so emitting every pool would split
	// traffic evenly instead of failing over. Ejection (agentGatewayEjectUnhealthy)
	// already dropped unhealthy backends, or kept the original list when every
	// backend was unhealthy, so the first ref is the first healthy backend or
	// the primary.
	if len(out) > 1 {
		out = out[:1]
	}
	return out
}

// gatewayHTTPRouteGVK is the GVK of the generated HTTPRoute. Same group/version
// as the Gateway plane's Gateway (gateway.networking.k8s.io/v1).
func gatewayHTTPRouteGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayBackendRefGroupAPI, Version: "v1", Kind: "HTTPRoute"}
}
