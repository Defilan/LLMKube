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

// Internal tests for the coder-prompt half of the #1554 per-clause rail.
// buildUserPrompt, clauseChecklist and extractClauses are unexported, so these
// live in package agent rather than agent_test.
package agent

import (
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func clausePromptTask(body string) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  1554,
				Prompt: body,
			},
		},
	}
}

// TestBuildUserPrompt_ClauseChecklistIsInjected guards the prompt-side half of
// the rail: buildUserPrompt must emit the clause checklist block.
//
// It asserts on the block's header and on the checklist RENDERING, never on the
// clause text alone. That distinction is the whole point. buildUserPrompt
// already pastes the entire issue body verbatim under "Issue context:", so a
// test that merely looked for the clause text would be satisfied by the pasted
// body and would keep passing with the checklist deleted. Deleting the
// clauseChecklist call in buildUserPrompt must fail this test.
func TestBuildUserPrompt_ClauseChecklistIsInjected(t *testing.T) {
	body := "## Expected Behavior\n\n" +
		"- the enabled path applies the new default\n" +
		"- the disabled path preserves the original no-side-effect behaviour\n"
	got := buildUserPrompt(clausePromptTask(body))

	if !strings.Contains(got, "Required behaviours to cover:") {
		t.Fatalf("clause checklist block missing from the coder prompt:\n%s", got)
	}
	want := clauseChecklist(extractClauses(body))
	if want == "" {
		t.Fatal("fixture produced no clauses; this test would be vacuous")
	}
	if !strings.Contains(got, want) {
		t.Errorf("expected the rendered checklist %q in the prompt:\n%s", want, got)
	}
	if strings.Index(got, "Required behaviours to cover:") < strings.Index(got, "Issue context:") {
		t.Error("checklist block should follow the issue context, not precede it")
	}
}

// TestBuildUserPrompt_NoClausesOmitsChecklist is the negative direction: an
// issue with only prose must not gain a checklist block, so a clause-less issue
// degrades to a no-op rather than an empty header.
func TestBuildUserPrompt_NoClausesOmitsChecklist(t *testing.T) {
	task := clausePromptTask("Some prose describing the problem with no enumerated sections.")
	if got := buildUserPrompt(task); strings.Contains(got, "Required behaviours to cover:") {
		t.Errorf("clause-less issue must not gain a checklist block:\n%s", got)
	}
}
