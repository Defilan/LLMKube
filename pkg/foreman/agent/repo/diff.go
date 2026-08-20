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

package repo

import (
	"context"
	"fmt"
	"strings"
)

// DiffNameOnly returns the file paths that differ between `base` and
// HEAD in the workspace. Uses `git diff --name-only base...HEAD`, the
// three-dot form, which compares HEAD against the merge-base with
// `base`: it is the right shape for "files this branch touched on
// top of base," excluding files that changed on `base` since the
// branch diverged.
//
// Used by the reviewer-result executor path to ground-truth
// `submit_result.extra.filesTouched` (a model-reported field that
// devstral, in particular, confabulates on multi-file diffs even when
// its earlier tool calls returned correct data; see #582).
//
// An empty diff is not an error: the reviewer's workspace will be on
// an unrelated empty branch off `base` until the model itself runs
// the mandatory Step 1 `git fetch + checkout`. If the model skipped
// Step 1, the diff really is empty -- the executor surfaces that as
// "no files touched" and the caller decides what to do with the
// discrepancy between an empty ground truth and a non-empty model
// claim.
func DiffNameOnly(ctx context.Context, workspace, base string) ([]string, error) {
	if workspace == "" {
		return nil, fmt.Errorf("DiffNameOnly: workspace is required")
	}
	if base == "" {
		return nil, fmt.Errorf("DiffNameOnly: base ref is required")
	}
	out, err := runGit(ctx, workspace, baseEnv(), "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	return parseNameOnly(out), nil
}

// DiffAdded returns the subset of the branch's diff whose status is ADDED
// (git status "A"), using `git diff --name-status base...HEAD`. The three-dot
// form matches DiffNameOnly, so the two agree on which files the branch
// touched; this only narrows to the additions.
//
// The content-based test-coverage vouch (#1610) reads the body of a diff-ADDED
// test file to check it imports the module it covers. Reading only added files
// keeps the vouch scoped to what this branch created — a test the branch added
// is the coverage we are vouching for — and bounds the file reads to the diff
// rather than the whole tree. An empty result is not an error.
func DiffAdded(ctx context.Context, workspace, base string) ([]string, error) {
	if workspace == "" {
		return nil, fmt.Errorf("DiffAdded: workspace is required")
	}
	if base == "" {
		return nil, fmt.Errorf("DiffAdded: base ref is required")
	}
	out, err := runGit(ctx, workspace, baseEnv(), "diff", "--name-status", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	return parseNameStatusAdded(out), nil
}

// DiffAddedLines returns the lines the branch ADDED to a single file, using
// `git diff -U0 base...HEAD -- file`. It is the content source for the
// modified-file half of the content-based test-coverage vouch (#1616): a test
// file this branch merely modified may have imported the module under test
// long before the branch touched it, so the file's whole content is not
// evidence of new coverage. The added LINES are: a test that gains
// `from pkg.module import ...` in this diff is evidence; one that merely
// already contained it is not.
//
// The three-dot form matches DiffNameOnly / DiffAdded, so the base boundary
// agrees. `-U0` drops context lines so the result is exactly the additions.
// Each `+` line is stripped of its leading `+`; the `+++ b/<file>` header is
// skipped. Returns the added lines joined by newlines ("" when the file has
// no additions, e.g. a pure deletion). An empty result is not an error.
func DiffAddedLines(ctx context.Context, workspace, base, file string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("DiffAddedLines: workspace is required")
	}
	if base == "" {
		return "", fmt.Errorf("DiffAddedLines: base ref is required")
	}
	if file == "" {
		return "", fmt.Errorf("DiffAddedLines: file path is required")
	}
	out, err := runGit(ctx, workspace, baseEnv(), "diff", "-U0", base+"...HEAD", "--", file)
	if err != nil {
		return "", err
	}
	return parseAddedLines(out), nil
}

// parseAddedLines collects the added lines from a unified diff: every line
// starting with `+` (minus the `+++` new-file header), with the leading `+`
// removed and CRLF line endings normalized. Returns "" when there are no
// additions.
func parseAddedLines(diff string) string {
	if diff == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "+++") {
			continue
		}
		if len(line) > 1 && strings.HasPrefix(line, "+") {
			b.WriteString(line[1:])
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// parseNameStatusAdded keeps only the ADDED ("A") paths from tab-separated
// `git diff --name-status` output. Rename/copy rows ("R..", "C..") are not
// additions of a new path and are excluded: a renamed test file's coverage was
// already present on the base, so it is not the new coverage the vouch targets.
// Returns nil (not []string{}) when nothing was added, so a DeepEqual against
// a nil expectation works in callers and tests.
func parseNameStatusAdded(out string) []string {
	if out == "" {
		return nil
	}
	var added []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		if f[0] == "A" {
			added = append(added, f[len(f)-1])
		}
	}
	return added
}

// parseNameOnly splits a `git diff --name-only` output into a clean
// slice of paths. Strips empty lines and trims surrounding whitespace
// off each entry. Returns nil (not []string{}) when the output yields
// no paths, so DeepEqual against a nil expectation works in callers
// and tests.
func parseNameOnly(out string) []string {
	if out == "" {
		return nil
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		paths = append(paths, l)
	}
	return paths
}
