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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// routing: Model compiles a router to agentgateway's own model router
// (v1.6.0+): one HTTPRoute whose single rule hands every request to
// AgentgatewayModel "*", and one AgentgatewayModel per backend. agentgateway
// matches the body's model, and answers GET /v1/models and unknown models
// itself.

const (
	agentGatewayModelKind = "AgentgatewayModel"

	// Models name the route's rule as sectionName: agentgateway lets a
	// parentRef omit it only while the route has exactly one rule.
	agentGatewayModelRuleName = "models"

	// A DNS-1123 label, so a generated name also fits a label value.
	agentGatewayModelNameMaxLength = 63

	// The AgentgatewayModel CRD's match.model maxLength.
	agentGatewayModelMatchMaxLength = 1024

	modelRouterAgentGatewayReasonModelCRDMissing         = "AgentgatewayModelCRDNotInstalled"
	modelRouterAgentGatewayReasonUnsupportedModelRouting = "UnsupportedInModelRouting"
	modelRouterAgentGatewayReasonUnsupportedRuntime      = "UnsupportedRuntimeInModelRouting"
)

func agentGatewayModelGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: agentGatewayPolicyGroup, Version: agentGatewayPolicyVersion, Kind: agentGatewayModelKind}
}

// agentGatewayModelRouting reports routing: Model; unset means InferencePool.
func agentGatewayModelRouting(mr *inferencev1alpha1.ModelRouter) bool {
	ref := mr.Spec.AgentGatewayRef
	return ref != nil && ref.Routing == inferencev1alpha1.AgentGatewayRoutingModel
}

func agentGatewayServedName(b inferencev1alpha1.RouterBackend) string {
	if b.DisplayName != "" {
		return b.DisplayName
	}
	return b.Name
}

// agentGatewayModelName derives the object name from router and backend, not
// from the served name, which need not be a valid name ("org/model"). Long or
// dash-ending names are cut and hashed. Collisions ("a-b"+"c" vs "a"+"b-c", or
// routers "a.b" and "a-b") are refused as GeneratedResourceNameTaken.
func agentGatewayModelName(mr *inferencev1alpha1.ModelRouter, backend string) string {
	name := sanitizeDNSName(mr.Name) + "-" + backend
	if len(name) <= agentGatewayModelNameMaxLength && !strings.HasSuffix(name, "-") {
		return name
	}
	sum := sha256.Sum256([]byte(mr.Name + "/" + backend))
	tag := "-" + hex.EncodeToString(sum[:])[:8]
	keep := min(len(name), agentGatewayModelNameMaxLength-len(tag))
	return strings.TrimRight(name[:keep], "-") + tag
}

// agentGatewayModelProvider maps an InferenceService's runtime and serving mode
// to agentgateway's provider identity and API formats. ok is false for other
// runtimes: a guessed format would fail every request.
func agentGatewayModelProvider(isvc *inferencev1alpha1.InferenceService) (provider string, formats []string, ok bool) {
	var chat []string
	switch runtimeNameLabel(isvc) {
	case "llamacpp":
		provider, chat = "llamacpp", []string{"Completions"}
	case RuntimeVLLM:
		// vLLM serves Messages and Responses itself.
		provider, chat = "vllm", []string{"Completions", "Messages", "AnthropicTokenCount", "Responses"}
	default:
		return "", nil, false
	}
	switch resolveServingMode(isvc) {
	case servingModeEmbedding:
		return provider, []string{"Embeddings"}, true
	case servingModeRerank:
		return provider, []string{"Rerank"}, true
	}
	return provider, chat, true
}

