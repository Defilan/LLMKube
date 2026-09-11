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

package agent

// Regression tests for #1769: the deleted-reference rail reported refs a
// diff never removed because branchDiffText handed git a base ref that was
// not the reviewer's base, so the diff spanned extra history.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestApplyDeletedReferenceRailForTask_NoRemovedRefsNoFlag(t *testing.T) {
	// A diff that modifies a line but removes no line citing a ref must not
	// record the flag. This is the direct unit half of #1769: the production
	// diff for that issue contained no removed line with a reference, yet the
	// rail reported 14 refs. The mechanism was that branchDiffText handed git
	// a base ref that was not the reviewer's base, so the diff spanned extra
	// history; this test pins the contract that a diff with no removed refs
	// produces no flag regardless of where those refs came from.
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{BaseBranch: "main"},
		},
	}
	loopRes := &LoopResult{Terminal: &ToolResult{}}

	// A diff whose removed line cites nothing.
	const diff = `diff --git a/foo.go b/foo.go
index 1234567..89abcde 100644
--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,3 @@
 package legacy

-// an old note with no reference at all
+// a new note with no reference at all
 const bufCap = 100
`
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && strings.Join(args, " ") == "diff main...HEAD" {
			return diff, nil
		}
		return "", context.Canceled
	}

	applyDeletedReferenceRailForTask(context.Background(), task, "/ws", loopRes, func(string) string { return "" })

	if loopRes.Terminal.Extra == nil {
		t.Fatalf("applyDeletedReferenceRailForTask did not ensure the extra map")
	}
	if _, ok := loopRes.Terminal.Extra["deletedIssueReferences"]; ok {
		t.Fatalf("deletedIssueReferences must be absent when no removed line cites a ref: %+v",
			loopRes.Terminal.Extra)
	}
	if _, ok := loopRes.Terminal.Extra["deletedReferenceNote"]; ok {
		t.Fatalf("deletedReferenceNote must be absent when no removed line cites a ref: %+v",
			loopRes.Terminal.Extra)
	}
}

