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

// Wiring test for the pre-flight staleness check (#1550). The guard itself
// (checkStaleness + the pure helpers) is already unit-tested in
// staleness_check_test.go, but those tests call the guard directly and would
// pass even if the rail were never invoked by the executor -- the exact defect
// this file exists to pin down. This test instead drives the PRODUCTION path:
// Execute -> runLLMPath -> the pre-dispatch prompt assembly, and asserts the
// staleness note lands in the coder's first OAI user message. It fails if the
// call site in runLLMPath is removed (mutation check), which a direct-call
// test cannot catch.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// staleRefRegistry is a fakeRegistry that records the workspace path so the
// test can seed a commit citing the issue number into the task branch.
type staleRefRegistry struct {
	workspace string
}

func (r *staleRefRegistry) Schemas() []oai.Tool { return nil }

func (r *staleRefRegistry) Dispatch(
	_ context.Context, name string, _ json.RawMessage,
) (*foremanagent.ToolResult, error) {
	if name != "submit_result" {
		return nil, &unknownTool{name}
	}
	return &foremanagent.ToolResult{
		Terminal:      true,
		Verdict:       "GO",
		Summary:       "fixed",
		CommitMessage: "fix: trivial change\n",
	}, nil
}

// unknownTool is a minimal error type so the fake registry can report an
// unexpected tool without importing errors just for this helper.
type unknownTool struct{ name string }

func (e *unknownTool) Error() string { return "unknown tool " + e.name }

// staleRefSeed builds a bare remote whose single commit (on main) has a
// subject line citing the issue number, so the executor's clone has a base
// whose `git log --grep` matches. Mirrors drRefSeed in the deleted-reference
// wiring test.
func staleRefSeed(t *testing.T, root, issue string) string {
	t.Helper()
	bare := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}
	seed := filepath.Join(root, "seed")
	if out, err := exec.Command("git", "clone", bare, seed).CombinedOutput(); err != nil {
		t.Fatalf("git clone seed: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	for _, args := range [][]string{
		{"git", "-c", "user.email=seed@x", "-c", "user.name=seed", "add", "README.md"},
		{"git", "-c", "user.email=seed@x", "-c", "user.name=seed", "commit", "-m", "seed"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = seed
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	// A second commit whose subject cites the issue so `git log --oneline
	// --grep=#N` on the base branch matches. The executor cuts the task
	// branch from the base, so the fix commit must already be on the base
	// branch (pushed as main), not added to the task branch.
	if out, err := exec.Command("git", "-C", seed, "-c", "user.email=seed@x", "-c", "user.name=seed",
		"commit", "--allow-empty", "-m", "fix: resolve issue #"+issue).CombinedOutput(); err != nil {
		t.Fatalf("seed cite commit: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", seed, "push", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("seed cite push: %v: %s", err, out)
	}
	cur, _ := exec.Command("git", "-C", seed, "branch", "--show-current").Output()
	if strings.TrimSpace(string(cur)) != "main" {
		cmd := exec.Command("git", "-C", seed, "branch", "-M", strings.TrimSpace(string(cur)), "main")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rename main: %v: %s", err, out)
		}
	}
	if out, err := exec.Command("git", "-C", seed, "push", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("seed push: %v: %s", err, out)
	}
	return bare
}

// TestStalenessCheck_WiredNotesCoderPrompt drives the real production coder
// path and asserts the staleness note lands in the coder's first OAI request.
// The seed commit cites the task's issue number, so the base-branch `git log
// --grep` matches and the check returns a non-empty note. Without the call
// site in runLLMPath the first request carries no such note and this fails.
func TestStalenessCheck_WiredNotesCoderPrompt(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	issue := "9999"
	bare := staleRefSeed(t, root, issue)

	var captured []string
	oaiSrv := recordingOAI(t, []string{submitGoBody}, &captured)

	agent, task := taskAndAgent("staleness")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	reg := &staleRefRegistry{}
	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiSrv.URL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Bot", Email: "b@x"},
		CommitCommitter:          repo.Identity{Name: "Bot", Email: "b@x"},
		RegistryFactory: func(
			_ context.Context, ws string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
	}

	if _, err := execWithAgent(t, e, task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(captured) == 0 {
		t.Fatal("no OAI requests captured")
	}
	first := captured[0]
	if !strings.Contains(first, "#"+issue) {
		t.Fatalf("first OAI request missing the staleness note citing #%s: %q", issue, truncForTest(first))
	}
	if !strings.Contains(first, "Staleness pre-flight") {
		t.Fatalf("first OAI request missing the staleness pre-flight note: %q", truncForTest(first))
	}
}
