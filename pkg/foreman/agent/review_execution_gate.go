package agent

import (
	"regexp"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// review_execution_gate.go is the deterministic rail behind #1618. The
// reviewer rubric's Section K mandates two execution runs (run the diff's own
// new test; probe one near-miss) whenever the diff touches `.go` files, but in
// the first live Section-K review the reviewer performed every read and skipped
// both runs, disclosing the omission in onTrust instead. Numbered step
// sequences are followed faithfully; checklist prose is treated as advisory.
// This rail makes the execution mandate observable: when the diff touches a
// `.go` file, a GO whose transcript never ran `go test` is marked in its
// record (the skipped rail is recorded on the result's extra) so a later stage
// can see the approval did not execute the change it covers. Marking, not
// demotion: the verdict stands as returned, and flipping a GO to NO-GO is a
// later decision for when the fleet shows the runs fit the turn budget.
//
// These functions are deterministic and model-free: they run over a stored
// transcript ([]oai.Message) and the ground-truth diff files, making no git
// calls. They mirror the shape of reviewer_diff_gate.go (walk a transcript,
// correlate tool results to the assistant tool_call that produced them, return
// a finding) and the mark-but-do-not-demote pattern of rail_skip.go (record the
// rail as skipped on extra, leave the verdict alone).

// reGoTestCommand matches a shell command that actually invokes `go test`. It
// is anchored on a command position (start of a line, or right after a `;`,
// `&`, or `|` command separator) so a mere mention — `grep "go test"
// Makefile`, or a read_file of a test file whose text carries the literal `go
// test` — does not satisfy the rail. The space after `test` separates the
// subcommand from the package pattern (`go test ./pkg/...`).
var reGoTestCommand = regexp.MustCompile(`(?m)(^|[;&|]\s*)go\s+test\b`)

// transcriptRanGoTest reports whether any executed bash invocation in the
// transcript is a `go test` run. It scans assistant tool_call arguments (via
// commandFromToolCallArgs, the same correlation the diff gate uses) and
// tool-role content, but a tool-role message counts only when a bash tool call
// produced it: a read_file of a file that merely contains the string `go test`
// must not satisfy the rail. The tool-role correlation uses callNameByToolCallID
// to recover the tool name from the assistant tool_call that produced the
// message. An empty transcript yields false without panic.
func transcriptRanGoTest(transcript []oai.Message) bool {
	names := callNameByToolCallID(transcript)
	for i := range transcript {
		m := transcript[i]
		switch m.Role {
		case oai.RoleAssistant:
			for _, tc := range m.ToolCalls {
				if tc.Function.Name != "bash" {
					continue
				}
				if reGoTestCommand.MatchString(commandFromToolCallArgs(tc.Function.Arguments)) {
					return true
				}
			}
		case oai.RoleTool:
			if names[m.ToolCallID] != "bash" {
				continue
			}
			if reGoTestCommand.MatchString(commandFromToolCallArgs(toolContent(m))) {
				return true
			}
		}
	}
	return false
}

// enforceReviewerExecution applies the review-execution rail (#1618) to a
// reviewer's verdict. It fires only for a GO on a diff that touches a `.go`
// file: the Section-K execution runs are mandatory for Go diffs but are
// exempt for docs/YAML-only changes. A GO whose transcript never ran `go test`
// is marked — the skipped rail is recorded on extra so the verdict is
// distinguishable afterwards from one that earned the execution check — and
// the verdict stands as the model returned it. A transcript that did run `go
// test` leaves the verdict untouched. Marking, not demotion: demotion is a
// later flip once the fleet shows the runs fit the turn budget. A non-GO
// verdict is not this rail's business and returns untouched.
func enforceReviewerExecution(
	log logr.Logger,
	extra map[string]any,
	diffFiles []string,
	verdict foremanv1alpha1.AgenticTaskVerdict,
	transcript []oai.Message,
) foremanv1alpha1.AgenticTaskVerdict {
	if extra == nil {
		log.Info("reviewer execution: extra is nil; cannot record the rail as skipped")
		return verdict
	}
	if verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		return verdict
	}
	// Non-.go diffs are exempt: the mandatory execution runs only apply when
	// the diff touches `.go` files. hasSourceFile defaults to [".go"] when exts
	// is nil, so this is the Go-only guard.
	if !hasSourceFile(diffFiles, nil) {
		return verdict
	}
	if transcriptRanGoTest(transcript) {
		return verdict
	}

	// The GO on a Go diff never ran `go test`. Record the skipped rail so the
	// approval is distinguishable afterwards from one that earned the execution
	// check; the verdict stands as returned. Demotion is a later flip once the
	// fleet shows the runs fit the turn budget.
	recordRailSkipped(extra, railExecution, skipReasonNoTestRun)
	log.Info("reviewer execution: GO on a .go diff with no `go test` run in the transcript; recorded the rail as skipped",
		"verdict", verdict)
	return verdict
}
