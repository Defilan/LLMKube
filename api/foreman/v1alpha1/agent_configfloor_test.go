package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The config floor is shared: the Agent controller publishes it as the
// Validated condition, and the executor stamps failing runs' task records
// (#1609). One implementation, so the two surfaces cannot drift.
func TestAgentSpecConfigFloor(t *testing.T) {
	complete := &AgentSpec{
		Role: AgentRoleReviewer, SystemPrompt: "# p",
		Tools: []string{"fetch_issue", "submit_result"},
	}
	ok, reason, _ := complete.ConfigFloor()
	if !ok || reason != AgentConfigReasonComplete {
		t.Fatalf("complete config must pass, got ok=%v reason=%q", ok, reason)
	}

	// LLM agent (endpoint set): an empty prompt must fail NoSystemPrompt.
	// The prompt requirement applies to the model path, not the
	// deterministic path (#1613).
	emptyPrompt := &AgentSpec{
		Role: AgentRoleReviewer, Provider: AgentProviderLocal,
		InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
		Tools:               []string{"fetch_issue", "submit_result"},
	}
	ok, reason, msg := emptyPrompt.ConfigFloor()
	if ok || reason != AgentConfigReasonNoSystemPrompt || msg == "" {
		t.Fatalf("empty prompt must fail with reason+message, got ok=%v reason=%q msg=%q", ok, reason, msg)
	}

	missingTool := &AgentSpec{
		Role: AgentRoleReviewer, SystemPrompt: "# p", Tools: []string{"bash", "submit_result"},
	}
	ok, reason, _ = missingTool.ConfigFloor()
	if ok || reason != AgentConfigReasonMissingRoleTools {
		t.Fatalf("reviewer without fetch_issue must fail, got ok=%v reason=%q", ok, reason)
	}
}

// TestAgentSpecConfigFloorDeterministic pins the #1613 fix: the floor skips
// the NoSystemPrompt requirement for a deterministic agent (no endpoint),
// because the executor routes it by endpoint absence and never renders a
// prompt. The role-required-tools check still applies, and an LLM-path
// agent (cloud-proxy) still needs a prompt even when its endpoint is empty.
func TestAgentSpecConfigFloorDeterministic(t *testing.T) {
	cases := []struct {
		name    string
		spec    *AgentSpec
		wantOK  bool
		wantWhy string
	}{
		{
			// Gate-shaped: no provider, no inferenceServiceRef, no prompt.
			// Deterministic, so the empty prompt is fine.
			name:    "gate-shaped (no provider, no isvc) passes with empty prompt",
			spec:    &AgentSpec{Role: AgentRoleVerifier, Tools: []string{"run_gate_job"}},
			wantOK:  true,
			wantWhy: AgentConfigReasonComplete,
		},
		{
			// Explicit local provider, empty endpoint: still deterministic.
			name:    "local provider, empty isvc, empty prompt passes",
			spec:    &AgentSpec{Role: AgentRoleVerifier, Provider: AgentProviderLocal, Tools: []string{"run_gate_job"}},
			wantOK:  true,
			wantWhy: AgentConfigReasonComplete,
		},
		{
			// cloud-proxy is never deterministic, so the prompt is required.
			name:    "cloud-proxy reviewer with empty prompt still fails NoSystemPrompt",
			spec:    &AgentSpec{Role: AgentRoleReviewer, Provider: AgentProviderCloudProxy, Tools: []string{"fetch_issue", "submit_result"}},
			wantOK:  false,
			wantWhy: AgentConfigReasonNoSystemPrompt,
		},
		{
			// Deterministic, but a reviewer without fetch_issue cannot ground
			// its verdicts: the role-required-tools check still applies.
			name:    "deterministic reviewer missing fetch_issue fails MissingRoleTools",
			spec:    &AgentSpec{Role: AgentRoleReviewer, Tools: []string{"submit_result"}},
			wantOK:  false,
			wantWhy: AgentConfigReasonMissingRoleTools,
		},
		{
			// Local provider WITH an endpoint is an LLM agent: it needs a
			// prompt even though its provider is local.
			name:    "local provider with isvc, empty prompt fails NoSystemPrompt",
			spec:    &AgentSpec{Role: AgentRoleVerifier, Provider: AgentProviderLocal, InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"}, Tools: []string{"run_gate_job"}},
			wantOK:  false,
			wantWhy: AgentConfigReasonNoSystemPrompt,
		},
		{
			// A whitespace-only prompt on a deterministic agent is irrelevant:
			// no prompt is rendered either way, and the floor must not fail.
			name:    "deterministic agent with whitespace-only prompt passes",
			spec:    &AgentSpec{Role: AgentRoleVerifier, SystemPrompt: "   ", Tools: []string{"run_gate_job"}},
			wantOK:  true,
			wantWhy: AgentConfigReasonComplete,
		},
		{
			// A whitespace-only prompt on an LLM agent is still "empty": the
			// deterministic carve-out must not accept it as a present prompt.
			name:    "local provider with isvc, whitespace-only prompt fails NoSystemPrompt",
			spec:    &AgentSpec{Role: AgentRoleVerifier, Provider: AgentProviderLocal, InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"}, SystemPrompt: "   ", Tools: []string{"run_gate_job"}},
			wantOK:  false,
			wantWhy: AgentConfigReasonNoSystemPrompt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason, msg := tc.spec.ConfigFloor()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if reason != tc.wantWhy {
				t.Fatalf("reason = %q, want %q (msg=%q)", reason, tc.wantWhy, msg)
			}
		})
	}
}
