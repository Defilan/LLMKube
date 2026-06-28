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

package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOut runs git in dir and returns trimmed stdout, failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes a file in work, commits it on the current branch, and
// pushes to origin/main. Returns the new commit SHA.
func commitFile(t *testing.T, work, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mustGit(t, work, "-c", "user.email=u@x", "-c", "user.name=u", "add", name)
	mustGit(t, work, "-c", "user.email=u@x", "-c", "user.name=u", "commit", "-m", "add "+name)
	mustGit(t, work, "push", "origin", "main")
	return gitOut(t, work, "rev-parse", "HEAD")
}

// TestCreateBranchFromUpstream verifies the task branch is cut from the
// CURRENT upstream tip, not the (stale) cloned fork's HEAD (#813).
func TestCreateBranchFromUpstream(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()

	// Upstream: bare repo, seeded (commit A), then advanced to commit B.
	bareUpstream := initBareOrigin(t, filepath.Join(dir, "up"))
	seedOrigin(t, bareUpstream)
	upWork := filepath.Join(dir, "up-work")
	mustGit(t, "", "clone", bareUpstream, upWork)
	upstreamTip := commitFile(t, upWork, "UPSTREAM.md", "upstream B\n")

	// Fork: an independent bare with its own (stale) commit; clone it to stand
	// in for the executor's freshly-cloned fork workspace.
	bareFork := initBareOrigin(t, filepath.Join(dir, "fork"))
	seedOrigin(t, bareFork)
	workspace := filepath.Join(dir, "workspace")
	mustGit(t, "", "clone", bareFork, workspace)
	forkTip := gitOut(t, workspace, "rev-parse", "HEAD")

	if upstreamTip == forkTip {
		t.Fatal("test setup: upstream and fork tips should differ")
	}

	if err := CreateBranchFromUpstream(context.Background(), UpstreamBranchOptions{
		Workspace:   workspace,
		Branch:      "foreman/issue-813-test",
		UpstreamURL: bareUpstream,
		BaseBranch:  "main",
	}); err != nil {
		t.Fatalf("CreateBranchFromUpstream: %v", err)
	}

	if cur := gitOut(t, workspace, "branch", "--show-current"); cur != "foreman/issue-813-test" {
		t.Errorf("current branch = %q, want foreman/issue-813-test", cur)
	}
	if got := gitOut(t, workspace, "rev-parse", "HEAD"); got != upstreamTip {
		t.Errorf("branch HEAD = %s, want upstream tip %s (stale fork tip was %s)", got, upstreamTip, forkTip)
	}
	// The upstream-only file proves the branch is based on upstream B.
	if _, err := os.Stat(filepath.Join(workspace, "UPSTREAM.md")); err != nil {
		t.Errorf("UPSTREAM.md should exist on the upstream-based branch: %v", err)
	}
}

// TestCreateBranchFromUpstream_DefaultsBaseToMain verifies an empty BaseBranch
// defaults to "main".
func TestCreateBranchFromUpstream_DefaultsBaseToMain(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	bareUpstream := initBareOrigin(t, filepath.Join(dir, "up"))
	seedOrigin(t, bareUpstream)
	upTip := gitOut(t, mustClone(t, bareUpstream, filepath.Join(dir, "peek")), "rev-parse", "HEAD")

	workspace := mustClone(t, bareUpstream, filepath.Join(dir, "workspace"))
	if err := CreateBranchFromUpstream(context.Background(), UpstreamBranchOptions{
		Workspace:   workspace,
		Branch:      "foreman/test",
		UpstreamURL: bareUpstream,
		// BaseBranch intentionally empty -> defaults to main.
	}); err != nil {
		t.Fatalf("CreateBranchFromUpstream: %v", err)
	}
	if got := gitOut(t, workspace, "rev-parse", "HEAD"); got != upTip {
		t.Errorf("branch HEAD = %s, want upstream main tip %s", got, upTip)
	}
}

// mustClone clones src into dest and returns dest.
func mustClone(t *testing.T, src, dest string) string {
	t.Helper()
	mustGit(t, "", "clone", src, dest)
	return dest
}

func TestCreateBranchFromUpstream_Validation(t *testing.T) {
	cases := []struct {
		name string
		opts UpstreamBranchOptions
	}{
		{"missing workspace", UpstreamBranchOptions{Branch: "b", UpstreamURL: "u"}},
		{"missing branch", UpstreamBranchOptions{Workspace: "w", UpstreamURL: "u"}},
		{"missing upstream url", UpstreamBranchOptions{Workspace: "w", Branch: "b"}},
		// argv flag smuggling / traversal guards.
		{"dash base", UpstreamBranchOptions{Workspace: "w", Branch: "b", UpstreamURL: "u", BaseBranch: "-x"}},
		{"traversal base", UpstreamBranchOptions{Workspace: "w", Branch: "b", UpstreamURL: "u", BaseBranch: "../evil"}},
		{"dash branch", UpstreamBranchOptions{Workspace: "w", Branch: "--upload-pack=x", UpstreamURL: "u"}},
		{"dash upstream url", UpstreamBranchOptions{Workspace: "w", Branch: "b", UpstreamURL: "-x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := CreateBranchFromUpstream(context.Background(), tc.opts); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

// TestCreateBranchFromUpstream_FetchFailureErrors verifies an unreachable
// upstream surfaces an error (caller maps it to CloneFailed) rather than
// silently branching from the stale fork base.
func TestCreateBranchFromUpstream_FetchFailureErrors(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	bareFork := initBareOrigin(t, filepath.Join(dir, "fork"))
	seedOrigin(t, bareFork)
	workspace := mustClone(t, bareFork, filepath.Join(dir, "workspace"))

	err := CreateBranchFromUpstream(context.Background(), UpstreamBranchOptions{
		Workspace:   workspace,
		Branch:      "foreman/test",
		UpstreamURL: filepath.Join(dir, "does-not-exist.git"),
		BaseBranch:  "main",
	})
	if err == nil {
		t.Fatal("expected error when the upstream fetch fails")
	}
}
