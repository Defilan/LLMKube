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

package agent_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// TestCrossStageContradiction_GatePassEmptyBranch drives the PRODUCTION path:
// Execute -> executeDeterministic -> applyCrossStageContradictionsForGate ->
// contradictions + shouldEscalate. It builds a gate task whose branch carries
// no commits ahead of its base (the coder never pushed, so the branch is
// absent and the executor falls back to the seed commit identical to base) but
// whose gate reports GATE-PASS -- the #1674 incident where the checks passed
// trivially on an empty branch. The detector must fire and record the
// contradiction on the terminal result.
//
// Mutation check: comment out the applyCrossStageContradictionsForGate call in
// Execute and this test fails, because res.Extra no longer carries
// "crossStageContradictions". A test that called contradictions() directly
// would still pass with the call site removed -- this one cannot.
func TestCrossStageContradiction_GatePassEmptyBranch(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)

	// Gate Agent: no inferenceServiceRef, no systemPrompt, tools list names
	// the deterministic worker tool first. The executor dispatches that
	// tool directly without spinning up the OAI loop.
	agent := &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gate", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:               foremanv1alpha1.AgentRoleVerifier,
			Tools:              []string{"run_gate_job"},
			RequiredCapability: foremanv1alpha1.RequiredCapability{Roles: []string{"verifier"}},
		},
	}
	// Verify task that ADOPTS a coder branch the coder never pushed: the
	// branch is absent on the remote, so the executor falls back to the
	// seed commit identical to its base (an empty branch).
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gate", Namespace: "default", UID: types.UID("gate-uid"),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindVerify,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  1674,
				Branch: "foreman/review-1",
			},
			AgentRef: &corev1.LocalObjectReference{Name: agent.Name},
		},
	}

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(agent, task).Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			// Gate tool reports GATE-PASS on the empty branch.
			"run_gate_job": {
				Terminal: true,
				Verdict:  "GATE-PASS",
				Summary:  "all checks green",
				Output:   map[string]any{"jobName": "foreman-gate-fake-001"},
			},
		},
	}

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:             c,
		WorkspaceRoot:      filepath.Join(root, "ws"),
		GitRemoteURL:       bare,
		UpstreamURLForRepo: func(string) string { return bare },
		CommitAuthor:       repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		CommitCommitter:    repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}

	res, err := execWithAgent(t, e, task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdict("GATE-PASS") {
		t.Fatalf("verdict: want GATE-PASS got %s", res.Verdict)
	}

	cs, ok := res.Extra["crossStageContradictions"].([]string)
	if !ok || len(cs) == 0 {
		t.Fatalf("cross-stage contradiction was not recorded on the production path; "+
			"got crossStageContradictions=%v (extra=%v)", res.Extra["crossStageContradictions"], res.Extra)
	}
	want := "gate: GATE-PASS on an empty branch (checks passed trivially)"
	found := false
	for _, c := range cs {
		if c == want || strings.Contains(c, "GATE-PASS on an empty branch") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a GATE-PASS empty-branch contradiction %q, got %v", want, cs)
	}
}
