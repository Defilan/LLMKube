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

// Regression tests for #1769. The coder GO-settle rails diffed HEAD against
// the workspace's LOCAL base ref. The workspace is a fork clone and the task
// branch is cut from the CURRENT upstream tip (#813), so when the fork's main
// lags upstream the diff swept in the whole intervening upstream delta and
// the rails judged changes the coder never made: the deleted-reference rail
// reported refs a diff never removed, the grounding rail flagged identifiers
// the coder never wrote, and the no-functional-change rail missed docs-only
// GOs behind upstream's functional delta. These tests reproduce that shape
// end to end and pin all three rails to the resolved upstream base SHA.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// rail1769Git runs one git command in dir and fails the test on error.
func rail1769Git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// rail1769Seed builds the two-remote shape behind #1769. upstream.git
// advances from commit A (legacy.go carries a line citing #999) to commit B
// (an upstream merge removed that line and added deltaFiles). fork.git
// receives commit A only, so a clone of the fork carries a local main that
// lags the upstream tip the task branch is cut from.
//
// The bare repos are reached through the production-shaped URLs via
// url.insteadOf under a test-scoped HOME: the executor must take the FORK
// clone path (Execute clones GitRemoteURL when it is a fork of payload.repo,
// #915), which is the staleness the rails must anchor against. Returns the
// fork and upstream URLs.
func rail1769Seed(t *testing.T, root string, deltaFiles map[string]string) (forkURL, upstreamURL string) {
	t.Helper()
	const (
		forkRemote     = "https://github.com/Defilan/LLMKube.git"
		upstreamRemote = "https://github.com/defilantech/LLMKube.git"
	)
	upstream := filepath.Join(root, "upstream.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", upstream).CombinedOutput(); err != nil {
		t.Fatalf("git init bare upstream: %v: %s", err, out)
	}
	u1 := filepath.Join(root, "seed-a")
	if out, err := exec.Command("git", "clone", upstream, u1).CombinedOutput(); err != nil {
		t.Fatalf("clone seed-a: %v: %s", err, out)
	}
	seedBody := "package legacy\n\n// This cap bounds the buffer. It exists because of #999.\nconst bufCap = 100\n"
	if err := os.WriteFile(filepath.Join(u1, "legacy.go"), []byte(seedBody), 0o644); err != nil {
		t.Fatalf("write legacy.go: %v", err)
	}
	rail1769Git(t, u1, "-c", "user.email=seed@x", "-c", "user.name=seed", "add", "-A")
	rail1769Git(t, u1, "-c", "user.email=seed@x", "-c", "user.name=seed",
		"commit", "-m", "seed: the buffer cap exists because of #999")
	rail1769Git(t, u1, "push", "origin", "main")

	fork := filepath.Join(root, "fork.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", fork).CombinedOutput(); err != nil {
		t.Fatalf("git init bare fork: %v: %s", err, out)
	}
	// The fork mirrors commit A and then stops syncing.
	rail1769Git(t, u1, "push", fork, "main")

	// Upstream advances: the #999-citing line is removed (merged upstream
	// work) and the delta files land. The fork never sees this commit.
	u2 := filepath.Join(root, "seed-b")
	if out, err := exec.Command("git", "clone", upstream, u2).CombinedOutput(); err != nil {
		t.Fatalf("clone seed-b: %v: %s", err, out)
	}
	upBody := "package legacy\n\n// a note without a reference\nconst bufCap = 100\n"
	if err := os.WriteFile(filepath.Join(u2, "legacy.go"), []byte(upBody), 0o644); err != nil {
		t.Fatalf("write legacy.go: %v", err)
	}
	for rel, body := range deltaFiles {
		p := filepath.Join(u2, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	rail1769Git(t, u2, "-c", "user.email=up@x", "-c", "user.name=up", "add", "-A")
	rail1769Git(t, u2, "-c", "user.email=up@x", "-c", "user.name=up", "commit", "-m", "upstream: drop the stale #999 note")
	rail1769Git(t, u2, "push", "origin", "main")

	// Map the production URLs onto the local bares for every git call the
	// executor makes (it threads HOME through runGit).
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	cfg := "[url \"" + fork + "\"]\n\tinsteadOf = " + forkRemote + "\n" +
		"[url \"" + upstream + "\"]\n\tinsteadOf = " + upstreamRemote + "\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write .gitconfig: %v", err)
	}
	t.Setenv("HOME", home)
	return forkRemote, upstreamRemote
}

