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

package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParkAndListDecisions(t *testing.T) {
	dir := t.TempDir()
	d := Decision{
		Issue:    1602,
		Workload: "wl-1602-deadcode-triage",
		Stage:    "verify",
		Kind:     "adjudicate",
		Opened:   time.Date(2026, 8, 23, 4, 12, 0, 0, time.UTC),
		Reason:   "verify found issues",
		Evidence: map[string]string{"verify": "./evidence/1602-verify.txt"},
		Options:  []string{"accept", "revise", "escalate", "drop"},
	}
	path, err := ParkDecision(dir, d)
	if err != nil {
		t.Fatalf("ParkDecision: %v", err)
	}
	if path == "" {
		t.Fatal("ParkDecision returned an empty path")
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Issue != 1602 || got[0].Kind != "adjudicate" {
		t.Errorf("got %+v, want issue 1602 / adjudicate", got[0])
	}
	if got[0].Answer != "" {
		t.Errorf("Answer = %q, want empty on a fresh decision", got[0].Answer)
	}
	// Every field the caller parked has to survive the write/read, not just
	// the two that also make up the filename.
	if got[0].Workload != d.Workload {
		t.Errorf("Workload = %q, want %q", got[0].Workload, d.Workload)
	}
	if got[0].Stage != d.Stage {
		t.Errorf("Stage = %q, want %q", got[0].Stage, d.Stage)
	}
	if !got[0].Opened.Equal(d.Opened) {
		t.Errorf("Opened = %v, want %v", got[0].Opened, d.Opened)
	}
	if got[0].Reason != d.Reason {
		t.Errorf("Reason = %q, want %q", got[0].Reason, d.Reason)
	}
	if got[0].Evidence["verify"] != "./evidence/1602-verify.txt" {
		t.Errorf("Evidence = %v, want the parked verify path", got[0].Evidence)
	}
	if !reflect.DeepEqual(got[0].Options, d.Options) {
		t.Errorf("Options = %v, want %v", got[0].Options, d.Options)
	}
}

func TestAnswerDecision_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate", Options: []string{"accept", "revise"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := AnswerDecision(dir, 1602, "adjudicate", "revise"); err != nil {
		t.Fatalf("AnswerDecision: %v", err)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Answer != "revise" {
		t.Errorf("Answer = %q, want revise", got[0].Answer)
	}
}

func TestAnswerDecision_RejectsAnOptionNotOffered(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate", Options: []string{"accept", "revise"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := AnswerDecision(dir, 1602, "adjudicate", "nonsense"); err == nil {
		t.Fatal("want an error for an answer outside the offered options")
	}
}

