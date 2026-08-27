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
	"encoding/json"
	"os"
	"os/exec"
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

// csGit runs one git command in dir with a fixed identity, failing the test
// on error.
func csGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@example.com",
		"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// csPushNonEmptyBranch clones the bare origin into a temp workdir, adds one
// committed file on `branch` (so the branch is one commit ahead of main), and
// pushes it to origin. Returns the bare origin path unchanged.
func csPushNonEmptyBranch(t *testing.T, root, bare, branch string) {
	t.Helper()
	clone := filepath.Join(root, "coder-clone")
	csGit(t, root, "clone", bare, clone)
	csGit(t, clone, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(clone, "fix.go"), []byte("package fix\n"), 0o644); err != nil {
		t.Fatalf("write fix.go: %v", err)
	}
	csGit(t, clone, "add", ".")
	csGit(t, clone, "commit", "-m", "the real fix")
	csGit(t, clone, "push", "origin", branch)
}

// TestCrossStageContradiction_WiredIntoRunLLMPath drives the PRODUCTION path:
// Execute -> runLLMPath -> the reviewer block -> applyCrossStageContradictions
// ForTask -> contradictions + shouldEscalate. It builds a reviewer task whose
// branch is NON-empty (one real commit ahead of main) but whose terminal
// summary asserts the branch is empty -- the #1549 incident where a reviewer
// reported "no commits" on a branch that demonstrably had them. The detector
// must fire and record the contradiction on the terminal result.
//
// Mutation check: comment out the applyCrossStageContradictionsForTask call in
// runLLMPath and this test fails, because res.Extra["modelExtra"] no longer
// carries "crossStageContradictions". A test that called contradictions()
// directly would still pass with the call site removed -- this one cannot.
func TestCrossStageContradiction_WiredIntoRunLLMPath(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	const branch = "foreman/review-1"
	csPushNonEmptyBranch(t, root, bare, branch)
	oaiSrv := scriptedOAI(t, []string{submitGoBody}) // triggers the submit_result tool call

	// Reviewer-role agent: read-only, so the non-GO path (no commit/push) is
	// taken and the reviewer rails -- including the cross-stage check -- run.
	agent := &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer-cs", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleReviewer,
			Model:               "test-model",
			InferenceServiceRef: corev1.LocalObjectReference{Name: "test-svc"},
			SystemPrompt:        "you are a test reviewer",
			Tools:               []string{"read_file", "submit_result"},
			MaxTurns:            5,
		},
	}
	// Review task that ADOPTS the non-empty branch under review.
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-cs", Namespace: "default", UID: types.UID("review-cs-uid"),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindReview,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  1549,
				Branch: branch,
			},
			AgentRef: &corev1.LocalObjectReference{Name: agent.Name},
		},
	}

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(agent, task).Build()

	// The reviewer's terminal asserts an empty branch even though the ground
	// truth is one committed file. No findings: the empty-claim rail (#1552)
	// may remap the verdict, but the cross-stage contradiction is independent
	// and must be recorded either way.
	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"submit_result": {
				Terminal: true,
				Verdict:  "NO-GO",
				Summary:  "Branch has no commits and no code changes; nothing to review.",
				Extra:    map[string]any{},
			},
		},
	}

	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		CommitCommitter:          repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		RegistryFactory: func(
			_ context.Context, ws string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}

	res, err := execWithAgent(t, e, task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	me, ok := res.Extra["modelExtra"].(map[string]any)
	if !ok {
		t.Fatalf("modelExtra missing or wrong type: %T (%v)", res.Extra["modelExtra"], res.Extra)
	}
	cs, ok := me["crossStageContradictions"].([]string)
	if !ok || len(cs) == 0 {
		t.Fatalf("cross-stage contradiction was not recorded on the production path; "+
			"got crossStageContradictions=%v (extra=%v)", me["crossStageContradictions"], res.Extra)
	}
	want := "reviewer: claims empty branch but CommitsAhead=1"
	found := false
	for _, c := range cs {
		if c == want || strings.Contains(c, "claims empty branch") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected an empty-branch contradiction %q, got %v", want, cs)
	}

	// #1674: the evidence behind the contradiction must round-trip. The claim
	// and facts that were evaluated come back with the same field values that
	// went in, and the old crossStageContradictions key is left exactly as it
	// was (this only added a key, it did not replace one). The evidence is a
	// small flat struct; unmarshal the stored JSON into a local mirror of that
	// shape and assert on its fields. This proves both the round-trip and that
	// the JSON tags are the ones a downstream reader would key on.
	var stored crossStageEvidenceJSON
	raw, _ := json.Marshal(me["crossStageEvidence"])
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal crossStageEvidence: %v\nraw=%s", err, raw)
	}
	if stored.Claim.Stage != "reviewer" {
		t.Fatalf("evidence claim stage = %q, want reviewer", stored.Claim.Stage)
	}
	if !stored.Claim.ClaimsEmptyBranch {
		t.Fatalf("evidence claim claimsEmptyBranch = false, want true")
	}
	if stored.Claim.NamedFiles[0] != "fix.go" {
		t.Fatalf("evidence claim namedFiles = %v, want [fix.go]", stored.Claim.NamedFiles)
	}
	if stored.Facts.CommitsAhead != 1 {
		t.Fatalf("evidence facts commitsAhead = %d, want 1", stored.Facts.CommitsAhead)
	}
	if stored.Facts.FilesChanged[0] != "fix.go" {
		t.Fatalf("evidence facts filesChanged = %v, want [fix.go]", stored.Facts.FilesChanged)
	}
	if stored.Facts.HeadSHA == "" || stored.Facts.BaseSHA == "" {
		t.Fatalf("evidence facts missing git-resolved SHAs: %+v", stored.Facts)
	}
	if stored.Facts.HeadSHA == stored.Facts.BaseSHA {
		t.Fatalf("evidence facts headSHA == baseSHA %q on a one-commit branch", stored.Facts.HeadSHA)
	}
	if len(stored.Contradictions) != len(cs) {
		t.Fatalf("evidence contradictions len = %d, want %d", len(stored.Contradictions), len(cs))
	}
	for i := range cs {
		if stored.Contradictions[i] != cs[i] {
			t.Fatalf("evidence contradiction[%d] = %q, want %q", i, stored.Contradictions[i], cs[i])
		}
	}
}

// crossStageEvidenceJSON mirrors the JSON shape the production path records
// under Extra["crossStageEvidence"], so the external test package can decode it
// without naming the unexported types. Field order + tags must match
// cross_stage.go's crossStageEvidence.
type crossStageEvidenceJSON struct {
	Claim struct {
		Stage             string   `json:"stage"`
		Verdict           string   `json:"verdict"`
		ClaimsEdits       bool     `json:"claimsEdits"`
		ClaimsEmptyBranch bool     `json:"claimsEmptyBranch"`
		NamedFiles        []string `json:"namedFiles"`
	} `json:"claim"`
	Facts struct {
		CommitsAhead int      `json:"commitsAhead"`
		FilesChanged []string `json:"filesChanged"`
		NetLineDelta int      `json:"netLineDelta"`
		HeadSHA      string   `json:"headSHA"`
		BaseSHA      string   `json:"baseSHA"`
	} `json:"facts"`
	Contradictions []string `json:"contradictions"`
}
