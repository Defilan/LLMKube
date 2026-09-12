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
	"strings"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// diffHasCodeLine reports whether a unified diff body contains an added or
// removed line that is neither blank nor a comment. It is a best-effort line
// heuristic (not a Go parser): a changed line counts as code unless, after
// dropping the +/- marker and trimming, it is empty or begins with a comment
// token (// or a /* * */ block-comment line). Good enough to tell a
// comments-only edit from a real logic change for an advisory signal.
func diffHasCodeLine(diff string) bool {
	for _, l := range strings.Split(diff, "\n") {
		if l == "" {
			continue
		}
		if l[0] != '+' && l[0] != '-' {
			continue
		}
		if strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") {
			continue
		}
		code := strings.TrimSpace(l[1:])
		switch {
		case code == "":
			continue
		case strings.HasPrefix(code, "//"):
			continue
		case strings.HasPrefix(code, "/*"), strings.HasPrefix(code, "*"), strings.HasPrefix(code, "*/"):
			continue
		default:
			return true
		}
	}
	return false
}

// functionalChangeInDiff reports whether the committed diff (base...HEAD) has
// any functional production change: a change to a non-test .go file whose
// added/removed lines are more than comments and blanks. Docs (.md, yaml,
// etc.), tests (_test.go), and comment-only .go edits are non-functional.
// Degrades open (returns true) on any git error so it never spuriously flags.
func functionalChangeInDiff(ctx context.Context, workspace, base string, run commandRunner) bool {
	names, err := run(ctx, workspace, nil, "git", "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return true
	}
	for _, f := range strings.Split(strings.TrimSpace(names), "\n") {
		f = strings.TrimSpace(f)
		if f == "" || !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		out, err := run(ctx, workspace, nil, "git", "diff", "-U0", base+"...HEAD", "--", f)
		if err != nil {
			return true
		}
		if diffHasCodeLine(out) {
			return true
		}
	}
	return false
}

// applyNoFunctionalChange records an advisory when a GO's committed diff has
// no functional production change (all changes are docs, comments, or tests).
// This surfaces the #850 class: a "fix" for a code issue that changes no code,
// where prose can claim behavior that is not implemented and the bite check is
// vacuous. Records-and-logs only; never changes the verdict.
func applyNoFunctionalChange(ctx context.Context, log logr.Logger, base, workspace string, loopRes *LoopResult) {
	if loopRes == nil || loopRes.Terminal == nil {
		return
	}
	if functionalChangeInDiff(ctx, workspace, base, execCommandRunner) {
		return
	}
	log.Info("coder gate: GO changed no functional production code (docs/comments/tests only)")
	if loopRes.Terminal.Extra == nil {
		loopRes.Terminal.Extra = map[string]any{}
	}
	loopRes.Terminal.Extra["noFunctionalChange"] = true
}

// applyNoFunctionalChangeForTask gates the advisory to issue-fix runs and
// takes the diff anchor from the caller, mirroring applyCoderGroundingRailForTask.
// base must be the literal upstream base SHA the task branch was cut from
// (evidenceBaseSHA in runLLMPath); the workspace's local base ref belongs to
// the fork, which lags upstream, so diffing against it sweeps the whole
// intervening upstream delta into the scanned diff and a docs-only GO can read
// as functional (#1769, the coder-side half of #1005). No resolved SHA degrades
// to the payload base branch name, the pre-#1769 posture. Must run after
// repo.Commit so the diff reflects the committed work.
func applyNoFunctionalChangeForTask(
	ctx context.Context, log logr.Logger, task *foremanv1alpha1.AgenticTask, workspace, base string, loopRes *LoopResult,
) {
	if task.Spec.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
		return
	}
	if base == "" {
		base = baseBranchOrDefault(task.Spec.Payload.BaseBranch)
	}
	applyNoFunctionalChange(ctx, log, base, workspace, loopRes)
}
