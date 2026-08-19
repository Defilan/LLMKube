package agent

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// skippedFor returns the recorded reason a rail could not run, if any.
func skippedFor(extra map[string]any, rail string) (string, bool) {
	entries, _ := extra[railsSkippedKey].([]string)
	for _, e := range entries {
		if strings.HasPrefix(e, rail+": ") {
			return strings.TrimPrefix(e, rail+": "), true
		}
	}
	return "", false
}

// One DiffNameOnly failure disables four rails at once (#1605). Each must say
// so, or a verdict nothing checked looks identical to one that earned it.

func TestRailSkipped_VerdictFromFindingsRecordsMissingDiff(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerVerdictFromFindings(logr.Discard(), extra,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)

	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("verdict must not change, got %v", got)
	}
	reason, ok := skippedFor(extra, railVerdictFromFindings)
	if !ok {
		t.Fatalf("want %s recorded, extra=%v", railVerdictFromFindings, extra)
	}
	if reason != skipReasonNoDiff {
		t.Errorf("want reason %q, got %q", skipReasonNoDiff, reason)
	}
}

func TestRailSkipped_VerdictFromFindingsThatRanIsNotMarked(t *testing.T) {
	extra := map[string]any{}
	changed := func(string) map[int]bool { return map[int]bool{1: true} }
	enforceReviewerVerdictFromFindings(logr.Discard(), extra,
		foremanv1alpha1.AgenticTaskVerdictGo, changed)

	if _, ok := skippedFor(extra, railVerdictFromFindings); ok {
		t.Errorf("a rail that ran must not be marked skipped, extra=%v", extra)
	}
}

func TestRailSkipped_EmptyClaimRecordsMissingDiff(t *testing.T) {
	extra := map[string]any{}
	got, _ := enforceReviewerEmptyClaim(logr.Discard(), extra,
		"the branch is empty", foremanv1alpha1.AgenticTaskVerdictNoGo, nil)

	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("verdict must not change, got %v", got)
	}
	reason, ok := skippedFor(extra, railEmptyClaim)
	if !ok {
		t.Fatalf("want %s recorded, extra=%v", railEmptyClaim, extra)
	}
	if reason != skipReasonNoDiff {
		t.Errorf("want reason %q, got %q", skipReasonNoDiff, reason)
	}
}

// The grounded rail already sets groundingUnavailable (#1576). It must ALSO
// join the shared list, or the list is misleading by omission.
func TestRailSkipped_GroundedFindingJoinsSharedList(t *testing.T) {
	extra := findingExtra("blocker", "pkg/cli/cache.go", 42)
	enforceReviewerGroundedFindings(logr.Discard(), extra,
		foremanv1alpha1.AgenticTaskVerdictNoGo, nil)

	if unavailable, _ := extra["groundingUnavailable"].(bool); !unavailable {
		t.Errorf("#1576 marker must be preserved, extra=%v", extra)
	}
	if _, ok := skippedFor(extra, railGroundedFinding); !ok {
		t.Errorf("want %s in the shared list, extra=%v", railGroundedFinding, extra)
	}
}

func TestRailSkipped_DedupesRepeatedEntries(t *testing.T) {
	extra := map[string]any{}
	recordRailSkipped(extra, railScopeOverlap, skipReasonNoDiff)
	recordRailSkipped(extra, railScopeOverlap, skipReasonNoDiff)
	if entries, _ := extra[railsSkippedKey].([]string); len(entries) != 1 {
		t.Errorf("want 1 entry, got %v", entries)
	}
}

func TestRailSkipped_NilExtraDoesNotPanic(t *testing.T) {
	recordRailSkipped(nil, railScopeOverlap, skipReasonNoDiff)
}
