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
//   - one multi-rule HTTPRoute (a rule per spec.rules entry, matching on the
//     OpenAI "model" field copied into the x-ai-eg-model header, plus header
//     matches) whose backendRefs reference the backend's InferencePool.
//
// Shapes follow the Gateway API Inference Extension v1 API
// (inference.networking.k8s.io/v1) and agentgateway's inference-routing docs.
// As with the Envoy plane we build *unstructured.Unstructured (no external
// module dependency) and own everything via the ModelRouter owner ref.

const (
	// inferenceExtensionGroup is the Gateway API Inference Extension API group.
	inferenceExtensionGroup   = "inference.networking.k8s.io"
	inferenceExtensionVersion = "v1"
	inferencePoolKind         = "InferencePool"

	// agentGatewayHTTPModelHeader is the header agentgateway matches the OpenAI
	// "model" field on. Mirrors the Envoy plane's x-ai-eg-model: the gateway
	// copies the request body "model" into this header, so a model match is an
	// Exact header match on it.
	agentGatewayHTTPModelHeader = "x-ai-eg-model"
)

// inferencePoolGVK is the GVK of the generated Inference Extension resource.
// Exposed as a function so the reconciler and tests share a single source of
// truth.
func inferencePoolGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: inferenceExtensionGroup, Version: inferenceExtensionVersion, Kind: inferencePoolKind}
}

// modelRouterAgentGatewayResourceName is the shared name for the HTTPRoute of a
// ModelRouter. Per-backend InferencePools are named after the referenced
// InferenceService instead (so route backendRefs can reference them), see
// newAgentGatewayInferencePool.
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
// pods by label and references the operator's Endpoint Picker so agentgateway
// can select a model-server endpoint per request. Lives in the ModelRouter
// namespace.
func newAgentGatewayInferencePool(mr *inferencev1alpha1.ModelRouter, b agentGatewayBackendResource) *unstructured.Unstructured {
	epRef := map[string]interface{}{
		metadataNameField: mr.Spec.AgentGatewayRef.EndpointPicker,
		"port": map[string]interface{}{
			"number": int64(mr.Spec.AgentGatewayRef.EndpointPickerPort),
		},
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(inferencePoolGVK())
	u.SetName(b.PoolName)
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"selector": inferencePoolSelectorLabels(b.PoolName),
		"targetPorts": []interface{}{
			map[string]interface{}{
				"number": b.Port,
			},
		},
		"endpointPickerRef": epRef,
	}
	return u
}

// newAgentGatewayHTTPRoute builds the one-per-ModelRouter multi-rule HTTPRoute
// attached to the referenced agentgateway Gateway. Each resolved rule becomes
// one spec.rules entry whose matches OR over the rule's model names (each ANDed
// with the rule's header matches) and whose backendRefs reference the backends'
// InferencePools. Mirrors the Gateway plane's route shape.
func newAgentGatewayHTTPRoute(
	mr *inferencev1alpha1.ModelRouter,
	ref *inferencev1alpha1.AgentGatewayReference,
	rules []agentGatewayRuleResource,
	backends []agentGatewayBackendResource,
) *unstructured.Unstructured {
	parentRef := map[string]interface{}{
		metadataNameField: ref.Name,
		"kind":            "Gateway",
		"group":           gatewayBackendRefGroupAPI,
	}
	if ref.Namespace != "" {
		parentRef["namespace"] = ref.Namespace
	}

	compiledRules := make([]interface{}, 0, len(rules))
	for _, rule := range rules {
		compiledRules = append(compiledRules, compileAgentGatewayRouteRule(rule, backends))
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gatewayHTTPRouteGVK())
	u.SetName(modelRouterAgentGatewayResourceName(mr))
	u.SetNamespace(mr.Namespace)
	u.Object["spec"] = map[string]interface{}{
		"parentRefs": []interface{}{parentRef},
		"rules":      compiledRules,
	}
	return u
}

// compileAgentGatewayRouteRule builds one HTTPRoute rule: the matches block (a
// match per model name, ANDed with header matches; a header-only match when the
// rule declares no models, e.g. a catch-all) and the backendRefs block.
func compileAgentGatewayRouteRule(rule agentGatewayRuleResource, backends []agentGatewayBackendResource) map[string]interface{} {
	return map[string]interface{}{
		"matches":     compileAgentGatewayRuleMatches(rule),
		"backendRefs": compileAgentGatewayRouteBackendRefs(rule.BackendRefs, backends),
	}
}

// compileAgentGatewayRuleMatches turns a rule's model names + headers into
// HTTPRoute match entries. The agentgateway data plane copies the request body
// "model" field into the x-ai-eg-model header, so a model match is an Exact
// header match on that header; user headers are ANDed into every model match.
// Match entries are ORed; conditions within an entry are ANDed.
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

// compileAgentGatewayRouteBackendRefs turns a rule's resolved backend refs into
// HTTPRoute backendRefs, each referencing the backend's InferencePool directly:
// name = the pool name, kind = InferencePool, group = the Inference Extension
// group, and port = the pool's target port. This is the shape GIE v1 documents
// and that agentgateway serves on the maintainer's cluster today. The refs are
// the (possibly health-ejected) per-rule RouterBackend names; backends supplies
// the pool name + port lookup for each.
func compileAgentGatewayRouteBackendRefs(refs []agentGatewayBackendRef, backends []agentGatewayBackendResource) []interface{} {
	poolByBackend := make(map[string]agentGatewayBackendResource, len(backends))
	for _, b := range backends {
		poolByBackend[b.Name] = b
	}

	out := make([]interface{}, 0, len(refs))
	for _, ref := range refs {
		b, ok := poolByBackend[ref.Name]
		if !ok {
			// A rule referencing a backend that is not in the resolved set should
			// never happen (rules are compiled from the same backends), but skip
			// it rather than emitting a pool ref with no backing pool.
			continue
		}
		backendRef := map[string]interface{}{
			metadataNameField: b.PoolName,
			"kind":            inferencePoolKind,
			"group":           inferenceExtensionGroup,
			"port":            b.Port,
		}
		out = append(out, backendRef)
	}
	return out
}

// gatewayHTTPRouteGVK is the GVK of the generated HTTPRoute. Same group/version
// as the Gateway plane's Gateway (gateway.networking.k8s.io/v1).
func gatewayHTTPRouteGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayBackendRefGroupAPI, Version: "v1", Kind: "HTTPRoute"}
}
