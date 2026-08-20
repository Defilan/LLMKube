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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// validateAgentConfig is the config-floor check behind the Validated
// condition (#1609). A hand-created reviewer Agent ran for 3+ days with no
// systemPrompt and no fetch_issue tool: every verdict it produced came from
// an uninstructed model that structurally could not read the issue under
// review, and nothing anywhere said so.
func TestValidateAgentConfig(t *testing.T) {
	cases := []struct {
		name       string
		spec       foremanv1alpha1.AgentSpec
		wantValid  bool
		wantReason string
	}{
		{
			name: "reviewer with prompt and full tools is valid",
			spec: foremanv1alpha1.AgentSpec{
				Role:         "reviewer",
				SystemPrompt: "# reviewer instructions",
				Tools:        []string{"read_file", "grep", "bash", "fetch_issue", "submit_result"},
			},
			wantValid: true,
		},
		{
			// An LLM agent (endpoint set) still needs a prompt; the
			// NoSystemPrompt floor applies to the model path, not the
			// deterministic path (#1613).
			name: "empty systemPrompt fails for any role",
			spec: foremanv1alpha1.AgentSpec{
				Role:                "coder",
				Provider:            foremanv1alpha1.AgentProviderLocal,
				InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
				Tools:               []string{"read_file", "bash", "submit_result"},
			},
			wantValid:  false,
			wantReason: foremanv1alpha1.AgentConfigReasonNoSystemPrompt,
		},
		{
			name: "whitespace-only systemPrompt fails",
			spec: foremanv1alpha1.AgentSpec{
				Role:                "reviewer",
				Provider:            foremanv1alpha1.AgentProviderLocal,
				InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
				SystemPrompt:        "  \n\t ",
				Tools:               []string{"fetch_issue", "submit_result"},
			},
			wantValid:  false,
			wantReason: foremanv1alpha1.AgentConfigReasonNoSystemPrompt,
		},
		{
			name: "reviewer without fetch_issue fails",
			spec: foremanv1alpha1.AgentSpec{
				Role:         "reviewer",
				SystemPrompt: "# reviewer instructions",
				Tools:        []string{"read_file", "grep", "bash", "submit_result"},
			},
			wantValid:  false,
			wantReason: foremanv1alpha1.AgentConfigReasonMissingRoleTools,
		},
		{
			// LLM reviewer (endpoint set) missing both prompt and
			// fetch_issue: the prompt failure is reported first.
			name: "missing prompt reported before missing tools",
			spec: foremanv1alpha1.AgentSpec{
				Role:                "reviewer",
				Provider:            foremanv1alpha1.AgentProviderLocal,
				InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
				Tools:               []string{"bash", "submit_result"},
			},
			wantValid:  false,
			wantReason: foremanv1alpha1.AgentConfigReasonNoSystemPrompt,
		},
		{
			name: "coder does not need fetch_issue",
			spec: foremanv1alpha1.AgentSpec{
				Role:         "coder",
				SystemPrompt: "# coder instructions",
				Tools:        []string{"read_file", "write_file", "bash", "submit_result"},
			},
			wantValid: true,
		},
		{
			name: "verifier role has no tool floor",
			spec: foremanv1alpha1.AgentSpec{
				Role:         "verifier",
				SystemPrompt: "# gate",
				Tools:        []string{"run_gate_job"},
			},
			wantValid: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			valid, reason, msg := validateAgentConfig(&c.spec)
			if valid != c.wantValid {
				t.Fatalf("valid=%v want %v (reason=%q msg=%q)", valid, c.wantValid, reason, msg)
			}
			if !valid && reason != c.wantReason {
				t.Fatalf("reason=%q want %q", reason, c.wantReason)
			}
			if !valid && msg == "" {
				t.Fatal("an invalid result must carry a human-readable message")
			}
		})
	}
}

// The reconciler must publish the result as the reserved Validated condition.
func TestAgentReconcilerWritesValidatedCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := foremanv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// The incident: a hand-created LLM reviewer ran with no systemPrompt
	// and no fetch_issue, so every verdict it minted was uninstructed and
	// it could not read the issue under review (#1609). An LLM reviewer
	// (endpoint set), not a deterministic one, so the prompt requirement
	// applies (#1613).
	agent := &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "blind-reviewer", Namespace: "default", Generation: 3},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                "reviewer",
			Provider:            foremanv1alpha1.AgentProviderLocal,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
			Tools:               []string{"read_file", "grep", "bash", "submit_result"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(agent).WithStatusSubresource(agent).Build()
	r := &AgentReconciler{Client: cl, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "blind-reviewer", Namespace: "default"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got foremanv1alpha1.Agent
	if err := cl.Get(context.Background(),
		types.NamespacedName{Name: "blind-reviewer", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, conditionValidated)
	if cond == nil {
		t.Fatal("want a Validated condition, got none")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != foremanv1alpha1.AgentConfigReasonNoSystemPrompt {
		t.Fatalf("want False/%s, got %s/%s", foremanv1alpha1.AgentConfigReasonNoSystemPrompt, cond.Status, cond.Reason)
	}
	if got.Status.ObservedGeneration != 3 {
		t.Fatalf("want observedGeneration=3, got %d", got.Status.ObservedGeneration)
	}

	// Fixing the spec flips the condition on the next reconcile.
	got.Spec.SystemPrompt = "# instructions"
	got.Spec.Tools = append(got.Spec.Tools, "fetch_issue")
	if err := cl.Update(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "blind-reviewer", Namespace: "default"}}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := cl.Get(context.Background(),
		types.NamespacedName{Name: "blind-reviewer", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if cond = apimeta.FindStatusCondition(got.Status.Conditions, conditionValidated); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("want Validated=True after fix, got %+v", cond)
	}
}
