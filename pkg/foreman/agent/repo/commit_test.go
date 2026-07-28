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
	"path/filepath"
	"strings"
	"testing"
)

// TestSoftResetToBase_AnchorsAtBranchPointNotUpstreamTip reproduces #1002: when
// upstream advances mid-run, BaseBranchSHA returns a tip AHEAD of where the task
// branch was actually cut. The soft reset must anchor at the true branch point
// (merge-base(base, HEAD)) so only the model's own edits are re-staged — the
// intervening upstream delta must never be dragged into the recovered commit.
func TestSoftResetToBase_AnchorsAtBranchPointNotUpstreamTip(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	bare := initBareOrigin(t, filepath.Join(dir, "up"))
	seedOrigin(t, bare) // main @ A (README.md)

	// Workspace cut from A; the model self-commits fix.txt on a task branch.
	ws := mustClone(t, bare, filepath.Join(dir, "ws"))
	mustGit(t, ws, "checkout", "-b", "foreman/wl/issue-1002")
	if err := os.WriteFile(filepath.Join(ws, "fix.txt"), []byte("model edit\n"), 0o644); err != nil {
		t.Fatalf("write fix.txt: %v", err)
	}
	mustGit(t, ws, "-c", "user.email=u@x", "-c", "user.name=u", "add", "fix.txt")
	mustGit(t, ws, "-c", "user.email=u@x", "-c", "user.name=u", "commit", "-m", "model self-commit")

	// Upstream advances to B (adds upstream.txt) AFTER the branch was cut.
	adv := mustClone(t, bare, filepath.Join(dir, "adv"))
	commitFile(t, adv, "upstream.txt", "intervening upstream delta\n")

	// Recovery: BaseBranchSHA fetches the current upstream tip (B) into ws and
	// returns it — the value the executor passes to SoftResetToBase.
	baseSHA, err := BaseBranchSHA(context.Background(), ws, bare, "main")
	if err != nil {
		t.Fatalf("BaseBranchSHA: %v", err)
	}
	if err := SoftResetToBase(context.Background(), ws, baseSHA); err != nil {
		t.Fatalf("SoftResetToBase: %v", err)
	}

	staged := gitOut(t, ws, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "fix.txt") {
		t.Errorf("model edit fix.txt must be staged for the recovered commit; staged=%q", staged)
	}
	if strings.Contains(staged, "upstream.txt") {
		t.Errorf("intervening upstream delta must NOT be re-staged (would revert merged work); staged=%q", staged)
	}
}

// TestSoftResetToBase_NoSelfCommitIsNothingToCommit verifies that when the model
// added no commits of its own, recovery reports ErrNothingToCommit rather than
// fabricating a commit — even if upstream advanced past the branch point.
func TestSoftResetToBase_NoSelfCommitIsNothingToCommit(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	bare := initBareOrigin(t, filepath.Join(dir, "up"))
	seedOrigin(t, bare)

	ws := mustClone(t, bare, filepath.Join(dir, "ws"))
	mustGit(t, ws, "checkout", "-b", "foreman/wl/issue-1002")

	// Upstream advances; the workspace itself made no commits.
	adv := mustClone(t, bare, filepath.Join(dir, "adv"))
	commitFile(t, adv, "upstream.txt", "intervening upstream delta\n")

	baseSHA, err := BaseBranchSHA(context.Background(), ws, bare, "main")
	if err != nil {
		t.Fatalf("BaseBranchSHA: %v", err)
	}
	if err := SoftResetToBase(context.Background(), ws, baseSHA); err != ErrNothingToCommit {
		t.Errorf("want ErrNothingToCommit, got %v", err)
	}
}

// TestCommit_CommitterAuthorSplit verifies that when Author and Committer
// differ, the commit's author block reflects the Author identity while the
// DCO sign-off trailer (`Signed-off-by`) is derived from the Committer
// identity. This is the fix for #1281: a bot author + human committer
// produces a human sign-off that satisfies CONTRIBUTING.md's DCO policy.
func TestCommit_CommitterAuthorSplit(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareOrigin(t, root)
	seedOrigin(t, bare)

	ctx := context.Background()
	dest := filepath.Join(root, "work")
	if err := Clone(ctx, CloneOptions{RemoteURL: bare, Dest: dest}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := CreateAndCheckoutBranch(ctx, dest, "foreman/issue-1281"); err != nil {
		t.Fatalf("CreateAndCheckoutBranch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "change.txt"),
		[]byte("bot-authored change\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}

	bot := Identity{Name: "Foreman Bot", Email: "bot@foreman.test"}
	human := Identity{Name: "Jory Dogfood", Email: "jory@example.com"}

	sha, err := Commit(ctx, CommitOptions{
		Workspace: dest,
		Message:   "fix: demonstrate committer/author split\n\nFixes #1281",
		Author:    bot,
		Committer: human,
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(sha) < 7 {
		t.Errorf("sha looks wrong: %q", sha)
	}

	// Author block must reflect the bot identity.
	authorName := gitOut(t, dest, "log", "-1", "--format=%an", sha)
	authorEmail := gitOut(t, dest, "log", "-1", "--format=%ae", sha)
	if authorName != bot.Name {
		t.Errorf("author name: got %q want %q", authorName, bot.Name)
	}
	if authorEmail != bot.Email {
		t.Errorf("author email: got %q want %q", authorEmail, bot.Email)
	}

	// Committer block must reflect the human identity.
	committerName := gitOut(t, dest, "log", "-1", "--format=%cn", sha)
	committerEmail := gitOut(t, dest, "log", "-1", "--format=%ce", sha)
	if committerName != human.Name {
		t.Errorf("committer name: got %q want %q", committerName, human.Name)
	}
	if committerEmail != human.Email {
		t.Errorf("committer email: got %q want %q", committerEmail, human.Email)
	}

	// The DCO sign-off trailer must be derived from the committer, not
	// the author — this is the whole point of #1281.
	body := gitOut(t, dest, "log", "-1", "--format=%B", sha)
	wantSignoff := "Signed-off-by: " + human.Name + " <" + human.Email + ">"
	if !strings.Contains(body, wantSignoff) {
		t.Errorf("missing human DCO trailer; commit body was:\n%s", body)
	}
	botSignoff := "Signed-off-by: " + bot.Name + " <" + bot.Email + ">"
	if strings.Contains(body, botSignoff) {
		t.Errorf("bot DCO trailer must not appear; commit body was:\n%s", body)
	}
}
