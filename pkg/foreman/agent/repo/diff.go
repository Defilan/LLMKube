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
// HEAD in the workspace. It stages the working tree (`git add -A`) and
// then runs `git diff --name-only --cached <base>`, which compares the
// index against `base`. This catches uncommitted working-tree changes
// (including new untracked files) that `git diff --name-only base...HEAD`
// would miss because HEAD has not moved yet at gate time.
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
	// Stage working-tree changes so the diff reflects uncommitted edits.
	// The gate runs before any commit, so `...HEAD` would otherwise see
	// nothing; `--cached` against the index catches the coder's in-flight
	// edits (including new untracked files).
	if _, err := runGit(ctx, workspace, baseEnv(), "add", "-A"); err != nil {
		return nil, err
	}
	out, err := runGit(ctx, workspace, baseEnv(), "diff", "--name-only", "--cached", base)
	if err != nil {
		return nil, err
	}
	return parseNameOnly(out), nil
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
