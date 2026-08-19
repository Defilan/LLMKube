package agent

import (
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// The scope rail's own short-circuits. See rail_skip_test.go for the shared
// helper and the other three rails fed by the same diff (#1605).

func TestScopeOverlap_NoIssueBodyIsRecorded(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, "",
		[]string{"pkg/foreman/agent/foo.go"}, foremanv1alpha1.AgenticTaskVerdictGo, nil)

	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("verdict must not change, got %v", got)
	}
	reason, ok := skippedFor(extra, railScopeOverlap)
	if !ok {
		t.Fatalf("want %s recorded, extra=%v", railScopeOverlap, extra)
	}
	if reason != skipReasonNoIssueBody {
		t.Errorf("want reason %q, got %q", skipReasonNoIssueBody, reason)
	}
}

func TestScopeOverlap_NoDiffFilesIsRecorded(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra,
		"Fix `pkg/foreman/agent/executor_native.go`.", nil,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)

	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("verdict must not change, got %v", got)
	}
	reason, ok := skippedFor(extra, railScopeOverlap)
	if !ok {
		t.Fatalf("want %s recorded, extra=%v", railScopeOverlap, extra)
	}
	if reason != skipReasonNoDiffFiles {
		t.Errorf("want reason %q, got %q", skipReasonNoDiffFiles, reason)
	}
}

// A check that ran must NOT carry the flag, or the signal is worthless.
func TestScopeOverlap_RanAndMatchedIsNotMarkedSkipped(t *testing.T) {
	extra := map[string]any{}
	enforceReviewerScopeOverlap(logr.Discard(), extra,
		"Fix `pkg/foreman/agent/foo.go`.", []string{"pkg/foreman/agent/foo.go"},
		foremanv1alpha1.AgenticTaskVerdictGo, nil)

	if _, ok := skippedFor(extra, railScopeOverlap); ok {
		t.Errorf("a check that ran must not be marked skipped, extra=%v", extra)
	}
}

func TestScopeOverlap_RanAndDriftedIsNotMarkedSkipped(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra,
		"Fix `pkg/foreman/agent/executor_native.go`.",
		[]string{"pkg/foreman/agent/unrelated.go"},
		foremanv1alpha1.AgenticTaskVerdictGo, nil)

	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("real drift must still demote, got %v", got)
	}
	if _, ok := skippedFor(extra, railScopeOverlap); ok {
		t.Errorf("a check that ran must not be marked skipped, extra=%v", extra)
	}
}

func TestScopeOverlap_NilExtraDoesNotPanic(t *testing.T) {
	got := enforceReviewerScopeOverlap(logr.Discard(), nil, "body", []string{"a.go"},
		foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("verdict must not change, got %v", got)
	}
}