// agentGatewayModelRoutingMessage returns why a routing: Model spec cannot
// compile, or "". The reconciler and the webhook share it.
func agentGatewayModelRoutingMessage(mr *inferencev1alpha1.ModelRouter) string {
	if !agentGatewayModelRouting(mr) {
		return ""
	}
	if len(mr.Spec.Rules) > 0 {
		return fmt.Sprintf("rule %q does not compile with agentGatewayRef.routing: Model: agentgateway's model router "+
			"sends each request to the backend whose displayName (else name) equals its model field. Remove "+
			"spec.rules, or use routing: InferencePool", mr.Spec.Rules[0].Name)
	}
	if mr.Spec.DefaultRoute != "" {
		return "spec.defaultRoute does not compile with agentGatewayRef.routing: Model: agentgateway answers an " +
			"unknown model with 404 model_not_found. Remove it, or use routing: InferencePool"
	}
	servedBy := make(map[string]string, len(mr.Spec.Backends))
	for _, b := range mr.Spec.Backends {
		switch {
		case b.External != nil:
			return fmt.Sprintf("backend %q is external; agentGatewayRef.routing: Model compiles only "+
				"inferenceServiceRef backends", b.Name)
		case b.Resolution == inferencev1alpha1.RouterBackendResolutionEndpoint:
			return fmt.Sprintf("backend %q sets resolution: endpoint; agentGatewayRef.routing: Model reaches the "+
				"InferenceService through its Service (resolution: service)", b.Name)
		case b.EndpointPicker != nil:
			return fmt.Sprintf("backend %q sets endpointPicker, which applies only to agentGatewayRef.routing: "+
				"InferencePool", b.Name)
		}
		served := agentGatewayServedName(b)
		if len(served) > agentGatewayModelMatchMaxLength {
			return fmt.Sprintf("backend %q serves a model name of %d characters; agentgateway's match.model accepts at most %d",
				b.Name, len(served), agentGatewayModelMatchMaxLength)
		}
		if strings.Contains(served, "*") {
			return fmt.Sprintf("backend %q serves model %q; agentgateway reads '*' in a model name as a wildcard", b.Name, served)
		}
		if other, taken := servedBy[served]; taken {
			return fmt.Sprintf("backends %q and %q both serve model %q; agentgateway's model router needs one "+
				"backend per model name", other, b.Name, served)
		}
		servedBy[served] = b.Name
	}
	return ""
}

// agentGatewayRefusalError is a refusal found during generation, reported as a
// condition rather than a reconcile error.
type agentGatewayRefusalError struct {
	reason, message string
}

func (e *agentGatewayRefusalError) Error() string { return e.message }

type agentGatewayModelBackend struct {
	ObjectName string
	ServedName string
	Service    string
	Port       int64
	Provider   string
	Formats    []string
}

// newAgentGatewayModelRoute builds the router's HTTPRoute. The rule sits at "/"
// because agentgateway strips the rule's path prefix before it classifies the
// endpoint: under /v1, an embeddings request would parse as a completion. It
// reuses the InferencePool route's name, so a mode switch rewrites one route.
func newAgentGatewayModelRoute(mr *inferencev1alpha1.ModelRouter) *unstructured.Unstructured {
	ref := mr.Spec.AgentGatewayRef
	spec := map[string]interface{}{
		"parentRefs": []interface{}{agentGatewayRouteParentRef(ref)},
		"rules": []interface{}{
			map[string]interface{}{
				metadataNameField: agentGatewayModelRuleName,
				"backendRefs": []interface{}{
					map[string]interface{}{
						metadataNameField: "*",
						"kind":            agentGatewayModelKind,
						"group":           agentGatewayPolicyGroup,
					},
				},
			},
		},
	}
	if len(ref.Hostnames) > 0 {
		spec["hostnames"] = toInterfaceSlice(ref.Hostnames)
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gatewayHTTPRouteGVK())
	u.SetName(modelRouterAgentGatewayResourceName(mr))
	u.SetNamespace(mr.Namespace)
	u.SetLabels(map[string]string{agentGatewayRouterUIDLabel: string(mr.UID)})
	u.Object["spec"] = spec
	return u
}

