package agent

import (
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// bashCallArgs builds the JSON arguments an assistant tool_call carries for a
// bash invocation (the "command" key the bash tool reads).
func bashCallArgs(command string) string {
	// Hand-built JSON keeps the test free of a json import and is exact.
	return `{"command":` + quoteJSON(command) + `}`
}

func quoteJSON(s string) string {
	// Minimal escaping: these test commands contain no quotes or backslashes.
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// assistantBash returns an assistant message whose sole tool_call runs the
// given command under the bash tool.
func assistantBash(id, command string) oai.Message {
	return oai.Message{
		Role:    oai.RoleAssistant,
		Content: "",
		ToolCalls: []oai.ToolCall{
			{
				ID:   id,
				Type: "function",
				Function: oai.ToolCallFunction{
					Name:      "bash",
					Arguments: bashCallArgs(command),
				},
			},
		},
	}
}

// toolResult returns a tool-role message responding to assistantCallID with the
// given raw content and (optionally empty) tool name.
func toolResult(name, assistantCallID, content string) oai.Message {
	return oai.Message{
		Role:       oai.RoleTool,
		Content:    content,
		ToolCallID: assistantCallID,
		Name:       name,
	}
}

// sampleDiff is a short, realistic `git diff` output that carries every kind of
// diff evidence (a `diff --git` header, a `--- a/` + `+++ b/` pair, and a hunk
// header), so a test can assert saw-diff with one realistic payload.
const sampleDiff = "diff --git a/x.go b/x.go\n" +
	"index 111..222 100644\n" +
	"--- a/x.go\n" +
	"+++ b/x.go\n" +
	"@@ -1,4 +1,6 @@\n" +
	"-func f() {}\n" +
	"+func g() {}\n"

// lsOutput and dockerfileContent reproduce the exact observed failure from
// #1570: the reviewer ran `ls -l` and read a Dockerfile, so the transcript
// carries no diff at all.
const lsOutput = "total 32\n" +
	"drwxr-xr-x 2 root root 4096 Aug 15 10:00 pkg\n" +
	"-rw-r--r-- 1 root root 1234 Aug 15 10:00 Dockerfile.foreman-agent\n"

const dockerfileContent = "FROM golang:1.22\nCOPY . .\nRUN go build -o /out /cmd/foreman-agent\n"

func TestReviewerSawDiff_GitDiffHeader(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", sampleDiff),
	}
	if !reviewerSawDiff(tr) {
		t.Fatalf("expected reviewerSawDiff=true for a `diff --git` tool result, got false")
	}
}

func TestReviewerSawDiff_HunkHeaderOnly(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git show HEAD"),
		toolResult("bash", "call-1", "@@ -1,4 +1,6 @@\n some context\n"),
	}
	if !reviewerSawDiff(tr) {
		t.Fatalf("expected reviewerSawDiff=true for a tool result carrying only a `@@` hunk header, got false")
	}
}

func TestReviewerSawDiff_FileHeaderPair(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff"),
		toolResult("bash", "call-1", "--- a/x.go\n+++ b/x.go\n-1+2\n"),
	}
	if !reviewerSawDiff(tr) {
		t.Fatalf("expected reviewerSawDiff=true for a tool result with both `--- a/` and `+++ b/`, got false")
	}
}

// TestReviewerSawDiff_NoDiff is the exact observed failure from #1570: the
// reviewer ran `ls -l` and read a Dockerfile, and the transcript contains no
// diff at all.
func TestReviewerSawDiff_NoDiff_ObservedFailure(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "ls -l"),
		toolResult("bash", "call-1", lsOutput),
		assistantBash("call-2", "cat Dockerfile.foreman-agent"),
		toolResult("bash", "call-2", dockerfileContent),
	}
	if reviewerSawDiff(tr) {
		t.Fatalf("expected reviewerSawDiff=false for a transcript with no diff (ls + read Dockerfile), got true")
	}
}

func TestReviewerSawDiff_EmptyTranscript(t *testing.T) {
	if reviewerSawDiff(nil) {
		t.Fatalf("expected reviewerSawDiff=false for a nil transcript, got true")
	}
	if reviewerSawDiff([]oai.Message{}) {
		t.Fatalf("expected reviewerSawDiff=false for an empty transcript, got true")
	}
	// A transcript with only non-tool messages must not count either.
	tr := []oai.Message{
		{Role: oai.RoleSystem, Content: "diff --git a/x b/x"},
		{Role: oai.RoleUser, Content: "review this"},
	}
	if reviewerSawDiff(tr) {
		t.Fatalf("expected reviewerSawDiff=false when no tool-role message carries the diff, got true")
	}
}