// rail1769Context7QueryBody is the first scripted OAI response when a test
// needs grounding evidence: the model queries context7 before submitting, so
// the transcript carries the evidence the grounding rail reads.
const rail1769Context7QueryBody = `{
  "id": "t0",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-0",
        "type": "function",
        "function": {"name": "mcp__context7__query-docs", "arguments": "{}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

// rail1769Registry is the fake tool registry: on submit_result it overwrites
// legacy.go with postBody (a change that removes nothing citing a ref) and
// returns a GO terminal, so the executor commits, pushes, and reaches the
// GO-settle block where the three rails run. When context7Doc is non-empty it
// also answers one mcp__context7__query-docs dispatch so the grounding rail
// sees evidence in the transcript.
type rail1769Registry struct {
	file        string
	postBody    string
	workspace   string
	context7Doc string
}

func (r *rail1769Registry) Schemas() []oai.Tool { return nil }

func (r *rail1769Registry) Dispatch(
	_ context.Context, name string, _ json.RawMessage,
) (*foremanagent.ToolResult, error) {
	switch name {
	case "mcp__context7__query-docs":
		return &foremanagent.ToolResult{Output: r.context7Doc}, nil
	case "submit_result":
		if r.workspace != "" {
			_ = os.WriteFile(filepath.Join(r.workspace, r.file), []byte(r.postBody), 0o644)
		}
		return &foremanagent.ToolResult{
			Terminal:      true,
			Verdict:       "GO",
			Summary:       "edited a comment line",
			CommitMessage: "fix: reword the note\n",
		}, nil
	default:
		return nil, fmt.Errorf("rail1769Registry: unexpected tool %q", name)
	}
}

// rail1769Execute drives the full production coder path over the two-remote
// shape: clone the fork, cut the branch from the current upstream tip, submit
// a GO whose only edit is postBody into legacy.go. A non-empty context7Doc
// scripts one context7 query before the submit.
func rail1769Execute(
	t *testing.T, postBody string, deltaFiles map[string]string, context7Doc string,
) *foremanagent.Result {
	t.Helper()
	gitOrSkip(t)
	root := t.TempDir()
	fork, upstream := rail1769Seed(t, root, deltaFiles)
	bodies := []string{submitGoBody}
	if context7Doc != "" {
		bodies = []string{rail1769Context7QueryBody, submitGoBody}
	}
	oaiSrv := scriptedOAI(t, bodies)
	agent, task := taskAndAgent("rail-1769")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()
	reg := &rail1769Registry{file: "legacy.go", postBody: postBody, context7Doc: context7Doc}
	e := &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             fork,
		UpstreamURLForRepo:       func(string) string { return upstream },
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

// rail1769ModelExtra pulls the coder's terminal extra off the result; it is
// the map the GO-settle rails record onto (goResult serializes it under
// Extra["modelExtra"]).
func rail1769ModelExtra(t *testing.T, res *foremanagent.Result) map[string]any {
	t.Helper()
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO (the rails run on the GO-settle path) got %s; result=%+v",
			res.Verdict, res)
	}
	me, ok := res.Extra["modelExtra"].(map[string]any)
	if !ok {
		t.Fatalf("modelExtra missing or wrong type %T: %+v", res.Extra["modelExtra"], res.Extra)
	}
	return me
}

// The direct #1769 falsifier: the coder's change removes no line citing a
// ref, so the deleted-reference rail must stay quiet even though the fork's
// stale local main diff sweeps in the upstream commit that removed the #999
// line. Before the fix this recorded deletedIssueReferences = ["#999"].
func TestDeletedReferenceRail_AnchoredToUpstreamBase_NoPhantomRefs(t *testing.T) {
	post := "package legacy\n\n// a different note, still citing nothing\nconst bufCap = 100\n"
	res := rail1769Execute(t, post, nil, "")

	me := rail1769ModelExtra(t, res)
	if _, ok := me["deletedIssueReferences"]; ok {
		t.Fatalf("deletedIssueReferences must be absent when the coder's change removes "+
			"no line citing a ref; the diff must be anchored to the upstream base, got %+v", me)
	}
	if _, ok := me["deletedReferenceNote"]; ok {
		t.Fatalf("deletedReferenceNote must be absent when the coder's change removes "+
			"no line citing a ref; got %+v", me)
	}
}

// A docs-only coder GO behind an upstream delta that carries functional code
// must still be flagged: before the fix the swept delta made the diff look
// functional and the advisory was silently absent.
func TestNoFunctionalChangeRail_AnchoredToUpstreamBase_StillFlagsDocsOnlyGO(t *testing.T) {
	post := "package legacy\n\n// a different note, still citing nothing\nconst bufCap = 100\n"
	delta := map[string]string{"delta.go": "package legacy\n\nfunc helper() {}\n"}
	res := rail1769Execute(t, post, delta, "")

	me := rail1769ModelExtra(t, res)
	if flagged, _ := me["noFunctionalChange"].(bool); !flagged {
		t.Fatalf("noFunctionalChange must be true for a comment-only coder GO even when "+
			"upstream's intervening delta carries functional code; got %+v", me)
	}
}

// An identifier added by an upstream merge must not be judged as the coder's
// writing: before the fix the swept delta line produced a phantom grounding
// violation against a coder who never wrote it.
func TestCoderGroundingRail_AnchoredToUpstreamBase_NoPhantomViolations(t *testing.T) {
	post := "package legacy\n\n// a different note, still citing nothing\nconst bufCap = 100\n"
	delta := map[string]string{"delta.go": "package legacy\n\nvar _ = rate(vllm:request_failure_total[5m])\n"}
	res := rail1769Execute(t, post, delta, "vllm:request_success_total{finished_reason}")

	me := rail1769ModelExtra(t, res)
	if _, ok := me["coderGroundingViolations"]; ok {
		t.Fatalf("coderGroundingViolations must be absent: the upstream delta line the "+
			"stale local-main diff sweeps in is not the coder's writing; got %+v", me)
	}
}
