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

// Staleness pre-flight check.
//
// Issues go stale. The tree moves, a fix lands, and the issue text still
// describes the original symptom. An agent handed that text will faithfully
// implement it — including by REMOVING the fix that superseded it. Observed in
// this fleet: an issue was queued whose fix was already on the default branch;
// the live code carried a comment citing that very issue number and explaining
// the trade-off it resolved. The coder deleted the field and its comment,
// reintroducing the regression the comment warned about, and rewrote the tests
// to match. Coder, gate and reviewer all passed it. A second issue in the same
// batch had been fully superseded by a merged PR; it was caught only because a
// human checked before queueing.
//
// This file detects the staleness. It is deliberately built as PURE functions
// that take already-gathered text (git-log output, grep output) and return
// findings. Nothing here shells out: taking strings makes every signal
// unit-testable without a repository, which is the whole point. Where the
// gather actually runs (dispatch time, wired to the executor or controller) is
// a decision for a human; this package owns only the detection.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// stalenessToggleEnv toggles the pre-flight staleness check off. Setting it to
// "0" disables; any other value (including unset) leaves it ENABLED.
const stalenessToggleEnv = "FOREMAN_STALENESS_CHECK"

// stalenessSignals carries what the pre-flight check found for a single issue.
// Each field holds deduplicated, stably-sorted evidence: the commits that
// reference the issue and the live-code locations that cite it.
type stalenessSignals struct {
	// Issue is the issue number the signals were gathered for.
	Issue int
	// Commits are commit references (short SHAs) whose subject references the
	// issue, gathered from `git log --oneline --grep="#N"` on the base branch.
	Commits []string
	// CodeRefs are "file:line" entries in live code or comments that cite the
	// issue, gathered from `grep -rn "#N"` across the tree.
	CodeRefs []string
}

// found reports whether the check surfaced any staleness evidence at all.
func (s stalenessSignals) found() bool {
	return len(s.Commits) > 0 || len(s.CodeRefs) > 0
}

// commitsReferencingIssue parses the output of
// `git log --oneline --grep="#N"` into commit references. Each non-empty line
// is one commit; its first whitespace-delimited field (the short SHA) is the
// reference returned. An empty input yields an empty slice, never nil, and
// never panics. Results are deduplicated and returned in stable sorted order.
func commitsReferencingIssue(gitLogOutput string, issue int) []string {
	_ = issue // the grep already filtered; the number is retained for symmetry.
	refs := make([]string, 0, 4)
	for _, line := range strings.Split(gitLogOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		refs = append(refs, fields[0])
	}
	return dedupeSorted(refs)
}

// codeCitingIssue parses the output of `grep -rn "#N"` into "file:line"
// entries. A line contributes its "file:line" prefix when it cites the issue.
//
// The match is exact: it must match `#1550` but NOT `#15500` (a longer number
// sharing a numeric prefix) and NOT `#155` (a shorter number). The guard is on
// the right edge of the number — the character following the digits must not
// itself be a digit — so `#15500` is never mistaken for `#1550`.
//
// An empty input yields an empty slice, never nil, and never panics. Results
// are deduplicated and returned in stable sorted order.
func codeCitingIssue(grepOutput string, issue int) []string {
	refs := make([]string, 0, 4)
	for _, line := range strings.Split(grepOutput, "\n") {
		if line == "" {
			continue
		}
		if !lineCitesIssue(line, issue) {
			continue
		}
		refs = append(refs, fileLineOf(line))
	}
	return dedupeSorted(refs)
}

// lineCitesIssue reports whether line cites `#<issue>` as a complete number:
// the `#` is immediately followed by exactly the issue's digits, and the
// character after those digits (if any) is not a digit. This is what keeps
// `#15500` from matching `#1550` and `#155` from matching `#1550`.
func lineCitesIssue(line string, issue int) bool {
	target := fmt.Sprintf("#%d", issue)
	for i := 0; i+len(target) <= len(line); i++ {
		if line[i] != '#' {
			continue
		}
		if !strings.HasPrefix(line[i:], target) {
			continue
		}
		end := i + len(target)
		if end < len(line) && isDigit(line[end]) {
			continue
		}
		return true
	}
	return false
}

// fileLineOf extracts the "file:line" prefix from a `grep -rn` output line,
// whose shape is "file:line:content". It returns the first two
// colon-separated fields joined by a colon. A line with no colon is returned
// as-is so the caller still records something for it.
func fileLineOf(grepLine string) string {
	parts := strings.SplitN(grepLine, ":", 3)
	switch len(parts) {
	case 0:
		return grepLine
	case 1:
		return parts[0]
	default:
		return parts[0] + ":" + parts[1]
	}
}

// stalenessDisabled reports whether the pre-flight staleness check has been
// turned off via FOREMAN_STALENESS_CHECK=="0". Default (unset or any other
// value) is ENABLED, so this returns false.
func stalenessDisabled() bool {
	return os.Getenv(stalenessToggleEnv) == "0"
}