// newAgentGatewayModel sets group and kind where the CRD would default them,
// so an unchanged spec causes no update.
func newAgentGatewayModel(mr *inferencev1alpha1.ModelRouter, route string, b agentGatewayModelBackend) *unstructured.Unstructured {
	formats := make([]interface{}, 0, len(b.Formats))
	for _, f := range b.Formats {
		formats = append(formats, map[string]interface{}{"type": f})
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(agentGatewayModelGVK())
	u.SetName(b.ObjectName)
	u.SetNamespace(mr.Namespace)
	u.SetLabels(map[string]string{agentGatewayRouterUIDLabel: string(mr.UID)})
	u.Object["spec"] = map[string]interface{}{
		"parentRefs": []interface{}{
			map[string]interface{}{
				metadataNameField: route,
				"kind":            "HTTPRoute",
				"group":           gatewayBackendRefGroupAPI,
				"sectionName":     agentGatewayModelRuleName,
			},
		},
		"match":    map[string]interface{}{"model": b.ServedName},
		"provider": "Custom",
		"custom": map[string]interface{}{
			"backendRef": map[string]interface{}{
				metadataNameField: b.Service,
				"port":            b.Port,
			},
			"providerOverride": b.Provider,
			"formats":          formats,
		},
	}
	return u
}

// vllmServesModel reports whether vLLM answers to model. The request keeps its
// model name, so it must be one the container is started with: the user's
// --served-model-name, else the modelRef LLMKube sets (#1988).
func vllmServesModel(isvc *inferencev1alpha1.InferenceService, model string) bool {
	return slices.Contains(servedModelNames(isvc), model)
}

// resolveAgentGatewayModelBackends returns an agentGatewayRefusalError for an
// unmapped runtime.
func (r *ModelRouterAgentGatewayReconciler) resolveAgentGatewayModelBackends(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]agentGatewayModelBackend, error) {
	resolved := make([]agentGatewayModelBackend, 0, len(mr.Spec.Backends))
	for _, b := range mr.Spec.Backends {
		isvc, err := r.backendInferenceService(ctx, mr, b)
		if err != nil {
			return nil, err
		}
		provider, formats, ok := agentGatewayModelProvider(isvc)
		if !ok {
			return nil, &agentGatewayRefusalError{
				reason: modelRouterAgentGatewayReasonUnsupportedRuntime,
				message: fmt.Sprintf("backend %q: InferenceService %q runs runtime %q, which agentGatewayRef.routing: "+
					"Model has no agentgateway provider mapping for (supported: llamacpp, vllm); use routing: "+
					"InferencePool for it", b.Name, isvc.Name, runtimeNameLabel(isvc)),
			}
		}
		served := agentGatewayServedName(b)
		if runtimeNameLabel(isvc) == RuntimeVLLM && !vllmServesModel(isvc, served) {
			return nil, &agentGatewayRefusalError{
				reason: modelRouterAgentGatewayReasonUnsupportedRuntime,
				message: fmt.Sprintf("backend %q: vLLM InferenceService %q does not serve model %q, so every request "+
					"would get a 404; add \"--served-model-name=%s\" to its extraArgs", b.Name, isvc.Name, served, served),
			}
		}
		resolved = append(resolved, agentGatewayModelBackend{
			ObjectName: agentGatewayModelName(mr, b.Name),
			ServedName: served,
			Service:    sanitizeDNSName(isvc.Name),
			Port:       gatewayServicePort(isvc),
			Provider:   provider,
			Formats:    formats,
		})
	}
	return resolved, nil
}

// reconcileAgentGatewayModels applies the models and route, then deletes the
// models of removed backends and whatever routing: InferencePool left behind.
// Deleting last means a mode switch never points the route at deleted pools.
func (r *ModelRouterAgentGatewayReconciler) reconcileAgentGatewayModels(ctx context.Context, mr *inferencev1alpha1.ModelRouter) error {
	backends, err := r.resolveAgentGatewayModelBackends(ctx, mr)
	if err != nil {
		return err
	}

	// Models first, so a rejected model leaves the previous route in place.
	route := newAgentGatewayModelRoute(mr)
	desired := make([]*unstructured.Unstructured, 0, len(backends)+1)
	keep := make(map[string]bool, len(backends))
	for _, b := range backends {
		desired = append(desired, newAgentGatewayModel(mr, route.GetName(), b))
		keep[b.ObjectName] = true
	}
	desired = append(desired, route)

	// Refuse before any write if a generated name belongs to someone else.
	for _, obj := range desired {
		if err := r.checkGeneratedNameAvailable(ctx, mr, obj); err != nil {
			return err
		}
	}

	for _, obj := range desired {
		if err := r.applyAgentGatewayResource(ctx, mr, obj); err != nil {
			return fmt.Errorf("%s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	if err := r.deleteGeneratedObjects(ctx, mr, keep, agentGatewayModelGVK()); err != nil {
		return err
	}
	if err := r.deleteGeneratedObjects(ctx, mr, nil, inferencePoolGVK()); err != nil {
		return err
	}
	return r.withdrawAgentGatewayPolicy(ctx, mr)
}