// TestApplyDeletedReferenceRailForTask_DiffAgainstActualBase is the
// integration half of #1769: it drives the production path (git diff) on a
// real workspace and asserts the diff is computed against the ref the
// reviewer expects. Before the fix, branchDiffText used the raw base
// branch name, which resolves to a possibly-stale local ref when the
// reviewer's base lives elsewhere; the diff then spanned extra history and
// removed lines that the change itself never touched. The fix resolves the
// base to the upstream tip via repo.BaseBranchSHA and diffs against that
// literal SHA, so the diff is the change itself, not the change plus
// whatever drifted between the local ref and the reviewer's base.
//
// This test FAILs before the fix and PASSES after it: reverting the
// production change (deleted_reference_gate.go to diff against a resolved
// SHA instead of the branch name) makes the diff span the extra history and
// the rail records the ref from the seeded commit, which this test asserts
// must not appear.
func TestApplyDeletedReferenceRailForTask_DiffAgainstActualBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()

	// Seed a bare remote. The reviewer's base (commit A) has a file with a
	// line citing #123. The coder will branch from commit A.
	bare := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}
	seed := filepath.Join(root, "seed")
	if out, err := exec.Command("git", "clone", bare, seed).CombinedOutput(); err != nil {
		t.Fatalf("git clone seed: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(seed, "legacy.go"),
		[]byte("package legacy\n\n// This cap bounds the buffer. It exists because of #123.\nconst bufCap = 100\n"),
		0o644); err != nil {
		t.Fatalf("write legacy.go: %v", err)
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
	if out, err := exec.Command("git", "-C", seed, "push", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("seed push: %v: %s", err, out)
	}
	// Record the SHA of the reviewer's base (commit A).
	baseSHAB, _ := exec.Command("git", "-C", seed, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(baseSHAB))

	// Meanwhile, origin/main moves forward to commit B, which removes a line
	// citing #999. This is the extra history that should NOT be in the
	// coder's diff.
	seedExtra := filepath.Join(root, "seed-extra")
	if out, err := exec.Command("git", "clone", bare, seedExtra).CombinedOutput(); err != nil {
		t.Fatalf("git clone seed-extra: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(seedExtra, "legacy.go"),
		[]byte("package legacy\n\nconst bufCap = 100\n"), 0o644); err != nil {
		t.Fatalf("write legacy.go: %v", err)
	}
	for _, args := range [][]string{
		{"git", "-c", "user.email=extra@x", "-c", "user.name=extra", "add", "-A"},
		{"git", "-c", "user.email=extra@x", "-c", "user.name=extra", "commit", "-m", "remove #999"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = seedExtra
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "-C", seedExtra, "push", "origin",
		"main").CombinedOutput(); err != nil {
		t.Fatalf("push extra: %v: %s", err, out)
	}

	// The coder's workspace clones the fork and cuts a branch from commit A
	// (the reviewer's base). The coder then modifies a file in a way that
	// removes no line citing a ref: it changes the note but keeps #123.
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	upstreamURL := bare
	if out, err := exec.Command("git", "-C", ws, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "remote", "add",
		"origin", bare).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "fetch", upstreamURL,
		"main").CombinedOutput(); err != nil {
		t.Fatalf("git fetch: %v: %s", err, out)
	}
	// Create branch at the reviewer's base (commit A), not origin/main
	// (which now points at commit B).
	if out, err := exec.Command("git", "-C", ws, "checkout", "-B",
		"coder-branch", baseSHA).CombinedOutput(); err != nil {
		t.Fatalf("git checkout -B: %v: %s", err, out)
	}
	// Make a change that removes no line citing a ref: add a new constant
	// without touching the existing line that cites #123.
	newBody := "package legacy\n\n" +
		"// This cap bounds the buffer. It exists because of #123.\n" +
		"const bufCap = 100\n" +
		"const otherCap = 200\n"
	if err := os.WriteFile(filepath.Join(ws, "legacy.go"),
		[]byte(newBody), 0o644); err != nil {
		t.Fatalf("write legacy.go: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "-c", "user.email=coder@x",
		"-c", "user.name=coder", "commit", "-m", "fix: update the note").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	// Set up a fake cloneURLResolver that points at the bare remote. This
	// is necessary because the fix in applyDeletedReferenceRailForTask
	// resolves the upstream URL via cloneURLResolver and then calls
	// repo.BaseBranchSHA to fetch the base tip. Without this, the fix
	// degrades to the branch-name diff and the test passes even without the
	// fix.
	origResolver := cloneURLResolver
	t.Cleanup(func() { cloneURLResolver = origResolver })
	cloneURLResolver = &fakeCodeHost{url: bare}

	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{BaseBranch: "main", Repo: "defilantech/LLMKube"},
		},
	}
	loopRes := &LoopResult{Terminal: &ToolResult{}}

	// Before the fix, branchDiffText diffs against "main", which resolves
	// to commit B, so the diff spans the coder's commit plus the extra
	// history (commit B removes the line citing #999), and the rail records
	// #999. After the fix, the diff is computed against the upstream tip
	// (commit A, the reviewer's base) and the rail records nothing because
	// the coder's change removes no line citing a ref.
	applyDeletedReferenceRailForTask(context.Background(), task, ws, loopRes,
		func(string) string { return upstreamURL })

	if loopRes.Terminal.Extra == nil {
		t.Fatalf("applyDeletedReferenceRailForTask did not ensure the extra map")
	}
	if refs, ok := loopRes.Terminal.Extra["deletedIssueReferences"].([]string); ok && len(refs) > 0 {
		t.Fatalf("deletedIssueReferences must be empty when the coder's change removes no "+
			"line citing a ref, got %v; the diff must be computed against the reviewer's "+
			"base, not against a possibly-stale local ref", refs)
	}
}

// fakeCodeHost is a minimal CodeHost that returns a fixed URL from
// ResolveCloneURL. The test only needs ResolveCloneURL; the other methods
// are not exercised by the code path under test.
type fakeCodeHost struct {
	url string
}

func (f *fakeCodeHost) ResolveCloneURL(string) string { return f.url }

func (f *fakeCodeHost) EnsureChangeRequest(
	_ context.Context, _, _, _, _, _ string, _ bool,
) (string, bool, error) {
	return "", false, nil
}

func (f *fakeCodeHost) PullRequestUpdate(
	_ context.Context, _, _, _ string,
) (string, error) {
	return "", nil
}

func (f *fakeCodeHost) HeadCommitSubject(
	_ context.Context, _, _ string,
) (string, error) {
	return "", nil
}
