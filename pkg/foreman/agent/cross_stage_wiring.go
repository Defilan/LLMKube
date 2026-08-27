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
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// branchFactsFromBranch computes the ground-truth BranchFacts for the branch
// under review, anchored at base (a resolved commit SHA, or a ref like
// "main"). files is the changed-file list the caller already resolved against
// base, so it is passed in rather than re-read. It shells out to git via run;
// any failure degrades to the corresponding zero value so the detector cannot
// fire on missing ground truth (it errs toward not flagging).
func branchFactsFromBranch(
	ctx context.Context, workspace, base string, run commandRunner, files []string,
) BranchFacts {
	facts := BranchFacts{FilesChanged: files, BaseSHA: base}
	if base == "" {
		return facts
	}
	if out, err := run(ctx, workspace, nil, "git", "rev-parse", "HEAD"); err == nil {
		facts.HeadSHA = strings.TrimSpace(out)
	}
	if out, err := run(ctx, workspace, nil, "git", "rev-list", "--count", base+"..HEAD"); err == nil {
		if n, aerr := strconv.Atoi(strings.TrimSpace(out)); aerr == nil {
			facts.CommitsAhead = n
		}
	}
	return facts
}

// claimsEdits reports whether the coder's free-text (summary plus the
// submit_result envelope) asserts that it made an edit to the branch -- the
// signal Rule 1 keys on. A coder that returns GO describing a specific change
// ("removed the now-unused <helper>") on a branch carrying zero commits is
// asserting an edit that cannot exist, so it is the case that motivated #1549.
// The scan is deliberately broad on edit verbs (add/remove/change/fix/update/
// delete/modify/implement) so an honest "I fixed X" trips the detector, while a
// bare "done" or a non-edit outcome does not.
func claimsEdits(texts ...string) bool {
	joined := strings.ToLower(strings.Join(texts, "\n"))
	for _, re := range editVerbPatterns {
		if re.MatchString(joined) {
			return true
		}
	}
	return false
}

// editVerbPatterns are the phrasings a coder uses to assert it edited the
// branch. Intentionally broad: an honest "I fixed X" must trip the detector so
// the empty-branch contradiction it motivates can fire.
var editVerbPatterns = []*regexp.Regexp{
	regexp.MustCompile(
		`(?i)\b(add(ed)?|remove[d]?|change[d]?|fix(ed)?|update[d]?|` +
			`delete[d]?|modif(y|ied)|implement(ed)?|edit(ed)?|` +
			`modify|refactor(ed)?)\b`,
	),
}

// applyCrossStageContradictionsForTask is the production wiring for the
// cross-stage contradiction detector (cross_stage.go). It builds a StageClaim
// from what the reviewer stage already asserted -- its verdict, its
// branch-emptiness prose, and the files it named -- and a BranchFacts from the
// ground-truth diff the caller resolved, then records every contradiction the
// detector reports onto the terminal result so the next pipeline step (or a
// human) sees the disagreement.
//
// Non-blocking (#1549): it never changes the verdict; it only surfaces the
// contradiction under Extra["crossStageContradictions"] and logs each one. It
// degrades open -- a missing ground-truth diff (diffErr non-nil) means the
// detector cannot separate a supported claim from an unsupported one, so it
// steps aside, exactly like the other diff-anchored rails.
//
// It runs in the reviewer path of runLLMPath, after the reviewer rails, where
// reviewBase/reviewDiff/reviewDiffErr are already in scope and a
// contradiction can actually occur (the reviewer claims the branch is empty
// when the ground-truth diff is non-empty).
func applyCrossStageContradictionsForTask(
	ctx context.Context, log logr.Logger, workspace, base string,
	diff []string, diffErr error, loopRes *LoopResult,
	verdict foremanv1alpha1.AgenticTaskVerdict,
) {
	if loopRes == nil || loopRes.Terminal == nil || diffErr != nil {
		return
	}
	extra := loopRes.Terminal.Extra
	facts := branchFactsFromBranch(ctx, workspace, base, execCommandRunner, diff)
	named := []string{}
	if extra != nil {
		named, _ = extra["filesTouched"].([]string)
	}
	claim := StageClaim{
		Stage:             "reviewer",
		Verdict:           string(verdict),
		ClaimsEmptyBranch: extra != nil && assertsEmptyBranch(emptyClaimTexts(loopRes.Terminal.Summary, extra)...),
		NamedFiles:        named,
	}
	cs := contradictions(claim, facts)
	if !shouldEscalate(cs) {
		return
	}
	for _, c := range cs {
		log.Info("cross-stage: reviewer claim contradicts the ground-truth branch facts",
			"contradiction", c)
	}
	if extra == nil {
		extra = map[string]any{}
		loopRes.Terminal.Extra = extra
	}
	extra["crossStageContradictions"] = cs
}

// applyCrossStageContradictionsForCoderTask is the production wiring for the
// cross-stage contradiction detector (cross_stage.go) in the coder's settled-GO
// path. It builds a StageClaim from what the coder stage already asserted -- its
// verdict, whether its summary/envelope claims it made an edit, and the files it
// named -- and a BranchFacts from the ground-truth diff the caller resolved, then
// records every contradiction the detector reports onto the terminal result.
//
// This is the case that motivated #1549: a coder returned GO describing a
// specific edit against a branch carrying zero commits, and the gate then
// GATE-PASSed because every check passes trivially on a branch identical to its
// base. Rule 1 ("claims edits on an empty branch") only fires when ClaimsEdits is
// set, which the reviewer wiring never does -- this is the coder half.
//
// Non-blocking (#1549, slice 1): like applyCrossStageContradictionsForTask it
// never changes the verdict; it only surfaces the contradiction under
// Extra["crossStageContradictions"] and logs each one. It degrades open -- a
// missing ground-truth diff (diffErr non-nil) means the detector cannot separate
// a supported claim from an unsupported one, so it steps aside, exactly like the
// other diff-anchored rails.
//
// It runs in the coder path of runLLMPath after the loop has settled on a GO and
// the branch is committed + pushed, where workspace + base + the committed diff
// are in scope and a contradiction can actually occur.
func applyCrossStageContradictionsForCoderTask(
	ctx context.Context, log logr.Logger, workspace, base string,
	diff []string, diffErr error, loopRes *LoopResult,
	verdict foremanv1alpha1.AgenticTaskVerdict,
) {
	if loopRes == nil || loopRes.Terminal == nil || diffErr != nil {
		return
	}
	extra := loopRes.Terminal.Extra
	facts := branchFactsFromBranch(ctx, workspace, base, execCommandRunner, diff)
	named := []string{}
	if extra != nil {
		named, _ = extra["filesTouched"].([]string)
	}
	claim := StageClaim{
		Stage:             "coder",
		Verdict:           string(verdict),
		ClaimsEdits:       claimsEdits(loopRes.Terminal.Summary),
		ClaimsEmptyBranch: extra != nil && assertsEmptyBranch(emptyClaimTexts(loopRes.Terminal.Summary, extra)...),
		NamedFiles:        named,
	}
	cs := contradictions(claim, facts)
	if !shouldEscalate(cs) {
		return
	}
	for _, c := range cs {
		log.Info("cross-stage: coder claim contradicts the ground-truth branch facts",
			"contradiction", c)
	}
	if extra == nil {
		extra = map[string]any{}
		loopRes.Terminal.Extra = extra
	}
	extra["crossStageContradictions"] = cs
}
