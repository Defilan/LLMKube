package agent

import (
	"errors"
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// assistantFunction returns an assistant message whose sole tool_call runs the
// given non-bash tool with the given raw JSON arguments. It mirrors
// assistantBash (in reviewer_diff_gate_test.go) but sets an arbitrary function
// name so the review-execution rail can distinguish a bash call from a
// read_file (or any other tool) that merely mentions `go test`.
func assistantFunction(id, name, args string) oai.Message {
	return oai.Message{
		Role:    oai.RoleAssistant,
		Content: "",
		ToolCalls: []oai.ToolCall{
			{
				ID:   id,
				Type: "function",
				Function: oai.ToolCallFunction{
					Name:      name,
					Arguments: args,
				},
			},
		},
	}
}

func TestTranscriptRanGoTest_BashCommand(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "go test ./pkg/foreman/agent/ -run TestReviewerSawDiff -count=1"),
		toolResult("bash", "call-1", "ok  \tpkg/foreman/agent\n"),
	}
	if !transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=true for a `go test` bash command, got false")
	}
}

func TestTranscriptRanGoTest_ToolResultCorrelatedToBashCall(t *testing.T) {
	// A bash tool result echoes the command back in its JSON output, so the
	// rail must count it when the result is correlated (via ToolCallID) to a
	// bash tool_call -- even though the assistant's own arguments mentioned
	// no `go test`.
	tr := []oai.Message{
		assistantBash("call-1", "make test-focus"),
		toolResult("bash", "call-1", `{"command":"go test ./pkg/...","exit_code":0,"stdout":"ok"}`),
	}
	if !transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=true for a `go test` bash tool result, got false")
	}
}

func TestTranscriptRanGoTest_ReadFileResultDoesNotCount(t *testing.T) {
	// Reading a file whose text carries the literal `go test` is not executing
	// `go test`. The reviewer must read every touched file (the
	// review_execution_gate_test.go fixtures contain exactly such a literal),
	// so a read_file result must not satisfy the rail.
	tr := []oai.Message{
		assistantFunction("call-1", "read_file", `{"path":"pkg/foreman/agent/review_execution_gate_test.go"}`),
		toolResult("read_file", "call-1", "package agent\n\ngo test ./pkg/foreman/agent/ -run TestX -count=1\n"),
	}
	if transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=false for a read_file result that merely mentions `go test`, got true")
	}
}

func TestTranscriptRanGoTest_GrepMentionDoesNotCount(t *testing.T) {
	// A grep that only searches for the string `go test` is not a `go test`
	// run; it must not satisfy the rail.
	tr := []oai.Message{
		assistantBash("call-1", `grep -rn "go test" Makefile`),
		toolResult("bash", "call-1", `matches found`),
	}
	if transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=false for a grep that mentions `go test`, got true")
	}

	// But a `go test` run after a command separator IS executed and counts.
	tr = []oai.Message{
		assistantBash("call-1", "cd /tmp && go test ./..."),
		toolResult("bash", "call-1", "ok\n"),
	}
	if !transcriptRanGoTest(tr) {
		t.Fatalf("expected transcriptRanGoTest=true for a `go test` after `&&`, got false")
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
// transcript contains a `go test` call must not be marked as skipped.
func TestEnforceReviewerExecution_GoDiffRanGoTest(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "go test ./pkg/foreman/agent/ -run TestReviewerSawDiff -count=1"),
		toolResult("bash", "call-1", "ok  \tpkg/foreman/agent\n"),
	}
	extra := map[string]any{}
	got := enforceReviewerExecution(logr.Discard(), extra, []string{"pkg/foreman/agent/x.go"}, nil,
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
// transcript never ran `go test` records the rail as skipped and the GO
// stands (marking, not demotion).
func TestEnforceReviewerExecution_GoDiffNoGoTest(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n"),
		assistantBash("call-2", "read_file pkg/foreman/agent/x.go"),
		toolResult("bash", "call-2", "package agent\n"),
	}
	extra := map[string]any{}
	got := enforceReviewerExecution(logr.Discard(), extra, []string{"pkg/foreman/agent/x.go"}, nil,
		foremanv1alpha1.AgenticTaskVerdictGo, tr)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("expected GO to stand (marking, not demotion), got %v", got)
	}
	reason, ok := skippedFor(extra, railExecution)
	if !ok {
		t.Fatalf("want %s recorded as skipped, extra=%v", railExecution, extra)
	}
	if reason != skipReasonNoTestRun {
		t.Fatalf("want reason %q, got %q", skipReasonNoTestRun, reason)
	}
	if _, ok := extra["verdictDemoted"]; ok {
		t.Fatalf("expected no verdictDemoted marker, extra=%v", extra)
	}
	if _, ok := extra["verdictDemotedBy"]; ok {
		t.Fatalf("expected no verdictDemotedBy marker, extra=%v", extra)
	}
	if _, ok := extra["verdictClaimed"]; ok {
		t.Fatalf("expected no verdictClaimed marker, extra=%v", extra)
	}
	if _, ok := extra["demotionReason"]; ok {
		t.Fatalf("expected no demotionReason marker, extra=%v", extra)
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
	got := enforceReviewerExecution(logr.Discard(), extra, []string{"docs/x.md"}, nil,
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
	got := enforceReviewerExecution(logr.Discard(), extra, []string{"x.go"}, nil,
		foremanv1alpha1.AgenticTaskVerdictNoGo, tr)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("a non-GO verdict is not this rail's business, got %v", got)
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Fatalf("expected no demotion marker on a non-GO verdict, extra=%v", extra)
	}
}