// TestReviewerSawDiff_EmptyToolNameCorrelatedByToolCallID exercises the
// backend that leaves a tool message's Name empty: the content must still be
// counted, and the producing tool is recoverable by ToolCallID.
func TestReviewerSawDiff_EmptyToolNameCorrelatedByToolCallID(t *testing.T) {
	const callID = "call-xyz"
	tr := []oai.Message{
		assistantBash(callID, "git diff"),
		toolResult("", callID, "diff --git a/main.go b/main.go\n@@ -10,2 +12,4 @@\n"),
	}
	if !reviewerSawDiff(tr) {
		t.Fatalf("expected reviewerSawDiff=true even when the tool message Name is empty, got false")
	}
	// The correlation map must recover the tool name by ToolCallID.
	names := callNameByToolCallID(tr)
	if got := names[callID]; got != "bash" {
		t.Fatalf("expected ToolCallID %q to correlate to tool name \"bash\", got %q", callID, got)
	}
}

func TestUngroundedReviewFinding_GoNoDiff(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "ls -l"),
		toolResult("bash", "call-1", "total 8\n-rw-r--r-- 1 root root 100 Aug 15 10:00 Dockerfile.foreman-agent\n"),
		assistantBash("call-2", "cat Dockerfile.foreman-agent"),
		toolResult("bash", "call-2", "FROM golang:1.22\n"),
	}
	failed, note := ungroundedReviewFinding(tr, "GO")
	if !failed {
		t.Fatalf("expected ungroundedReviewFinding failed=true for GO without a diff, got false")
	}
	if strings.TrimSpace(note) == "" {
		t.Fatalf("expected a non-empty note, got empty string")
	}
	low := strings.ToLower(note)
	if !strings.Contains(low, "diff") {
		t.Fatalf("expected the note to mention the diff, got: %s", note)
	}
	// No diff command was attempted, so the note should say so.
	if !strings.Contains(low, "no diff-producing command") {
		t.Fatalf("expected the note to report that no diff-producing command was attempted, got: %s", note)
	}
}

func TestUngroundedReviewFinding_GoWithDiff(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", "diff --git a/x.go b/x.go\n@@ -1,4 +1,6 @@\n"),
	}
	failed, note := ungroundedReviewFinding(tr, "GO")
	if failed {
		t.Fatalf("expected no finding for a GO whose transcript contains the diff, got failed=true note=%q", note)
	}
	if note != "" {
		t.Fatalf("expected an empty note when there is no finding, got %q", note)
	}
}

func TestUngroundedReviewFinding_NoGoNoDiff(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "ls -l"),
		toolResult("bash", "call-1", "total 8\n-rw-r--r-- 1 root root 100 Aug 15 10:00 Dockerfile.foreman-agent\n"),
	}
	failed, note := ungroundedReviewFinding(tr, "NO-GO")
	if failed {
		t.Fatalf("expected no finding for a NO-GO (a different problem, not this rail), got failed=true note=%q", note)
	}
	if note != "" {
		t.Fatalf("expected an empty note for a NO-GO, got %q", note)
	}
}

// TestUngroundedReviewFinding_GoDiffCommandAttempted checks the note reports
// the specific diff command the reviewer tried but never surfaced.
func TestUngroundedReviewFinding_GoDiffCommandAttempted(t *testing.T) {
	tr := []oai.Message{
		assistantBash("call-1", "git diff main...HEAD"),
		toolResult("bash", "call-1", "fatal: ambiguous argument 'main...HEAD'"),
	}
	failed, note := ungroundedReviewFinding(tr, "GO")
	if !failed {
		t.Fatalf("expected failed=true for a GO that tried to diff but surfaced none, got false")
	}
	if !strings.Contains(note, "git diff") {
		t.Fatalf("expected the note to name the attempted `git diff`, got: %s", note)
	}
}

func TestReviewerDiffCommands_PicksUpDiffCommandsIgnoresOthers(t *testing.T) {
	tr := []oai.Message{
		assistantBash("c1", "git diff main...HEAD"),
		assistantBash("c2", "git show"),
		assistantBash("c3", "git log -p --stat"),
		assistantBash("c4", "ls"),
		assistantBash("c5", "cat Dockerfile"),
		assistantBash("c6", "git log --oneline"), // no -p: not a diff producer
		// Duplicates must collapse to one entry each.
		assistantBash("c7", "git diff HEAD~1..HEAD"),
		assistantBash("c8", "git show HEAD"),
	}
	got := reviewerDiffCommands(tr)
	want := []string{"git diff", "git log -p", "git show"}
	if len(got) != len(want) {
		t.Fatalf("expected exactly %d distinct diff commands %v, got %v", len(want), want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %dth command to be %q, got %q (full: %v)", i, want[i], got[i], got)
		}
	}
}

func TestReviewerDiffCommands_EmptyTranscript(t *testing.T) {
	if got := reviewerDiffCommands(nil); len(got) != 0 {
		t.Fatalf("expected no diff commands for a nil transcript, got %v", got)
	}
}