func TestListDecisions_EmptyDirIsNotAnError(t *testing.T) {
	got, err := ListDecisions(t.TempDir())
	if err != nil {
		t.Fatalf("ListDecisions on an empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestParkDecision_CreatesTheDecisionsDir(t *testing.T) {
	// The loop parks into a directory that does not exist yet on the first
	// run of a session, so creating it is part of the contract.
	dir := filepath.Join(t.TempDir(), "decisions")
	path, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "adjudicate"})
	if err != nil {
		t.Fatalf("ParkDecision into a missing dir: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func TestParkDecision_StampsOpenedWhenUnset(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "adjudicate"}); err != nil {
		t.Fatal(err)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Opened.IsZero() {
		t.Error("Opened is zero, want a stamp from ParkDecision")
	}
}

func TestListDecisions_MissingDirIsNotAnError(t *testing.T) {
	// Distinct from an existing empty dir: nothing has parked yet, so the
	// directory itself is absent.
	got, err := ListDecisions(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("ListDecisions on a missing dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestListDecisions_SkipsNonYAMLEntries(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "adjudicate"}); err != nil {
		t.Fatal(err)
	}
	// A stray note that happens to parse as a Decision, and a directory whose
	// name ends in .yaml. Neither is a parked decision.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("issue: 999\nkind: bogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "archive.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if got[0].Issue != 1602 {
		t.Errorf("Issue = %d, want 1602", got[0].Issue)
	}
}

func TestListDecisions_OldestFirst(t *testing.T) {
	dir := t.TempDir()
	// 1602 is opened earlier but sorts after 1500 by filename, so filename
	// order and Opened order disagree.
	if _, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate",
		Opened: time.Date(2026, 8, 23, 4, 12, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ParkDecision(dir, Decision{
		Issue: 1500, Kind: "adjudicate",
		Opened: time.Date(2026, 8, 23, 5, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Issue != 1602 || got[1].Issue != 1500 {
		t.Errorf("order = [%d %d], want [1602 1500] (oldest first)", got[0].Issue, got[1].Issue)
	}
}

func TestParkDecision_KeepsAnAnsweredDecision(t *testing.T) {
	// The driver parks unconditionally and nothing reads a decision before
	// re-parking it, so a second run of the same item must not overwrite an
	// answer a human already typed.
	dir := t.TempDir()
	first, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate",
		Reason:  "verify found issues",
		Options: []string{"accept", "revise"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AnswerDecision(dir, 1602, "adjudicate", "revise"); err != nil {
		t.Fatal(err)
	}
	again, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate",
		Reason:   "second pass found more issues",
		Evidence: map[string]string{"verify": "./evidence/1602-verify-2.txt"},
		Options:  []string{"accept", "revise"},
	})
	if err != nil {
		t.Fatalf("re-park an answered decision: %v", err)
	}
	if again != first {
		t.Errorf("path = %q, want the existing %q", again, first)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Answer != "revise" {
		t.Errorf("Answer = %q, want revise to survive the re-park", got[0].Answer)
	}
	if got[0].Reason != "verify found issues" {
		t.Errorf("Reason = %q, want the answered decision left intact", got[0].Reason)
	}
}

func TestParkDecision_RefreshesAnUnansweredDecision(t *testing.T) {
	// The guard is about answers, not about existence: an unanswered decision
	// still picks up the current reason. Options are set here to match the
	// answered case exactly, so the only difference between the two tests is
	// the answer itself and the guard cannot be satisfied by anything else.
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate", Reason: "verify found issues",
		Options: []string{"accept", "revise"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate", Reason: "second pass found more issues",
		Options: []string{"accept", "revise"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Reason != "second pass found more issues" {
		t.Errorf("Reason = %q, want the refreshed reason", got[0].Reason)
	}
}

// answeredDecisionDir parks one answerable decision and returns its directory
// and file path. Named for this file to avoid colliding with the package's
// other test helpers.
func decisionFixtureDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate", Options: []string{"accept", "revise"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func TestAnswerDecision_ErrorsWhenThereIsNoSuchDecision(t *testing.T) {
	dir, _ := decisionFixtureDir(t)
	if err := AnswerDecision(dir, 9999, "adjudicate", "revise"); err == nil {
		t.Error("want an error for an issue with no parked decision")
	}
	if err := AnswerDecision(dir, 1602, "triage", "revise"); err == nil {
		t.Error("want an error for a kind with no parked decision")
	}
	if err := AnswerDecision(filepath.Join(t.TempDir(), "never-created"), 1602, "adjudicate", "revise"); err == nil {
		t.Error("want an error when the decisions dir does not exist")
	}
}

func TestAnswerDecision_ErrorsWhenTheWriteFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, permission bits do not apply")
	}
	dir, p := decisionFixtureDir(t)
	// Read-only file and read-only directory: an in-place rewrite and a
	// staged replace both have to fail, and the answer must not be reported
	// as recorded when it was not.
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // G302: directory mode
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: directory mode
	err := AnswerDecision(dir, 1602, "adjudicate", "revise")
	if err == nil {
		t.Fatal("want an error when the decision cannot be written")
	}
	if !strings.HasPrefix(err.Error(), "write decision") {
		t.Errorf("err = %v, want it wrapped like the file's other errors", err)
	}
}

func TestParkDecision_WritesTheFileUserReadWriteOnly(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, permission bits do not apply")
	}
	_, p := decisionFixtureDir(t)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// A decision can carry evidence paths and a human's answer. Nothing else
	// on the box needs to read it.
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
}

func TestParkDecision_RefusesToOverwriteAnUnparseableDecision(t *testing.T) {
	// A half-written decision still has the answer in it in plain text. The
	// likeliest way it got that way is an interrupted write of the answer.
	dir := t.TempDir()
	p := filepath.Join(dir, "1602-adjudicate.yaml")
	truncated := "answer: revise\noptions: [accept, revise\n"
	if err := os.WriteFile(p, []byte(truncated), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate", Reason: "second pass found more issues",
	}); err == nil {
		t.Error("want an error rather than an overwrite of an unparseable decision")
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != truncated {
		t.Errorf("file = %q, want the damaged original left alone", string(after))
	}
}

func TestParkDecision_RefusesToOverwriteAnUnreadableDecision(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, permission bits do not apply")
	}
	dir, p := decisionFixtureDir(t)
	if err := AnswerDecision(dir, 1602, "adjudicate", "revise"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	if _, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate", Reason: "second pass found more issues",
	}); err == nil {
		t.Error("want an error rather than an overwrite of an unreadable decision")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Answer != "revise" {
		t.Errorf("got %+v, want the answer still on disk", got)
	}
}

func TestDecisionWritesReplaceRatherThanTruncate(t *testing.T) {
	// An in-place truncating write is the mechanism behind a half-written
	// answer. A staged write plus rename leaves a different file behind, which
	// is the observable difference between the two.
	dir, p := decisionFixtureDir(t)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := AnswerDecision(dir, 1602, "adjudicate", "revise"); err != nil {
		t.Fatal(err)
	}
	afterAnswer, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, afterAnswer) {
		t.Error("AnswerDecision rewrote the file in place, want an atomic replace")
	}

	dir2, p2 := decisionFixtureDir(t)
	before2, err := os.Stat(p2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParkDecision(dir2, Decision{
		Issue: 1602, Kind: "adjudicate", Reason: "refreshed",
		Options: []string{"accept", "revise"},
	}); err != nil {
		t.Fatal(err)
	}
	after2, err := os.Stat(p2)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before2, after2) {
		t.Error("ParkDecision rewrote the file in place, want an atomic replace")
	}
}

func TestDecisionWritesLeaveNoTempFiles(t *testing.T) {
	dir, _ := decisionFixtureDir(t)
	if err := AnswerDecision(dir, 1602, "adjudicate", "revise"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "1602-adjudicate.yaml" {
		t.Errorf("dir = %v, want just the decision file", decisionDirNames(t, dir))
	}

	// The failing path is the one that matters: a staged file that never got
	// renamed must not be left lying around for the next run to trip over.
	blocked := t.TempDir()
	target := filepath.Join(blocked, "1602-adjudicate.yaml")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDecisionFile(target, []byte("issue: 1602\n")); err == nil {
		t.Fatal("want an error when the staged file cannot be renamed into place")
	}
	names := decisionDirNames(t, blocked)
	if len(names) != 1 || names[0] != "1602-adjudicate.yaml" {
		t.Errorf("dir = %v, want no staged leftovers", names)
	}
}

func decisionDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestParkDecision_RejectsAKindThatEscapesTheDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sub", "decisions")
	// "1602-.." is one path element, so it takes three to climb out of dir.
	if _, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "../../../x"}); err == nil {
		t.Error("want an error for a kind that climbs out of the decisions dir")
	}
	escaped := filepath.Join(base, "sub", "x.yaml")
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Errorf("%s exists, want nothing written outside the decisions dir", escaped)
	}
}

