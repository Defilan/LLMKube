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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// A loop that exhausts its turns (ErrMaxTurnsExhausted) with a dirty
// workspace used to discard the workspace entirely: both mapLoopError call
// sites returned early before the commit + push block, so the model's work
// died with the pod (#1715). This test drives a real Execute whose scripted
// model never calls submit_result (so the loop burns through MaxTurns) while
// its read_file tool writes a file into the workspace, then asserts the
// branch is pushed and the verdict is non-GO.

// loopExhaustBody is a chat-completions body whose tool call is read_file —
// a non-terminal tool — so the loop keeps running until MaxTurns.
const loopExhaustBody = `{
  "id": "t1",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-1",
        "type": "function",
        "function": {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

func TestNativeExecutor_LoopErrorPreservesDirtyWorkspace(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{loopExhaustBody})

	agent, task := taskAndAgent("loop-error-dirty")
	agent.Spec.MaxTurns = 2 // keep the test fast: burn through the budget quickly
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(agent, task).
		Build()

	// read_file returns a non-terminal result (the loop keeps running) and
	// writes a file into the workspace so the commit has something to add.
	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"read_file": {
				Terminal: false,
				Verdict:  "",
				Summary:  "read file",
			},
		},
		touch: func(name, ws string) {
			if name == "read_file" {
				_ = os.WriteFile(filepath.Join(ws, "loopwork.go"), []byte("package coder\n"), 0o644)
			}
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
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("verdict: want INCOMPLETE (non-GO) got %s; result=%+v", res.Verdict, res)
	}
	if res.FailureReason != foremanv1alpha1.FailureMaxTurnsExhausted {
		t.Fatalf("failureReason: want MaxTurnsExhausted got %q", res.FailureReason)
	}
	if got, _ := res.Extra["outcome"].(string); got != "LOOP-INCOMPLETE" {
		t.Fatalf("outcome: want LOOP-INCOMPLETE got %q", got)
	}
	// The dirty workspace must have been committed + pushed and recorded.
	if got := res.Extra["loopErrorBranch"]; got != "foreman/issue-9999" {
		t.Errorf("loopErrorBranch: want %q got %v", "foreman/issue-9999", got)
	}
	if got := res.Extra["commitSHA"]; got == nil || got == "" {
		t.Errorf("commitSHA missing in Extra: %+v", res.Extra)
	}
	// The branch must exist on the remote for a human to read.
	out, err := exec.Command("git", "-C", bare, "branch", "--list", "foreman/issue-9999").CombinedOutput()
	if err != nil {
		t.Fatalf("post-push branch list: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "foreman/issue-9999") {
		t.Errorf("branch %q not pushed to origin; git branch: %q", "foreman/issue-9999", out)
	}
}

// A loop that exhausts its turns with a CLEAN workspace must not error on
// nothing-to-commit: it keeps the original loop-error verdict (INCOMPLETE)
// rather than converting to NO-CHANGES, and records no pushed branch.
func TestNativeExecutor_LoopErrorCleanWorkspaceNoError(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{loopExhaustBody})

	agent, task := taskAndAgent("loop-error-clean")
	agent.Spec.MaxTurns = 2
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(agent, task).
		Build()

	// read_file is non-terminal and does NOT touch the workspace, so the
	// working tree stays clean when the loop exhausts its turns.
	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"read_file": {
				Terminal: false,
				Verdict:  "",
				Summary:  "read file",
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
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("verdict: want INCOMPLETE got %s; result=%+v", res.Verdict, res)
	}
	if res.FailureReason != foremanv1alpha1.FailureMaxTurnsExhausted {
		t.Fatalf("failureReason: want MaxTurnsExhausted got %q", res.FailureReason)
	}
	// Nothing to commit is the expected case here: no NO-CHANGES conversion,
	// and no loopErrorBranch recorded.
	if got, _ := res.Extra["outcome"].(string); got != "LOOP-INCOMPLETE" {
		t.Fatalf("outcome: want LOOP-INCOMPLETE got %q", got)
	}
	if _, ok := res.Extra["loopErrorBranch"]; ok {
		t.Errorf("loopErrorBranch recorded for a clean workspace; want none")
	}
	if _, ok := res.Extra["commitSHA"]; ok {
		t.Errorf("commitSHA recorded for a clean workspace; want none")
	}
}
