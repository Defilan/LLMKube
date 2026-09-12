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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
)

// Production-path test for the #1454 unverified-summary rail. runLLMPath runs
// enforceReviewerUnverifiedSummary inside the reviewer block: a GO whose
// terminal summary admits in plain language that verification could not be
// performed must come out of Execute as a NO-GO carrying the demotion markers
// inertDemotion (internal/foreman/controller) keys off.
//
// This drives the REAL rail through Execute -> runLLMPath -> the reviewer
// block (not a direct call to the helper), and asserts the markers on
// modelExtra. Mutation check: remove the enforceReviewerUnverifiedSummary
// call from runLLMPath and this test fails, because the verdict stays GO and
// none of the demotion markers ever reach modelExtra. A test that called
// enforceReviewerUnverifiedSummary directly would still pass with the call
// site removed -- this one cannot.
//
// The other rails are kept out of the picture by construction: the fetched
// issue body names no files, so the scope-overlap rail skips
// (no-path-refs-in-issue) instead of demoting; the submit envelope's issueAsk
// is a verbatim substring of that body, so reconcileReviewerIssueAsk marks it
// verified and the issueAsk rail returns without touching the verdict; and
// findings are empty, so the verdict-from-findings rail has nothing to
// promote. The unverified-summary rail is the only actor. The summary is the
// windowstead#321 incident that motivated the rail.

const submitGOUnverifiedSummaryArgs = `{"verdict":"GO","summary":"APPROVE. The verify job is disabled in this fleet, ` +
	`so I cannot verify goal reward or progression logic.",` +
	`"issueAsk":"The verify Job is disabled in this fleet"}`

// submitGOUnverifiedSummary mirrors a reviewer approval whose summary admits
// verification could not be performed.
var submitGOUnverifiedSummary = fmt.Sprintf(`{
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
          "arguments": %q
        }
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`,
	submitGOUnverifiedSummaryArgs)

func TestUnverifiedSummaryRail_WiredIntoRunLLMPath(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	const branch = "foreman/review-unverified"
	csPushNonEmptyBranch(t, root, bare, branch)
	oaiSrv := scriptedOAI(t, []string{fetchIssueBody, submitGOUnverifiedSummary})

	reg := &fakeRegistry{results: map[string]*foremanagent.ToolResult{
		"fetch_issue": {
			// No path refs, so the scope-overlap rail skips instead of
			// demoting.
			Output: map[string]any{"body": "## Bug\n\nThe verify Job is disabled " +
				"in this fleet, so runtime checks cannot run here."},
		},
		"submit_result": {
			Terminal: true,
			Verdict:  "GO",
			Summary:  "APPROVE. The verify job is disabled in this fleet, so I cannot verify goal reward or progression logic.",
			Extra: map[string]any{
				"reviewOutcome": "APPROVE",
				// Verbatim substring of the fetched body above, so
				// issueAsk is verified and its rail leaves the verdict alone.
				"issueAsk": "The verify Job is disabled in this fleet",
				"findings": []any{},
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
		t.Fatalf("final verdict: want NO-GO got %s (the unverified-summary rail must have demoted the GO)",
			res.Verdict)
	}

	me, ok := res.Extra["modelExtra"].(map[string]any)
	if !ok {
		t.Fatalf("modelExtra missing or wrong type: %T (%v)", res.Extra["modelExtra"], res.Extra)
	}
	if got := me["verdictDemoted"]; got != true {
		t.Fatalf("verdictDemoted: want true, got %v", got)
	}
	if got := me["verdictDemotedBy"]; got != reviewer.RailUnverifiedSummary {
		t.Fatalf("verdictDemotedBy: want %q got %v", reviewer.RailUnverifiedSummary, got)
	}
	if got := me["verdictClaimed"]; got != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Fatalf("verdictClaimed: want the original GO archived, got %v", got)
	}
	if phrase, _ := me["unverifiedSummaryPhrase"].(string); !strings.Contains(phrase, "cannot verify") {
		t.Fatalf("unverifiedSummaryPhrase: want the matched admission quoted, got %q", phrase)
	}
	if reason, _ := me["demotionReason"].(string); !strings.Contains(reason, `"cannot verify"`) {
		t.Fatalf("demotionReason: want the matched phrase quoted in the reason, got %q", reason)
	}
}
