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

import (
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// nonEmptyChanged simulates a ground-truth branch diff that touched a.go
// lines 10 and 11 (i.e. the branch is demonstrably non-empty).
func nonEmptyChanged() func(string) map[int]bool {
	return func(f string) map[int]bool {
		if f == "a.go" {
			return map[int]bool{10: true, 11: true}
		}
		return nil
	}
}

// TestEmptyClaim_UnsupportedYieldsError is the #1552 case: the reviewer
// asserts the branch is empty with no supporting evidence, the ground-truth
// diff is non-empty, and the reviewer carries no grounded finding. The NO-GO
// must be remapped to ERROR (INCOMPLETE + ModelReportedError) so the branch
// routes to a human instead of failing the workload.
func TestEmptyClaim_UnsupportedYieldsError(t *testing.T) {
	extra := map[string]any{"reviewOutcome": "REQUEST-CHANGES"}
	got, reason := enforceReviewerEmptyClaim(logr.Discard(), extra,
		"NO-GO: branch has no commits and no code changes",
		foremanv1alpha1.AgenticTaskVerdictNoGo, nonEmptyChanged())
	if got != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("unsupported emptiness claim must remap to INCOMPLETE(ERROR), got %s", got)
	}
	if reason != foremanv1alpha1.FailureModelReportedError {
		t.Fatalf("remapped ERROR must carry ModelReportedError, got %s", reason)
	}
	if extra["emptyClaimRemappedToError"] != true {
		t.Fatal("expected emptyClaimRemappedToError=true")
	}
}

// TestEmptyClaim_EvidencedFindingKeepsNoGo is the regression guard: a
// reviewer that makes an emptiness claim BUT carries a grounded blocking
// finding (real supporting evidence) must still yield NO-GO, not ERROR.
func TestEmptyClaim_EvidencedFindingKeepsNoGo(t *testing.T) {
	extra := map[string]any{
		"reviewOutcome": "REQUEST-CHANGES",
		"findings": []any{
			map[string]any{"severity": "blocker", "area": "scope", "message": "branch empty", "file": "a.go", "line": 10},
		},
	}
	got, reason := enforceReviewerEmptyClaim(logr.Discard(), extra,
		"NO-GO: branch has no commits and no code changes",
		foremanv1alpha1.AgenticTaskVerdictNoGo, nonEmptyChanged())
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("an evidenced emptiness claim must keep NO-GO, got %s", got)
	}
	if reason != "" {
		t.Fatalf("evidenced emptiness claim must not set a failure reason, got %s", reason)
	}
	if _, remapped := extra["emptyClaimRemappedToError"]; remapped {
		t.Fatal("evidenced emptiness claim must not be marked remapped")
	}
}

// TestEmptyClaim_NoClaimKeepsNoGo: a genuine NO-GO that never asserts the
// branch is empty is left untouched even though the diff is non-empty.
func TestEmptyClaim_NoClaimKeepsNoGo(t *testing.T) {
	extra := map[string]any{
		"reviewOutcome": "REQUEST-CHANGES",
		"findings": []any{
			map[string]any{"severity": "blocker", "area": "scope", "message": "scope drift", "file": "a.go", "line": 11},
		},
	}
	got, reason := enforceReviewerEmptyClaim(logr.Discard(), extra,
		"NO-GO: diff addresses a different bug than the issue",
		foremanv1alpha1.AgenticTaskVerdictNoGo, nonEmptyChanged())
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("a non-emptiness NO-GO must stay NO-GO, got %s", got)
	}
	if reason != "" {
		t.Fatalf("a non-emptiness NO-GO must not set a failure reason, got %s", reason)
	}
}

// TestEmptyClaim_GoUntouched: the rail only acts on NO-GO.
func TestEmptyClaim_GoUntouched(t *testing.T) {
	extra := map[string]any{}
	got, reason := enforceReviewerEmptyClaim(logr.Discard(), extra,
		"branch is empty", foremanv1alpha1.AgenticTaskVerdictGo, nonEmptyChanged())
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("rail must leave GO untouched, got %s", got)
	}
	if reason != "" {
		t.Fatalf("rail must not set a failure reason on GO, got %s", reason)
	}
}

// TestEmptyClaim_GitUnavailableDegradesOpen: with no ground-truth diff
// (changedLines nil) the claim cannot be contradicted, so the verdict is left
// untouched (degrade open).
func TestEmptyClaim_GitUnavailableDegradesOpen(t *testing.T) {
	extra := map[string]any{}
	got, reason := enforceReviewerEmptyClaim(logr.Discard(), extra,
		"branch is empty", foremanv1alpha1.AgenticTaskVerdictNoGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("nil changedLines must leave the verdict untouched, got %s", got)
	}
	if reason != "" {
		t.Fatalf("nil changedLines must not set a failure reason, got %s", reason)
	}
}

// TestEmptyClaim_ToggleOff: FOREMAN_EMPTY_CLAIM=0 disables the rail.
func TestEmptyClaim_ToggleOff(t *testing.T) {
	t.Setenv("FOREMAN_EMPTY_CLAIM", "0")
	extra := map[string]any{}
	got, reason := enforceReviewerEmptyClaim(logr.Discard(), extra,
		"branch is empty", foremanv1alpha1.AgenticTaskVerdictNoGo, nonEmptyChanged())
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("toggle off must leave the verdict untouched, got %s", got)
	}
	if reason != "" {
		t.Fatalf("toggle off must not set a failure reason, got %s", reason)
	}
}

// TestAssertsEmptyBranch checks the claim detector on the reviewer's own
// words: positive cases trip, ordinary findings do not.
func TestAssertsEmptyBranch(t *testing.T) {
	positives := []string{
		"branch has no commits and no code changes",
		"the branch is empty",
		"commit history is unreadable",
		"nothing to review",
		"cannot read the history",
	}
	for _, s := range positives {
		if !assertsEmptyBranch(s) {
			t.Errorf("assertsEmptyBranch(%q) = false, want true", s)
		}
	}
	negatives := []string{
		"diff addresses a different bug",
		"missing regression test",
		"scope drift on the touched file",
		"empty error message is not handled",
	}
	for _, s := range negatives {
		if assertsEmptyBranch(s) {
			t.Errorf("assertsEmptyBranch(%q) = true, want false", s)
		}
	}
}
