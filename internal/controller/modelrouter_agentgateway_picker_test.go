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
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func pickerBackend(name, isvc, epp string, port int32) inferencev1alpha1.RouterBackend {
	b := inferencev1alpha1.RouterBackend{Name: name, InferenceServiceRef: corev1LocalRef(isvc)}
	if epp != "" {
		b.EndpointPicker = &inferencev1alpha1.RouterEndpointPicker{
			Service: &inferencev1alpha1.EndpointPickerService{Name: epp, Port: port},
		}
	}
	return b
}

func pickerRouter(defaultEPP string, defaultPort int32, backends ...inferencev1alpha1.RouterBackend) *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{Spec: inferencev1alpha1.ModelRouterSpec{
		DataPlane: inferencev1alpha1.ModelRouterDataPlaneAgentGateway,
		AgentGatewayRef: &inferencev1alpha1.AgentGatewayReference{
			Name: "ai-gateway", EndpointPicker: defaultEPP, EndpointPickerPort: defaultPort,
		},
		Backends: backends,
	}}
}

func TestAgentGatewayResolvePoolPickers(t *testing.T) {
	tests := []struct {
		name       string
		mr         *inferencev1alpha1.ModelRouter
		wantReason string
		wantMsg    string
		wantPools  map[string]agentGatewayPicker
	}{{
		name:      "single pool on the router default (today's shape)",
		mr:        pickerRouter("epp", 9002, pickerBackend("a", "qwen", "", 0)),
		wantPools: map[string]agentGatewayPicker{"qwen": {"epp", 9002}},
	}, {
		name: "two pools, a picker each, no default",
		mr: pickerRouter("", 0,
			pickerBackend("glm", "glm-53", "glm-epp", 9002),
			pickerBackend("qwen", "qwen-38", "qwen-epp", 9002)),
		wantPools: map[string]agentGatewayPicker{"glm-53": {"glm-epp", 9002}, "qwen-38": {"qwen-epp", 9002}},
	}, {
		name: "one backend overrides, the other takes the default",
		mr: pickerRouter("epp", 9002,
			pickerBackend("glm", "glm-53", "glm-epp", 9002),
			pickerBackend("qwen", "qwen-38", "", 0)),
		wantPools: map[string]agentGatewayPicker{"glm-53": {"glm-epp", 9002}, "qwen-38": {"epp", 9002}},
	}, {
		name: "two pools on the router default",
		mr: pickerRouter("epp", 9002,
			pickerBackend("glm", "glm-53", "", 0),
			pickerBackend("qwen", "qwen-38", "", 0)),
		wantReason: modelRouterAgentGatewayReasonMultiPool,
		wantMsg:    "exactly one pool",
	}, {
		name: "an override that names the default's Service and port",
		mr: pickerRouter("epp", 9002,
			pickerBackend("glm", "glm-53", "epp", 9002),
			pickerBackend("qwen", "qwen-38", "", 0)),
		wantReason: modelRouterAgentGatewayReasonMultiPool,
		wantMsg:    "exactly one pool",
	}, {
		name: "the same Service on two ports is two pickers",
		mr: pickerRouter("", 0,
			pickerBackend("glm", "glm-53", "epp", 9002),
			pickerBackend("qwen", "qwen-38", "epp", 9012)),
		wantPools: map[string]agentGatewayPicker{"glm-53": {"epp", 9002}, "qwen-38": {"epp", 9012}},
	}, {
		name: "two backends on one InferenceService, different pickers",
		mr: pickerRouter("", 0,
			pickerBackend("a", "qwen", "epp-1", 9002),
			pickerBackend("b", "qwen", "epp-2", 9002)),
		wantReason: modelRouterAgentGatewayReasonConflictingPicker,
		wantMsg:    `InferencePool "qwen"`,
	}, {
		name: "two backends on one InferenceService, one picker and one unset",
		mr: pickerRouter("", 0,
			pickerBackend("a", "qwen", "epp", 9002),
			pickerBackend("b", "qwen", "", 0)),
		wantPools: map[string]agentGatewayPicker{"qwen": {"epp", 9002}},
	}, {
		name:       "no default and no override",
		mr:         pickerRouter("", 0, pickerBackend("a", "qwen", "", 0)),
		wantReason: modelRouterAgentGatewayReasonPickerRequired,
		wantMsg:    `InferencePool "qwen"`,
	}, {
		name: "names that sanitize to one pool",
		mr: pickerRouter("", 0,
			pickerBackend("a", "glm.53", "epp-1", 9002),
			pickerBackend("b", "glm-53", "epp-2", 9002)),
		wantReason: modelRouterAgentGatewayReasonMultiPool,
		wantMsg:    `both compile to InferencePool "glm-53"`,
	}, {
		// Only the collision check guards this shape: one shared picker and one
		// computed pool name would otherwise compile, and the second
		// InferenceService's pool apply would overwrite the first.
		name: "names that sanitize to one pool, both on the router default",
		mr: pickerRouter("epp", 9002,
			pickerBackend("a", "glm.53", "", 0),
			pickerBackend("b", "glm-53", "", 0)),
		wantReason: modelRouterAgentGatewayReasonMultiPool,
		wantMsg:    `both compile to InferencePool "glm-53"`,
	}, {
		name: "a backend with no picker inherits its pool's picker over the router default",
		mr: pickerRouter("epp", 9002,
			pickerBackend("a", "qwen", "qwen-epp", 9002),
			pickerBackend("b", "qwen", "", 0)),
		wantPools: map[string]agentGatewayPicker{"qwen": {"qwen-epp", 9002}},
	}, {
		name: "external backends are not counted",
		mr: func() *inferencev1alpha1.ModelRouter {
			mr := pickerRouter("epp", 9002, pickerBackend("a", "qwen", "", 0))
			mr.Spec.Backends = append(mr.Spec.Backends, inferencev1alpha1.RouterBackend{
				Name: "cloud", External: &inferencev1alpha1.ExternalProvider{Provider: "openai", Model: "gpt"},
			})
			return mr
		}(),
		wantPools: map[string]agentGatewayPicker{"qwen": {"epp", 9002}},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pools, reason, msg := agentGatewayResolvePoolPickers(tt.mr)
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q (message %q)", reason, tt.wantReason, msg)
			}
			if tt.wantMsg != "" && !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("message %q does not contain %q", msg, tt.wantMsg)
			}
			if tt.wantReason != "" {
				return
			}
			if len(pools) != len(tt.wantPools) {
				t.Fatalf("pools = %v, want %v", pools, tt.wantPools)
			}
			for pool, want := range tt.wantPools {
				if pools[pool] != want {
					t.Errorf("pool %s picker = %+v, want %+v", pool, pools[pool], want)
				}
			}
		})
	}
}
