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

// Wiring tests for the deleted-reference rail (#1553). The guard itself
// (recordDeletedIssueReferences + deletedIssueReferences) is already
// unit-tested in deleted_reference_gate_test.go, but those tests call the
// guard directly and would pass even if the rail were never invoked by the
// executor -- the exact defect this file exists to pin down. These tests
// instead drive the PRODUCTION path: Execute -> runLLMPath -> the GO-settle
// block where the sibling post-coding rails run, and assert the flag lands on
// the task's result extra. They fail if the call site in runLLMPath is
// removed (mutation check), which a direct-call test cannot catch.

import (
	"context"
	"encoding/json"
	"fmt"
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

// drRefRegistry is a fakeRegistry that, on submit_result, overwrites the
// seeded file with postBody (removing the line the test wants gone) and
// returns a GO terminal so the executor commits + pushes and reaches the
// GO-settle block where the deleted-reference rail runs.
type drRefRegistry struct {
	file      string
	postBody  string
	workspace string
}

func (r *drRefRegistry) Schemas() []oai.Tool { return nil }

func (r *drRefRegistry) Dispatch(
	_ context.Context, name string, _ json.RawMessage,
) (*foremanagent.ToolResult, error) {
	if name != "submit_result" {
		return nil, fmt.Errorf("drRefRegistry: unexpected tool %q", name)
	}
	if r.workspace != "" {
		_ = os.WriteFile(filepath.Join(r.workspace, r.file), []byte(r.postBody), 0o644)
	}
	return &foremanagent.ToolResult{
		Terminal:      true,
		Verdict:       "GO",
		Summary:       "removed a tracked line",
		CommitMessage: "fix: drop the line under test\n",
	}, nil
}

// drRefSeed builds a bare remote whose single commit (on main) contains one
// file at rel with body, so the executor's clone has a base to diff against
// and a committed line to remove. Mirrors initBareWithSeed.
func drRefSeed(t *testing.T, root, rel, body string) string {
	t.Helper()
	bare := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}
	seed := filepath.Join(root, "seed")
	if out, err := exec.Command("git", "clone", bare, seed).CombinedOutput(); err != nil {
		t.Fatalf("git clone seed: %v: %s", err, out)
	}
	p := filepath.Join(seed, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	for _, args := range [][]string{
		{"git", "-c", "user.email=seed@x", "-c", "user.name=seed", "add", "-A"},
		{"git", "-c", "user.email=seed@x", "-c", "user.name=seed", "commit", "-m", "seed"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = seed
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
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

// drRefExecute drives the full production coder path to a terminal Result:
// seed the base, clone, have the model (fakeRegistry) remove a line from the
// seeded file and submit GO, then commit + push + settle. The removed line is
// seedRel's body minus the line the test drops, so the base...HEAD diff the
// rail scans contains exactly that removed line.
func drRefExecute(t *testing.T, seedRel, seedBody, postBody string) *foremanagent.Result {
	t.Helper()
	gitOrSkip(t)
	root := t.TempDir()
	bare := drRefSeed(t, root, seedRel, seedBody)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})
	agent, task := taskAndAgent("dr-ref")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()
	reg := &drRefRegistry{file: seedRel, postBody: postBody}
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
	res, err := execWithAgent(t, e, task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// drRefModelExtra pulls the coder's terminal extra off the result; it is the
// map the deleted-reference rail records onto (goResult serializes it under
// Extra["modelExtra"]).
func drRefModelExtra(t *testing.T, res *foremanagent.Result) map[string]any {
	t.Helper()
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO (the rail runs on the GO-settle path) got %s; result=%+v",
			res.Verdict, res)
	}
	me, ok := res.Extra["modelExtra"].(map[string]any)
	if !ok {
		t.Fatalf("modelExtra missing or wrong type %T: %+v", res.Extra["modelExtra"], res.Extra)
	}
	return me
}

// The removed line cites #123, so the production path must record the
// flag: extra["deletedIssueReferences"] carries the ref and
// extra["deletedReferenceNote"] is a non-empty string. Without the call site
// in runLLMPath the terminal extra is empty and this fails.
func TestDeletedReferenceRail_WiredRecordsRemovedRefs(t *testing.T) {
	seed := "package legacy\n\n// This cap bounds the buffer. It exists because of #123.\nconst bufCap = 100\n"
	post := "package legacy\n\nconst bufCap = 100\n"
	res := drRefExecute(t, "legacy.go", seed, post)

	me := drRefModelExtra(t, res)
	refs, ok := me["deletedIssueReferences"].([]string)
	if !ok {
		t.Fatalf("deletedIssueReferences missing or wrong type %T: %+v",
			me["deletedIssueReferences"], me)
	}
	found := false
	for _, r := range refs {
		if r == "#123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deletedIssueReferences does not contain #123: %v", refs)
	}
	if note, _ := me["deletedReferenceNote"].(string); note == "" {
		t.Fatalf("deletedReferenceNote is empty; want a non-empty flag note: %+v", me)
	}
}

// The removed line cites nothing, so the flag must NOT appear: neither
// key is present on the terminal extra. This is the negative half that
// proves the rail is gated on the diff content, not just always-on.
func TestDeletedReferenceRail_WiredAbsentWhenNoRemovedRefs(t *testing.T) {
	seed := "package legacy\n\n// an old note with no reference at all\nconst bufCap = 100\n"
	post := "package legacy\n\nconst bufCap = 100\n"
	res := drRefExecute(t, "legacy.go", seed, post)

	me := drRefModelExtra(t, res)
	if _, ok := me["deletedIssueReferences"]; ok {
		t.Fatalf("deletedIssueReferences must be absent when no removed line cites a ref: %+v", me)
	}
	if _, ok := me["deletedReferenceNote"]; ok {
		t.Fatalf("deletedReferenceNote must be absent when no removed line cites a ref: %+v", me)
	}
}
