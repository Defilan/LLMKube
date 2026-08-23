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
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := AnswerDecision(dir, 1602, "adjudicate", "revise"); err == nil {
		t.Fatal("want an error when the decision cannot be written")
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