func TestParkDecision_RejectsAKindWithAPathSeparator(t *testing.T) {
	dir := t.TempDir()
	// The subdirectory exists, so without the check this park would succeed
	// and file the decision somewhere ListDecisions never looks.
	if err := os.Mkdir(filepath.Join(dir, "1602-nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "nested/x"}); err == nil {
		t.Error("want an error for a kind containing a path separator")
	}
	buried := filepath.Join(dir, "1602-nested", "x.yaml")
	if _, err := os.Stat(buried); !os.IsNotExist(err) {
		t.Errorf("%s exists, want no decision filed in a subdirectory", buried)
	}
}

func TestAnswerDecision_RejectsAKindThatEscapesTheDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sub", "decisions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(base, "sub", "victim.yaml")
	const unrelated = "unrelated: keep me\n"
	if err := os.WriteFile(victim, []byte(unrelated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AnswerDecision(dir, 1602, "../../../victim", "revise"); err == nil {
		t.Error("want an error for a kind that climbs out of the decisions dir")
	}
	after, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != unrelated {
		t.Errorf("victim = %q, want the unrelated file untouched", string(after))
	}
}

func TestListDecisions_ReturnsTheGoodOnesAndNamesTheBad(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "adjudicate"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1500-adjudicate.yaml"),
		[]byte("answer: revise\noptions: [accept, revise\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := os.Getuid() != 0
	if unreadable {
		if err := os.WriteFile(filepath.Join(dir, "1400-adjudicate.yaml"), []byte("issue: 1400\n"), 0o200); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListDecisions(dir)
	if err == nil {
		t.Fatal("want an error naming the decisions that could not be read")
	}
	if !strings.Contains(err.Error(), "1500-adjudicate.yaml") {
		t.Errorf("err = %v, want it to name 1500-adjudicate.yaml", err)
	}
	if unreadable && !strings.Contains(err.Error(), "1400-adjudicate.yaml") {
		t.Errorf("err = %v, want it to name 1400-adjudicate.yaml", err)
	}
	// The point of the change: the human still sees what is parked.
	if len(got) != 1 {
		t.Fatalf("len = %d, want the one decision that parsed", len(got))
	}
	if got[0].Issue != 1602 {
		t.Errorf("Issue = %d, want 1602", got[0].Issue)
	}
}

func TestAnswerDecision_TrimsTheAnswer(t *testing.T) {
	dir, _ := decisionFixtureDir(t)
	if err := AnswerDecision(dir, 1602, "adjudicate", "  revise\n"); err != nil {
		t.Fatalf("AnswerDecision with surrounding whitespace: %v", err)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Answer != "revise" {
		t.Errorf("Answer = %q, want it trimmed", got[0].Answer)
	}

	// Without options there is no membership check to do the trimming for us,
	// so this pins the stored value on its own.
	loose := t.TempDir()
	if _, err := ParkDecision(loose, Decision{Issue: 1700, Kind: "adjudicate"}); err != nil {
		t.Fatal(err)
	}
	if err := AnswerDecision(loose, 1700, "adjudicate", "  accept  "); err != nil {
		t.Fatal(err)
	}
	got, err = ListDecisions(loose)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Answer != "accept" {
		t.Errorf("Answer = %q, want it trimmed", got[0].Answer)
	}
}

func TestAnswerDecision_RejectsAnEmptyAnswer(t *testing.T) {
	// No options, so the permissive branch would otherwise accept anything.
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "adjudicate"}); err != nil {
		t.Fatal(err)
	}
	for _, answer := range []string{"", "   ", "\t\n"} {
		if err := AnswerDecision(dir, 1602, "adjudicate", answer); err == nil {
			t.Errorf("AnswerDecision(%q) = nil, want an error", answer)
		}
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Answer != "" {
		t.Errorf("Answer = %q, want the decision still unanswered", got[0].Answer)
	}
}

func TestDecisionWritesStageInTheDecisionsDir(t *testing.T) {
	// The staged file has to be a sibling of the target, not a file in TMPDIR.
	// os.Rename across filesystems returns EXDEV, and a decisions dir under a
	// home directory against a /var/folders TMPDIR is exactly that arrangement
	// on macOS, so staging in TMPDIR would fail every write on a real box while
	// passing every test on a box where the two happen to share a filesystem.
	// Pointing TMPDIR at somewhere that does not exist stands in for that: a
	// write that needs TMPDIR breaks, a write that stages beside the target
	// does not care.
	dir := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(dir, "no-such-tmpdir"))
	p, err := ParkDecision(dir, Decision{
		Issue: 1602, Kind: "adjudicate", Options: []string{"accept", "revise"},
	})
	if err != nil {
		t.Fatalf("ParkDecision with TMPDIR pointing nowhere: %v", err)
	}
	if got := filepath.Dir(p); got != dir {
		t.Errorf("decision at %s, want it in %s", got, dir)
	}
	if err := AnswerDecision(dir, 1602, "adjudicate", "revise"); err != nil {
		t.Fatalf("AnswerDecision with TMPDIR pointing nowhere: %v", err)
	}
}

func TestParkDecision_ChecksTheKindBeforeTouchingTheDisk(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "decisions")
	if _, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "../x"}); err == nil {
		t.Fatal("want an error for a kind containing a path separator")
	}
	// Rejecting the kind only after MkdirAll would leave the directory behind.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s exists, want nothing created for a rejected kind", dir)
	}
}

