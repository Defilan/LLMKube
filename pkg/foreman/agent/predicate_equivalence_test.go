package agent

import (
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// TestConfigFloorDeterministicMatchesIsDeterministicAgent is the
// private-copy-plus-equivalence-test guard for the api package's
// isDeterministicSpec (#1613). The api package cannot import this package
// (the executor imports it for AgentSpec), so ConfigFloor keeps a private
// copy of the deterministic predicate — exactly the pattern the admission
// webhook uses for the same constraint.
//
// The api copy is private, so we assert behavioral equivalence through the
// public surface: for an empty-prompt verifier (a role with no required
// tools), ConfigFloor passes if and only if the executor would route the
// agent down the deterministic path. If someone edits isDeterministicSpec
// to diverge from IsDeterministicAgent, this table fails the moment the two
// disagree.
func TestConfigFloorDeterministicMatchesIsDeterministicAgent(t *testing.T) {
	cases := []struct {
		name string
		spec foremanv1alpha1.AgentSpec
		want bool
	}{
		{
			name: "empty spec (v0.1 shape) -\u003e deterministic",
			spec: foremanv1alpha1.AgentSpec{},
			want: true,
		},
		{
			name: "gate-shaped (local, no isvc) -\u003e deterministic",
			spec: foremanv1alpha1.AgentSpec{
				Provider: foremanv1alpha1.AgentProviderLocal,
			},
			want: true,
		},
		{
			name: "cloud-proxy provider -\u003e LLM (not deterministic)",
			spec: foremanv1alpha1.AgentSpec{
				Provider: foremanv1alpha1.AgentProviderCloudProxy,
			},
			want: false,
		},
		{
			name: "local provider with inferenceServiceRef set -\u003e LLM",
			spec: foremanv1alpha1.AgentSpec{
				Provider:            foremanv1alpha1.AgentProviderLocal,
				InferenceServiceRef: corev1.LocalObjectReference{Name: "svc"},
			},
			want: false,
		},
		{
			name: "local without inferenceServiceRef -\u003e deterministic",
			spec: foremanv1alpha1.AgentSpec{
				Provider: foremanv1alpha1.AgentProviderLocal,
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Verifier has no role-required tools, so ConfigFloor reduces
			// to the deterministic check alone: an empty-prompt verifier
			// passes iff the spec is deterministic.
			spec := tc.spec
			spec.Role = foremanv1alpha1.AgentRoleVerifier
			spec.SystemPrompt = ""

			want := IsDeterministicAgent(spec)
			if want != tc.want {
				t.Fatalf("test setup broken: IsDeterministicAgent(%+v) = %v, want %v", spec, want, tc.want)
			}

			ok, reason, _ := spec.ConfigFloor()
			if ok != tc.want {
				t.Fatalf("ConfigFloor() = %v/%q, want deterministic=%v (api copy drifted)", ok, reason, tc.want)
			}
		})
	}
}
