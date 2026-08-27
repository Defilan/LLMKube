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

// Production-path tests for the #1678 demotion-provenance guard. runLLMPath
// runs two demotion rails in sequence -- enforceReviewerScopeOverlap then
// enforceReviewerIssueAsk -- and BOTH wrote verdictClaimed unconditionally, so
// when the scope rail demoted GO->NO-GO and the issueAsk rail then ran on the
// already-demoted verdict, the second write clobbered the first and
// inertDemotion (internal/foreman/controller) could no longer tell a real
// GO->NO-GO rewrite from a rail that merely re-annotated.
//
// These drive the REAL rails through Execute -> runLLMPath -> the reviewer
// block (not a direct call to a helper), and assert the archived
// verdictClaimed that the controller keys off. They fail if the first-write
// guard is removed from either rail: TestScopeThenIssueAskVerdictClaimed must
// still read GO, and reverting the guard makes it read NO-GO.

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

const fetchIssueBody = `{
  "id": "t-fetch",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-fetch",
        "type": "function",
        "function": {"name": "fetch_issue", "arguments": "{\"repo\":\"defilantech/LLMKube\",\"number\":1678}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

// submitGOUnverified mirrors a reviewer approval whose submit_result carries
// no issueAsk quote. reconcileReviewerIssueAsk then fills issueAsk from the
// fetched body and marks it unverified, which is what lets the issueAsk rail
// run on the scope rail's demoted verdict in TestScopeThenIssueAskVerdictClaimed.
const submitGOUnverified = `{
  "id": "t-submit",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-submit",
        "type": "function",
        "function": {
          "name": "submit_result",
          "arguments": "{\"verdict\":\"GO\",\"summary\":\"APPROVE: the change is minimal and well covered\"}"
        }
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

// submitGOVerified mirrors a reviewer approval whose issueAsk quote is a
// verbatim substring of the fetched body, so reconcileReviewerIssueAsk marks
// it verified and the issueAsk rail returns without touching verdictClaimed.
const submitGOVerified = `{
  "id": "t-submit",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-submit",
        "type": "function",
        "function": {"name": "submit_result", "arguments": "{\"verdict\":\"GO\",\"summary\":\"ok\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

// reviewExecutor assembles a reviewer executor wired to reg, whose loop
// dispatches fetch_issue + submit_result through it. The reviewer never edits
// the workspace, so the non-GO/no-commit reviewer path runs and the reviewer
// rails execute.
func reviewExecutor(
	t *testing.T,
	bare, branch string,
	reg *fakeRegistry,
	oaiSrv *httptest.Server,
) *foremanagent.NativeAgentLoopExecutor {
	t.Helper()
	agent := &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer-demo", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleReviewer,
			Model:               "test-model",
			InferenceServiceRef: corev1.LocalObjectReference{Name: "test-svc"},
			SystemPrompt:        "you are a test reviewer",
			Tools:               []string{"fetch_issue", "submit_result"},
			MaxTurns:            5,
		},
	}
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-demo", Namespace: "default", UID: types.UID("review-demo-uid"),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  1678,
				Branch: branch,
			},
			AgentRef: &corev1.LocalObjectReference{Name: agent.Name},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()
	return &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(t.TempDir(), "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		CommitCommitter:          repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}
}

// TestScopeThenIssueAskVerdictClaimed is the #1678 regression. The branch
// touches fix.go, but the issue body names other.go, so the scope rail detects
// drift and demotes GO->NO-GO, archiving verdictClaimed=GO. The submit
// envelope carries no issueAsk quote, so reconcileReviewerIssueAsk marks the
// ask unverified and the issueAsk rail then runs on the already-demoted
// verdict. The archived verdictClaimed must still be GO, not the demoted
// NO-GO that last-writer-wins would have written.
func TestScopeThenIssueAskVerdictClaimed(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	const branch = "foreman/review-demo"
	// csPushNonEmptyBranch pushes fix.go, which the issue body does NOT name,
	// so the scope rail sees drift.
	csPushNonEmptyBranch(t, root, bare, branch)
	oaiSrv := scriptedOAI(t, []string{fetchIssueBody, submitGOUnverified})

	reg := &fakeRegistry{results: map[string]*foremanagent.ToolResult{
		"fetch_issue": {
			// A body that names other.go, so the scope rail extracts a ref
			// the diff never touches.
			Output: map[string]any{"body": "## Bug\n\nEdit other.go to resolve the issue."},
		},
		"submit_result": {
			Terminal: true,
			Verdict:  "GO",
			Summary:  "APPROVE: the change is minimal and well covered",
			Extra: map[string]any{
				"reviewOutcome": "APPROVE",
				"findings":      []any{},
			},
		},
	}}
	e := reviewExecutor(t, bare, branch, reg, oaiSrv)

	res, err := execWithAgent(t, e, &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "review-demo", Namespace: "default", UID: types.UID("review-demo-uid")},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  1678,
				Branch: branch,
			},
			AgentRef: &corev1.LocalObjectReference{Name: "reviewer-demo"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("final verdict: want NO-GO got %s", res.Verdict)
	}

	me, ok := res.Extra["modelExtra"].(map[string]any)
	if !ok {
		t.Fatalf("modelExtra missing or wrong type: %T (%v)", res.Extra["modelExtra"], res.Extra)
	}
	if got, want := me["verdictClaimed"], string(foremanv1alpha1.AgenticTaskVerdictGo); got != want {
		t.Fatalf("verdictClaimed after both rails: got %v, want %v (the scope rail must own the archive)",
			got, want)
	}
}

// TestScopeRailAloneArchivesOriginalVerdict is the single-rail regression.
// The issueAsk quote is a verbatim substring of the fetched body, so
// reconcileReviewerIssueAsk marks it verified and the issueAsk rail returns
// early; only the scope rail demotes. The archived verdictClaimed must be the
// original GO and verdictDemotedBy must name the scope rail.
func TestScopeRailAloneArchivesOriginalVerdict(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	const branch = "foreman/review-solo"
	csPushNonEmptyBranch(t, root, bare, branch)
	oaiSrv := scriptedOAI(t, []string{fetchIssueBody, submitGOVerified})

	reg := &fakeRegistry{results: map[string]*foremanagent.ToolResult{
		"fetch_issue": {
			Output: map[string]any{"body": "## Bug\n\nEdit other.go to resolve the issue."},
		},
		"submit_result": {
			Terminal: true,
			Verdict:  "GO",
			Summary:  "APPROVE: the change is minimal and well covered",
			Extra: map[string]any{
				"reviewOutcome": "APPROVE",
				"issueAsk":      "Edit other.go to resolve the issue.",
				"findings":      []any{},
			},
		},
	}}
	e := reviewExecutor(t, bare, branch, reg, oaiSrv)

	res, err := execWithAgent(t, e, &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "review-demo", Namespace: "default", UID: types.UID("review-demo-uid")},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  1678,
				Branch: branch,
			},
			AgentRef: &corev1.LocalObjectReference{Name: "reviewer-demo"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	me, ok := res.Extra["modelExtra"].(map[string]any)
	if !ok {
		t.Fatalf("modelExtra missing or wrong type: %T (%v)", res.Extra["modelExtra"], res.Extra)
	}
	if got, want := me["verdictClaimed"], string(foremanv1alpha1.AgenticTaskVerdictGo); got != want {
		t.Fatalf("verdictClaimed after the scope rail: got %v, want %v", got, want)
	}
	if got := me["verdictDemotedBy"].(string); got != "scope-overlap" {
		t.Fatalf("verdictDemotedBy: want scope-overlap got %q", got)
	}
}
