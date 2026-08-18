package agent

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// reviewer_diff_gate.go is a pure-function rail that detects whether a
// reviewer Agent's transcript contains evidence that it ever obtained the
// diff of the branch it reviewed. See issue #1570: in two consecutive runs a
// reviewer returned GO while its transcript contained zero occurrences of the
// branch's diff, so the verdict was uncorrelated with the code it approved.
//
// These functions are deterministic and model-free: they run over a stored
// transcript ([]oai.Message) and make no git calls, mirroring the shape of
// coder_grounding_gate.go (walk a transcript, correlate tool results back to
// the assistant tool_call that produced them, return findings). Wiring them
// into the executor / gate chain is deliberately a separate change.
var (
	// reDiffGitLine matches a line that begins "diff --git" — the header `git
	// diff` emits for every changed file.
	reDiffGitLine = regexp.MustCompile(`(?m)^diff --git`)
	// reMinusFile / rePlusFile are the two-file header pair a unified diff
	// carries for every changed file. The plus signs are escaped: `+` is a
	// quantifier in regexp and `+++` would otherwise parse as a nested
	// repetition and fail to compile.
	reMinusFile = regexp.MustCompile(`(?m)^--- a/`)
	rePlusFile  = regexp.MustCompile(`(?m)^\+\+\+ b/`)
	// reHunkHeader matches a unified-diff hunk header:
	//
	//	@@ -<n>,<m> +<n>,<m> @@
	//
	// The comma-count parts are optional (a single-line hunk omits them).
	reHunkHeader = regexp.MustCompile(`(?m)@@ -\d+(,\d+)? \+\d+(,\d+)? @@`)
)

// diffEvidence reports whether s carries any marker of a unified diff: a
// "diff --git" line, both a "--- a/" and a "+++ b/" header, or a unified hunk
// header. It is the single predicate the rail relies on.
func diffEvidence(s string) bool {
	if reDiffGitLine.MatchString(s) {
		return true
	}
	if reMinusFile.MatchString(s) && rePlusFile.MatchString(s) {
		return true
	}
	return reHunkHeader.MatchString(s)
}

// toolContent returns the textual content of a tool message. Content is a
// plain string on the wire; Parts (multimodal, #1466) is folded in so a text
// part that carries the diff is not missed.
func toolContent(m oai.Message) string {
	if len(m.Parts) == 0 {
		return m.Content
	}
	var b strings.Builder
	b.WriteString(m.Content)
	for _, p := range m.Parts {
		if p.Type == oai.ContentPartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// callNameByToolCallID builds the assistant tool_call ID -> function name map
// used to recover a tool message's name when the backend left Name empty, the
// same correlation coder_grounding_gate.go uses to tie a tool result back to
// the call that produced it.
func callNameByToolCallID(transcript []oai.Message) map[string]string {
	names := make(map[string]string)
	for _, m := range transcript {
		if m.Role != oai.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			names[tc.ID] = tc.Function.Name
		}
	}
	return names
}

// reviewerSawDiff reports whether any TOOL-ROLE message in the transcript
// carries diff evidence (see diffEvidence). A tool message whose Name is
// empty is still counted — the content is what matters, and the name is
// recoverable from the assistant tool_call by ToolCallID if it is ever needed
// (see callNameByToolCallID). An empty transcript yields false without panic.
func reviewerSawDiff(transcript []oai.Message) bool {
	_ = callNameByToolCallID(transcript) // keep the correlation available (see gate)
	for i := range transcript {
		m := transcript[i]
		if m.Role != oai.RoleTool {
			continue
		}
		if diffEvidence(toolContent(m)) {
			return true
		}
	}
	return false
}

// classifyDiffCommand returns the canonical diff-producing command a raw shell
// command belongs to, or "" when it produces no diff. It recognises
// `git diff`, `git show`, and `git log -p` (a log must carry -p / --patch to
// produce patches; a bare `git log` does not).
func classifyDiffCommand(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) < 2 || fields[0] != "git" {
		return ""
	}
	switch fields[1] {
	case "diff":
		return "git diff"
	case "show":
		return "git show"
	case "log":
		for _, f := range fields[2:] {
			if f == "-p" || f == "--patch" {
				return "git log -p"
			}
		}
	}
	return ""
}

// commandFromToolCallArgs extracts the shell command from an assistant
// tool_call's JSON arguments. The bash tool exposes it under "command"; the
// alternates are tolerated so the rail is robust to the exact schema.
func commandFromToolCallArgs(args string) string {
	s := strings.TrimSpace(args)
	if s == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return ""
	}
	for _, key := range []string{"command", "cmd", "script"} {
		if v, ok := obj[key].(string); ok {
			return v
		}
	}
	return ""
}

// reviewerDiffCommands returns the distinct diff-producing commands the
// reviewer attempted, read from the assistant tool_call arguments, in
// deterministic (sorted) order and deduplicated. It is used to make the
// ungrounded finding specific: whether the reviewer tried and failed to diff,
// or never tried at all.
func reviewerDiffCommands(transcript []oai.Message) []string {
	seen := make(map[string]bool)
	var out []string
	for i := range transcript {
		m := transcript[i]
		if m.Role != oai.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			cat := classifyDiffCommand(commandFromToolCallArgs(tc.Function.Arguments))
			if cat == "" || seen[cat] {
				continue
			}
			seen[cat] = true
			out = append(out, cat)
		}
	}
	sort.Strings(out)
	return out
}

// ungroundedReviewFinding applies the rail to a verdict. It fires ONLY for a
// GO: when the reviewer approved without ever obtaining the diff, failed is
// true and note says plainly that the reviewer approved without ever
// obtaining the diff, and reports which diff-producing commands (if any) were
// attempted. A non-GO verdict returns false — a NO-GO that never diffed is a
// different problem and not this rail's business (per #1552 a wrong NO-GO
// destroys good work).
func ungroundedReviewFinding(transcript []oai.Message, verdict string) (bool, string) {
	if !strings.EqualFold(strings.TrimSpace(verdict), "GO") {
		return false, ""
	}
	if reviewerSawDiff(transcript) {
		return false, ""
	}

	var b strings.Builder
	b.WriteString("reviewer returned GO without ever obtaining the diff of the branch under review: ")
	cmds := reviewerDiffCommands(transcript)
	if len(cmds) == 0 {
		b.WriteString("no diff-producing command (git diff / git show / git log -p) was attempted")
	} else {
		b.WriteString("diff-producing command(s) attempted but no diff evidence was captured: " +
			strings.Join(cmds, ", "))
	}
	b.WriteString("; the transcript contains no `diff --git` line, no `--- a/` + ")
	b.WriteString("`+++ b/` header pair, and no `@@` hunk header, so the approval ")
	b.WriteString("is uncorrelated with the code it covers.")
	return true, b.String()
}