// gatherStaleness assembles the pure signals from already-gathered text. It is
// the single entry point a future call site uses: it never shells out and is
// safe to call with empty strings (which yields an empty, non-founding result).
func gatherStaleness(issue int, gitLogOutput, grepOutput string) stalenessSignals {
	return stalenessSignals{
		Issue:    issue,
		Commits:  commitsReferencingIssue(gitLogOutput, issue),
		CodeRefs: codeCitingIssue(grepOutput, issue),
	}
}

// checkStaleness is the end-to-end assembly a future call site would use: it
// respects the FOREMAN_STALENESS_CHECK toggle and returns the note to attach
// to a task — an empty string when the check is disabled or when nothing was
// found. It is pure with respect to its text inputs and safe to call with
// empty strings. Where the gather actually runs is a decision for a human;
// this function is the reusable seam between the gathered text and the note.
func checkStaleness(issue int, gitLogOutput, grepOutput string) string {
	if stalenessDisabled() {
		return ""
	}
	return stalenessNote(gatherStaleness(issue, gitLogOutput, grepOutput))
}

// applyStalenessCheckForTask is the production entry point that wires the
// pre-flight staleness check (#1550) into the coder's pre-dispatch path. It is
// the command seam between the pure checkStaleness entry point and the real
// workspace: it gathers the two text inputs the check needs by shelling out
// once (git log on the base branch, grep across the tree), feeds them to
// checkStaleness, and, when the check found something, prepends the returned
// note to the task's prompt so the coder reads the citing code before editing.
//
// It runs once per issue-fix task, before buildUserPrompt assembles the prompt,
// so the note lands in the coder's first turn rather than after a model has
// already started (and could delete the very fix it should have left alone).
// The git log and grep are run in the task workspace against the branch the
// executor just cut from the base, so `git log --grep` sees the base branch's
// history and `grep -rn` sees the live tree. Best-effort: any git/grep error
// is logged and the check is skipped, so an unreachable repo never blocks a
// task. It gates to issue-fix tasks (the only kind that carries an issue
// number worth checking), mirroring the sibling apply*ForTask wrappers.
func applyStalenessCheckForTask(
	ctx context.Context, log logr.Logger, task *foremanv1alpha1.AgenticTask, workspace string,
) {
	if task.Spec.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
		return
	}
	if task.Spec.Payload.Issue <= 0 {
		return
	}
	issue := int(task.Spec.Payload.Issue)
	base := baseBranchOrDefault(task.Spec.Payload.BaseBranch)
	// grep exits 1 when it finds no matches; that is the common, healthy
	// case (the issue is not cited in live code), not a failure, so its
	// output is empty rather than a reason to skip. Anything else is logged
	// and skipped so an unexpected git error never blocks the task.
	gitArgs := []string{"log", "--oneline", "--grep=#" + strconv.Itoa(issue), base}
	gitLog, gitErr := execCommandRunner(ctx, workspace, nil, "git", gitArgs...)
	if gitErr != nil {
		log.Info("staleness pre-flight: git log failed; skipping", "issue", issue, "err", gitErr.Error())
		return
	}
	grep, grepErr := execCommandRunner(ctx, workspace, nil, "grep", "-rn", "#"+strconv.Itoa(issue), ".")
	if grepErr != nil {
		if exitErr, ok := grepErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			grepErr = nil
		} else {
			log.Info("staleness pre-flight: grep failed; skipping", "issue", issue, "err", grepErr.Error())
			return
		}
	}
	note := checkStaleness(issue, gitLog, grep)
	if note == "" {
		return
	}
	log.Info("staleness pre-flight: tree may already address this issue", "issue", issue)
	task.Spec.Payload.PromptPrefix = note + "\n\n" + task.Spec.Payload.PromptPrefix
}

// stalenessNote renders the signals into a short human-readable note suitable
// for attaching to a task's extra map. It names what was found so the coder
// must read the citing code before editing and the reviewer receives the same
// references as required context for its regression check. An empty note is
// returned when nothing was found (or, by the caller, when the check is
// disabled).
func stalenessNote(s stalenessSignals) string {
	if !s.found() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Staleness pre-flight for #%d: the tree may already address this issue.", s.Issue)
	if len(s.Commits) > 0 {
		fmt.Fprintf(&b, " %d commit(s) reference it: %s.", len(s.Commits), strings.Join(s.Commits, ", "))
	}
	if len(s.CodeRefs) > 0 {
		fmt.Fprintf(&b, " Live code cites it at: %s.", strings.Join(s.CodeRefs, ", "))
	}
	b.WriteString(" Read these before editing; declare whether this work extends or replaces the cited code.")
	return b.String()
}

// isDigit reports whether c is an ASCII decimal digit.
func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// dedupeSorted removes duplicate strings and returns them in stable sorted
// order. It always returns a non-nil slice (empty input yields an empty, not
// nil, slice).
func dedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