func TestListDecisions_IgnoresALeakedStageFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{Issue: 1602, Kind: "adjudicate"}); err != nil {
		t.Fatal(err)
	}
	// What a process killed between the create and the rename leaves behind.
	// Half-written, so if ListDecisions ever looks at it, it does not merely
	// show up as an extra decision, it becomes a parse error the human cannot
	// clear without knowing what the file is.
	leaked := filepath.Join(dir, strings.Replace(decisionStagePattern, "*", "3901215257", 1))
	if err := os.WriteFile(leaked, []byte("issue: 1602\noptions: [accept, revise\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ListDecisions(dir)
	if err != nil {
		t.Errorf("ListDecisions: %v, want a leaked stage file to be invisible", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want just the parked decision: %v", len(got), decisionDirNames(t, dir))
	}
	if got[0].Issue != 1602 {
		t.Errorf("Issue = %d, want 1602", got[0].Issue)
	}
}

func TestParkDecision_DoesNotClaimACorruptDecisionWhenNoneExists(t *testing.T) {
	// A name too long is a read error that is not "no such file", so it lands
	// in the same refusal branch as a genuinely unreadable decision. It still
	// has to fail safely, but it must not send a human hunting for a corrupt
	// file that was never there.
	dir := t.TempDir()
	_, err := ParkDecision(dir, Decision{Issue: 1602, Kind: strings.Repeat("k", 300)})
	if err == nil {
		t.Fatal("want an error for a decision path that cannot be read or written")
	}
	if strings.Contains(err.Error(), "overwrite") {
		t.Errorf("err = %v, want no claim that an existing decision is in the way", err)
	}
	if names := decisionDirNames(t, dir); len(names) != 0 {
		t.Errorf("dir = %v, want nothing written", names)
	}
}

func TestDecisionKindMustNotBeEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{Issue: 1602}); err == nil {
		t.Error("want an error for an empty kind")
	}
	if names := decisionDirNames(t, dir); len(names) != 0 {
		t.Errorf("dir = %v, want no decision parked without a kind", names)
	}
	if err := AnswerDecision(dir, 1602, "", "revise"); err == nil {
		t.Error("want an error for an empty kind")
	}
}
