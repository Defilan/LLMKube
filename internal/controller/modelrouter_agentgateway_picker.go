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
	"fmt"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// agentGatewayPicker is one Endpoint Picker Service, identified by name and
// port: the same Service on two ports is two pickers.
type agentGatewayPicker struct {
	Name string
	Port int32
}

// agentGatewayResolvePoolPickers resolves the Endpoint Picker of every
// InferencePool a dataPlane: AgentGateway router compiles, keyed by pool name
// (the sanitized InferenceService name). A backend's own endpointPicker wins;
// otherwise spec.agentGatewayRef supplies the default. A Gateway API Inference
// Extension EPP binds to exactly one pool, so the router compiles only when
// every pool resolves exactly one picker and no picker serves two pools. A
// non-empty reason names the refusal; the reconciler and the webhook both call
// this, so admission and reconcile cannot diverge. External backends are not
// counted: the reconciler refuses them separately.
func agentGatewayResolvePoolPickers(mr *inferencev1alpha1.ModelRouter) (map[string]agentGatewayPicker, string, string) {
	var def *agentGatewayPicker
	if ref := mr.Spec.AgentGatewayRef; ref != nil && ref.EndpointPicker != "" {
		def = &agentGatewayPicker{Name: ref.EndpointPicker, Port: ref.EndpointPickerPort}
	}

	var pools []string
	isvcOf := map[string]string{}
	pickers := map[string]agentGatewayPicker{}
	for _, b := range mr.Spec.Backends {
		if b.External != nil || b.InferenceServiceRef == nil {
			continue
		}
		isvc := b.InferenceServiceRef.Name
		pool := sanitizeDNSName(isvc)
		if other, seen := isvcOf[pool]; seen && other != isvc {
			return nil, modelRouterAgentGatewayReasonMultiPool, fmt.Sprintf(
				"InferenceServices %q and %q both compile to InferencePool %q, and an InferencePool's Endpoint "+
					"Picker binds to exactly one pool. Rename one InferenceService", other, isvc, pool)
		}
		if _, seen := isvcOf[pool]; !seen {
			isvcOf[pool] = isvc
			pools = append(pools, pool)
		}

		if b.EndpointPicker == nil || b.EndpointPicker.Service == nil {
			continue
		}
		p := agentGatewayPicker{Name: b.EndpointPicker.Service.Name, Port: b.EndpointPicker.Service.Port}
		if prev, set := pickers[pool]; set && prev != p {
			return nil, modelRouterAgentGatewayReasonConflictingPicker, fmt.Sprintf(
				"backends on InferencePool %q name different Endpoint Pickers (%s:%d and %s:%d); "+
					"a pool has exactly one picker", pool, prev.Name, prev.Port, p.Name, p.Port)
		}
		pickers[pool] = p
	}

	for _, pool := range pools {
		if _, set := pickers[pool]; set {
			continue
		}
		if def == nil {
			return nil, modelRouterAgentGatewayReasonPickerRequired, fmt.Sprintf(
				"InferencePool %q has no Endpoint Picker: set spec.backends[].endpointPicker.service on its "+
					"backend, or spec.agentGatewayRef.endpointPicker and endpointPickerPort", pool)
		}
		pickers[pool] = *def
	}

	owner := map[agentGatewayPicker]string{}
	for _, pool := range pools {
		p := pickers[pool]
		if other, taken := owner[p]; taken {
			return nil, modelRouterAgentGatewayReasonMultiPool, fmt.Sprintf(
				"InferencePools %q and %q both use Endpoint Picker %s:%d, but a Gateway API Inference Extension "+
					"EPP binds to exactly one pool, so it would make endpoint decisions for the wrong pool. "+
					"Set spec.backends[].endpointPicker.service so each InferenceService has its own picker",
				other, pool, p.Name, p.Port)
		}
		owner[p] = pool
	}
	return pickers, "", ""
}
