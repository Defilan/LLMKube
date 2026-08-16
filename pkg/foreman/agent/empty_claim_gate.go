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

// Empty-claim rail (#1552): an emptiness or unreadability claim a reviewer
// makes about the branch under review ("branch has no commits", "no code
// changes", "commit history is unreadable") is an EVIDENCE claim, not a
// review finding. It is not something the reviewer can assert on a checklist
// pass; it must be backed by the command output that supports it. A reviewer
// that emits such a claim and returns NO-GO with no supporting evidence is
// asserting the branch is empty when it demonstrably is not (the ground-truth
// diff is non-empty), and that false NO-GO has burned review iterations and
// discarded a correct, gate-passed fix (#1552).
//
// The ERROR verdict exists for exactly this ("commit history is unreadable"
// is one of the reviewer prompt's own ERROR definitions) but nothing enforced
// it. This rail enforces it: when the ground-truth branch diff is non-empty
// (so the emptiness claim is contradicted on its face) and the reviewer
// carried no grounded blocking finding to support a rejection, the NO-GO is
// remapped to ERROR (INCOMPLETE + ModelReportedError), which routes to a
// human and preserves the branch, instead of consuming the workload's
// remaining iterations and failing it.
//
// The rail must run BEFORE the grounded-finding demote rail: an ungrounded
// NO-GO that carries an emptiness claim would otherwise be demoted to GO and
// this rail would see GO and no-op.
//
// Degrades open: when the ground-truth diff is unavailable or the reviewer
// cites a grounded blocking finding (real supporting evidence), the verdict
// is left untouched.
//
// Disabled by FOREMAN_EMPTY_CLAIM=0.
package agent

import (
	"context"
	"os"
	"regexp"
	"strings"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
)

// emptyClaimPatterns are the phrasings a reviewer uses to assert the branch
// under review is empty, missing, or unreadable. They are intentionally
// narrow: a vague "looks empty" must not trip the rail, only a claim that the
// branch carries no commits / no code changes / unreadable history.
var emptyClaimPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)no\s+commits?`),
	regexp.MustCompile(`(?i)no\s+code\s+changes?`),
	regexp.MustCompile(`(?i)no\s+(commits|changes|diff)\s+(ahead|at\s+all|present)`),
	regexp.MustCompile(`(?i)empty\s+(branch|diff|commit|history|repository)`),
	regexp.MustCompile(`(?i)branch\s+(is\s+)?empty`),
	regexp.MustCompile(`(?i)nothing\s+to\s+review`),
	regexp.MustCompile(`(?i)commit\s+history\s+is\s+(unreadable|empty)`),
	regexp.MustCompile(`(?i)cannot\s+(read|see|find|clone)\s+(the\s+)?(commit|history|branch|code|repo)`),
	regexp.MustCompile(`(?i)history\s+is\s+unreadable`),
}

// emptyClaimDisabled reports whether the rail is off via
// FOREMAN_EMPTY_CLAIM=0. Default (unset) is enabled.
func emptyClaimDisabled() bool {
	return os.Getenv("FOREMAN_EMPTY_CLAIM") == "0"
}

// assertsEmptyBranch reports whether the reviewer's free-text (summary and
// finding messages) asserts the branch is empty, missing, or unreadable.
func assertsEmptyBranch(texts ...string) bool {
	joined := strings.ToLower(strings.Join(texts, "\n"))
	for _, re := range emptyClaimPatterns {
		if re.MatchString(joined) {
			return true
		}
	}
	return false
}

// emptyClaimTexts collects the reviewer's summary plus every finding message
// so assertsEmptyBranch can scan the reviewer's own words.
func emptyClaimTexts(summary string, extra map[string]any) []string {
	findings, _ := reviewer.ParseFindings(extra)
	out := make([]string, 0, 1+len(findings))
	out = append(out, summary)
	for _, f := range findings {
		out = append(out, f.Message)
	}
	return out
}

// runEmptyClaimRail resolves the ground-truth changed-lines closure for the
// empty-claim rail and applies it. It is the wiring helper runLLMPath calls
// so the rail's diff resolution stays out of the executor's hot path. When
// the ground-truth diff is unavailable (reviewDiffErr non-nil) the closure is
// nil and the rail degrades open.
func runEmptyClaimRail(
	ctx context.Context, log logr.Logger, workspace, base string,
	diff []string, diffErr error, extra map[string]any, summary string,
	verdict foremanv1alpha1.AgenticTaskVerdict,
) (foremanv1alpha1.AgenticTaskVerdict, foremanv1alpha1.AgenticTaskFailureReason) {
	var changed func(string) map[int]bool
	if diffErr == nil {
		changed = reviewerGroundedChangedLines(ctx, log, workspace, base, diff, diffErr)
	}
	return enforceReviewerEmptyClaim(log, extra, summary, verdict, changed)
}

// enforceReviewerEmptyClaim remaps an unsupported emptiness / unreadability
// NO-GO to ERROR (INCOMPLETE + ModelReportedError).
//
// It fires only when ALL of:
//   - the verdict is NO-GO,
//   - the reviewer's text asserts the branch is empty/unreadable,
//   - the ground-truth branch diff is non-empty (changedLines non-nil: the
//     claim is contradicted on its face), and
//   - the reviewer carried no grounded blocking finding (no supporting
//     evidence for a rejection).
//
// On a hit it returns (INCOMPLETE, ModelReportedError) so the caller stores
// the ERROR-shaped verdict and routes the branch to a human. On every other
// input it returns (verdict, "") and the caller proceeds unchanged.
func enforceReviewerEmptyClaim(
	log logr.Logger,
	extra map[string]any,
	summary string,
	verdict foremanv1alpha1.AgenticTaskVerdict,
	changedLines func(string) map[int]bool,
) (foremanv1alpha1.AgenticTaskVerdict, foremanv1alpha1.AgenticTaskFailureReason) {
	if emptyClaimDisabled() ||
		verdict != foremanv1alpha1.AgenticTaskVerdictNoGo ||
		extra == nil || changedLines == nil {
		return verdict, ""
	}
	if !assertsEmptyBranch(emptyClaimTexts(summary, extra)...) {
		return verdict, ""
	}
	// The reviewer cited a grounded blocking finding: that is real supporting
	// evidence for a rejection, so the NO-GO stands (regression guard).
	findings, _ := reviewer.ParseFindings(extra)
	grounded, _ := groundedBlockingFindings(findings, changedLines)
	if len(grounded) >= 1 {
		return verdict, ""
	}

	if extra != nil {
		extra["emptyClaimRemappedToError"] = true
		extra["emptyClaimReason"] = "unsupported branch-emptiness/unreadability " +
			"claim contradicted by a non-empty ground-truth diff"
	}
	log.Info("reviewer empty-claim: unsupported emptiness/unreadability NO-GO "+
		"remapped to ERROR (non-empty diff, no grounded finding)",
		"summary", summary)
	return foremanv1alpha1.AgenticTaskVerdictIncomplete, foremanv1alpha1.FailureModelReportedError
}
