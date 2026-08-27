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
	// Preserve the structured evidence behind the contradiction strings: the
	// claim that was evaluated, the ground facts it was checked against, and
	// the contradictions themselves (#1674). The strings alone say *that*
	// something disagreed; the claim + facts say *what* disagreed with *what*,
	// so a human or a later stage can judge whether the disagreement is real
	// without re-deriving the inputs. Extra["crossStageContradictions"] is
	// left exactly as it was -- this only adds a key, it does not replace one.
	extra["crossStageEvidence"] = crossStageEvidence{
		Claim:          claim,
		Facts:          facts,
		Contradictions: cs,
	}
}
