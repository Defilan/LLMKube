package agent

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

func TestTranscriptRanGoTest_BashCommand(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "go test ./pkg/foreman/agent/ -run TestReviewerSawDiff -count=1"),
		toolResult("bash", "call-1", "ok  	pkg/foreman/agent\n"),
	}
	if !transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=true for a `go test` bash command, got false")
	}
}

func TestTranscriptRanGoTest_ToolResultOnly(t *testing.T) {
	// The bash tool echoes the command back in its JSON output; a transcript
	// that records only the tool result (Name left empty) must still count.
	tr := []oai.Message{
		toolResult("", "call-1", `{"command":"go test ./pkg/...","exit_code":0,"stdout":"ok"}`),
	}
	if !transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=true for a `go test` tool result, got false")
	}
}

func TestTranscriptRanGoTest_NoGoTest(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n"),
		assistantBash("call-2", "ls -l"),
		toolResult("bash", "call-2", "total 8\n"),
	}
	if transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=false for a transcript with no `go test`, got true")
	}
}

func TestTranscriptRanGoTest_EmptyTranscript(t *testing.T) {
	if transcriptRanGoTest(nil) {
		t.Fatalf("expected transcriptRanGoTest=false for a nil transcript, got true")
	}
	if transcriptRanGoTest([]oai.Message{}) {
		t.Fatalf("expected transcriptRanGoTest=false for an empty transcript, got true")
	}
	// A transcript with only non-bash messages must not count either.
	tr := []oai.Message{
		{Role: oai.RoleSystem, Content: "go test ./..."},
		{Role: oai.RoleUser, Content: "review this"},
	}
	if transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=false when no bash/tool message runs `go test`, got true")
	}
}

// TestEnforceReviewerExecution_GoDiffRanGoTest is case 1: a .go diff whose
// transcript contains a `go test` call must not be demoted.
func TestEnforceReviewerExecution_GoDiffRanGoTest(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "go test ./pkg/foreman/agent/ -run TestReviewerSawDiff -count=1"),
		toolResult("bash", "call-1", "ok  	pkg/foreman/agent\n"),
	}
	extra := map[string]any{}
	got := enforceReviewerExecution(logr.Discard(), extra, []string{"pkg/foreman/agent/x.go"},
		foremanv1alpha1.AgenticTaskVerdictGo, tr)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("expected GO to stand when `go test` ran, got %v", got)
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Fatalf("expected no demotion marker when `go test` ran, extra=%v", extra)
	}
	if _, skipped := skippedFor(extra, railExecution); skipped {
		t.Fatalf("expected no rail-skip when `go test` ran, extra=%v", extra)
	}
}

// TestEnforceReviewerExecution_GoDiffNoGoTest is case 2: a .go diff whose
// transcript never ran `go test` must record the rail skipped and demote GO.
func TestEnforceReviewerExecution_GoDiffNoGoTest(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n"),
		assistantBash("call-2", "read_file pkg/foreman/agent/x.go"),
		toolResult("bash", "call-2", "package agent\n"),
	}
	extra := map[string]any{}
	got := enforceReviewerExecution(logr.Discard(), extra, []string{"pkg/foreman/agent/x.go"},
		foremanv1alpha1.AgenticTaskVerdictGo, tr)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("expected GO demoted to NO-GO when no `go test` ran, got %v", got)
	}
	reason, ok := skippedFor(extra, railExecution)
	if !ok {
		t.Fatalf("want %s recorded as skipped, extra=%v", railExecution, extra)
	}
	if reason != skipReasonNoTestRun {
		t.Fatalf("want reason %q, got %q", skipReasonNoTestRun, reason)
	}
	if extra["verdictDemotedBy"] != railExecution {
		t.Fatalf("demotion must name this rail in verdictDemotedBy=%q; got %v",
			railExecution, extra["verdictDemotedBy"])
	}
	if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Fatalf("verdictClaimed must archive the original GO, got %v", extra["verdictClaimed"])
	}
	if reason, _ := extra["demotionReason"].(string); strings.TrimSpace(reason) == "" {
		t.Fatalf("expected a non-empty demotionReason, got %v", extra["demotionReason"])
	}
}

// TestEnforceReviewerExecution_NonGoDiffExempt is case 3: a non-.go diff with
// no `go test` is exempt -- the mandatory execution runs only apply to Go.
func TestEnforceReviewerExecution_NonGoDiffExempt(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", "diff --git a/docs/x.md b/docs/x.md\n@@ -1 +1 @@\n"),
	}
	extra := map[string]any{}
	got := enforceReviewerExecution(logr.Discard(), extra, []string{"docs/x.md"},
		foremanv1alpha1.AgenticTaskVerdictGo, tr)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("expected a non-.go diff to be exempt (GO stands), got %v", got)
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Fatalf("expected no demotion for a non-.go diff, extra=%v", extra)
	}
	if _, skipped := skippedFor(extra, railExecution); skipped {
		t.Fatalf("expected no rail-skip for a non-.go diff, extra=%v", extra)
	}
}

func TestEnforceReviewerExecution_NonGoVerdictUntouched(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n"),
	}
	extra := map[string]any{}
	got := enforceReviewerExecution(logr.Discard(), extra, []string{"x.go"},
		foremanv1alpha1.AgenticTaskVerdictNoGo, tr)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("a non-GO verdict is not this rail's business, got %v", got)
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Fatalf("expected no demotion marker on a non-GO verdict, extra=%v", extra)
	}
}