// TestTranscriptRanGoTest_CommandPrefixes pins the command-position anchor on
// both sides: the prefixes a reviewer naturally types in front of `go test`
// count as a run, while mentions inside another command's arguments do not.
func TestTranscriptRanGoTest_CommandPrefixes(t *testing.T) {
	// rawArgs carries a multi-line script as the JSON the bash tool would
	// actually receive; bashCallArgs does not escape newlines, so the table
	// spells that one case out.
	cases := []struct {
		name    string
		command string
		rawArgs string
		want    bool
	}{
		{name: "bare", command: "go test ./...", want: true},
		{name: "time prefix", command: "time go test ./pkg/foreman/agent/ -count=1", want: true},
		{name: "env assignment prefix", command: "GOFLAGS=-v go test ./...", want: true},
		{name: "two env assignments", command: "CGO_ENABLED=0 GOFLAGS=-mod=mod go test ./...", want: true},
		{name: "indented line of a script", rawArgs: `{"command":"cd /tmp\n  go test ./..."}`, want: true},
		{name: "after separator with prefix", command: "cd /tmp && CGO_ENABLED=0 go test ./...", want: true},
		{name: "grep mention", command: `grep -rn "go test" Makefile`, want: false},
		{name: "echo mention with prefix", command: `echo "time go test ./..."`, want: false},
		{name: "comment line", command: "# go test ./...", want: false},
		{name: "go testing subcommand", command: "go testing ./...", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := assistantBash("call-1", tc.command)
			if tc.rawArgs != "" {
				call = assistantFunction("call-1", "bash", tc.rawArgs)
			}
			tr := []oai.Message{
				call,
				toolResult("bash", "call-1", "ok\n"),
			}
			if got := transcriptRanGoTest(tr); got != tc.want {
				t.Fatalf("transcriptRanGoTest(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestEnforceReviewerExecution_DiffUnavailableRecordsSkip covers the input the
// Go-only guard cannot see: when the ground-truth diff fetch failed, diffFiles
// is nil and hasSourceFile reads that as a docs-only exemption. A GO in that
// state must be recorded as skipping the rail with the diff-unavailable
// reason, not the no-test-run reason, and the verdict must stand. A non-GO
// verdict is still not the rail's business and records nothing.
func TestEnforceReviewerExecution_DiffUnavailableRecordsSkip(t *testing.T) {
	diffErr := errors.New("git diff: exit status 128")
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", "fatal: bad revision\n"),
	}

	extra := map[string]any{}
	got := enforceReviewerExecution(logr.Discard(), extra, nil, diffErr,
		foremanv1alpha1.AgenticTaskVerdictGo, tr)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("expected GO to stand when the diff is unavailable (marking, not demotion), got %v", got)
	}
	reason, ok := skippedFor(extra, railExecution)
	if !ok {
		t.Fatalf("want %s recorded as skipped when the diff is unavailable, extra=%v", railExecution, extra)
	}
	if reason != skipReasonNoDiff {
		t.Fatalf("want reason %q, got %q", skipReasonNoDiff, reason)
	}
	for _, marker := range []string{"verdictDemoted", "verdictDemotedBy", "verdictClaimed", "demotionReason"} {
		if _, present := extra[marker]; present {
			t.Fatalf("expected no %s marker, extra=%v", marker, extra)
		}
	}

	// A transcript that DID run `go test` still records the skip: the rail
	// has no diff to say whether the run covered the change.
	extra = map[string]any{}
	ran := []oai.Message{
		assistantBash("call-1", "go test ./..."),
		toolResult("bash", "call-1", "ok\n"),
	}
	enforceReviewerExecution(logr.Discard(), extra, nil, diffErr, foremanv1alpha1.AgenticTaskVerdictGo, ran)
	if reason, ok := skippedFor(extra, railExecution); !ok || reason != skipReasonNoDiff {
		t.Fatalf("want %s: %s even when go test ran, extra=%v", railExecution, skipReasonNoDiff, extra)
	}

	// A non-GO verdict is untouched and records nothing.
	extra = map[string]any{}
	got = enforceReviewerExecution(logr.Discard(), extra, nil, diffErr,
		foremanv1alpha1.AgenticTaskVerdictNoGo, tr)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("a non-GO verdict is not this rail's business, got %v", got)
	}
	if _, skipped := skippedFor(extra, railExecution); skipped {
		t.Fatalf("expected no rail-skip on a non-GO verdict, extra=%v", extra)
	}
}
