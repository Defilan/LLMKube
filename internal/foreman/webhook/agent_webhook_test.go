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

package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

// validAgent builds a baseline LLM-driven Agent that passes validation;
// each test mutates one field to exercise a single branch.
func validLLMAgent() *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleCoder,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "qwen"},
			SystemPrompt:        "You are a coder.",
			Tools:               []string{"read_file", "write_file", "submit_result"},
		},
	}
}

// validDeterministicAgent builds a baseline gate-shaped Agent (no
// InferenceServiceRef, no SystemPrompt, one non-terminal tool).
func validDeterministicAgent() *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gate", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:  foremanv1alpha1.AgentRoleVerifier,
			Tools: []string{"run_gate_job", "submit_result"},
		},
	}
}

func TestAgentValidator_Create(t *testing.T) {
	v := &AgentValidator{}
	ctx := context.Background()

	tests := []struct {
		name      string
		mutate    func(*foremanv1alpha1.Agent)
		wantError bool
	}{
		{
			name:      "valid LLM-driven agent accepted",
			mutate:    func(*foremanv1alpha1.Agent) {},
			wantError: false,
		},
		{
			name: "valid deterministic agent accepted",
			mutate: func(a *foremanv1alpha1.Agent) {
				*a = *validDeterministicAgent()
			},
			wantError: false,
		},
		{
			name: "LLM agent with empty systemPrompt rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				a.Spec.SystemPrompt = ""
			},
			wantError: true,
		},
		{
			name: "LLM agent with whitespace-only systemPrompt rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				a.Spec.SystemPrompt = "   \n\t "
			},
			wantError: true,
		},
		{
			name: "deterministic agent with systemPrompt rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				*a = *validDeterministicAgent()
				a.Spec.SystemPrompt = "should not be here"
			},
			wantError: true,
		},
		{
			name: "deterministic agent with only submit_result rejected (no usable tool)",
			mutate: func(a *foremanv1alpha1.Agent) {
				*a = *validDeterministicAgent()
				a.Spec.Tools = []string{"submit_result"}
			},
			wantError: true,
		},
		{
			name: "unknown tool name rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				a.Spec.Tools = []string{"read_file", "typo_tool", "submit_result"}
			},
			wantError: true,
		},
		{
			name: "cloud-proxy agent is never deterministic; empty isvc requires systemPrompt path NOT taken",
			mutate: func(a *foremanv1alpha1.Agent) {
				// Provider cloud-proxy with empty InferenceServiceRef is
				// LLM-driven (isDeterministicAgent == false), so a
				// non-empty systemPrompt is REQUIRED, not forbidden.
				a.Spec.Provider = foremanv1alpha1.AgentProviderCloudProxy
				a.Spec.InferenceServiceRef = corev1.LocalObjectReference{}
				a.Spec.SystemPrompt = "You are a cloud reviewer."
			},
			wantError: false,
		},
		{
			name: "cloud-proxy agent with empty systemPrompt rejected",
			mutate: func(a *foremanv1alpha1.Agent) {
				a.Spec.Provider = foremanv1alpha1.AgentProviderCloudProxy
				a.Spec.InferenceServiceRef = corev1.LocalObjectReference{}
				a.Spec.SystemPrompt = ""
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := validLLMAgent()
			tt.mutate(agent)
			_, err := v.ValidateCreate(ctx, agent)
			if tt.wantError && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tt.wantError && err != nil && !apierrors.IsInvalid(err) {
				t.Fatalf("expected an Invalid error, got %T: %v", err, err)
			}
		})
	}
}

// TestAgentValidator_Update reuses the create branches: update applies the
// same spec invariants.
func TestAgentValidator_Update(t *testing.T) {
	v := &AgentValidator{}
	ctx := context.Background()

	bad := validLLMAgent()
	bad.Spec.SystemPrompt = ""
	if _, err := v.ValidateUpdate(ctx, validLLMAgent(), bad); err == nil {
		t.Fatalf("expected update of an LLM agent with empty systemPrompt to fail")
	}

	good := validLLMAgent()
	if _, err := v.ValidateUpdate(ctx, validLLMAgent(), good); err != nil {
		t.Fatalf("expected update of a valid agent to pass, got %v", err)
	}
}

// TestDeterministicPredicateMatchesExecutor asserts the webhook's
// deterministic predicate stays aligned with the executor's: any tool set
// the webhook calls "usable for a deterministic agent" must be one the
// executor's pickDeterministicTool can dispatch, and vice versa.
func TestDeterministicPredicateMatchesExecutor(t *testing.T) {
	cases := []struct {
		toolNames  []string
		wantUsable bool
	}{
		{[]string{"run_gate_job", "submit_result"}, true},
		{[]string{"submit_result"}, false},
		{[]string{""}, false},
		{nil, false},
		{[]string{"read_file"}, true},
	}
	for _, c := range cases {
		if got := hasUsableDeterministicTool(c.toolNames); got != c.wantUsable {
			t.Errorf("hasUsableDeterministicTool(%v) = %v, want %v", c.toolNames, got, c.wantUsable)
		}
	}
}

// TestCanonicalToolNamesCoversWhitelist guards that every name the
// validator accepts is a real registered tool (and that the canonical set
// is non-empty so a registry refactor that empties it would fail loud).
func TestCanonicalToolNamesCoversWhitelist(t *testing.T) {
	names := tools.CanonicalToolNames()
	if len(names) == 0 {
		t.Fatalf("CanonicalToolNames returned empty; webhook would reject every tool")
	}
	wantSubset := []string{"read_file", "write_file", "submit_result", "run_gate_job"}
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for _, w := range wantSubset {
		if !have[w] {
			t.Errorf("canonical tool set missing expected tool %q (have %v)", w, names)
		}
	}
}
