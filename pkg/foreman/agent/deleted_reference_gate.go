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

// Deleted-reference rail: a flag, not a block. Codebases encode decisions in
// comments that cite the issue motivating them ("this exists because of #N").
// Those comments are the cheapest available signal that a piece of code is
// load-bearing for a reason not obvious from the code itself. Observed: an
// agent deleted a configuration field whose doc comment cited the issue it was
// fixing AND two earlier issues explaining why the behaviour must not simply
// be removed; the deletion reintroduced the regression those issues exist to
// prevent, and because the tests were rewritten to match, every automated
// stage passed.
//
// This rail mechanically scans the REMOVED lines of the diff for issue/PR
// references and records them on the task's extra map so the reviewer and the
// human can see what is being undone. It deliberately does NOT change the
// verdict: removing superseded code is legitimate and common. The requirement
// is that the removal be STATED, not prevented.
//
// It pairs with the pre-flight staleness check: that one catches "the tree
// already fixed this issue" before dispatch, this one catches "this diff is
// removing the fix" after.
//
// Disabled by FOREMAN_DELETED_REFERENCE=0.
package agent

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// deletedIssueRefRe matches a full owner/repo#N reference first (so the full
// form is captured when present) and otherwise a bare #N reference. Go's
// regexp is leftmost-first: at the leftmost position the first alternative is
// tried, which yields the longest, owner-qualified form where one exists.
var deletedIssueRefRe = regexp.MustCompile(`[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+#\d+|#\d+`)

// deletedReferenceDisabled reports whether this rail is off via
// FOREMAN_DELETED_REFERENCE=0. Default (unset) is enabled.
func deletedReferenceDisabled() bool {
	return os.Getenv("FOREMAN_DELETED_REFERENCE") == "0"
}

// deletedIssueReferences returns the issue/PR references found on the REMOVED
// lines of a unified diff, deduplicated and sorted so the output is
// deterministic and testable.
//
// Only removed lines are considered: those starting with "-" but NOT "---"
// (the latter is the "a/..." file header, not a removed line). Added and
// context lines are ignored entirely. Both the bare "#N" and the full
// "owner/repo#N" forms are matched. Taking a diff string rather than a repo
// path keeps the function pure, so it is testable without git.
func deletedIssueReferences(unifiedDiff string) []string {
	seen := make(map[string]struct{})
	for _, line := range strings.Split(unifiedDiff, "\n") {
		if !strings.HasPrefix(line, "-") || strings.HasPrefix(line, "---") {
			continue
		}
		for _, m := range deletedIssueRefRe.FindAllString(line, -1) {
			seen[m] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// recordDeletedIssueReferences records the removed-line issue references on the
// task's extra map (extra["deletedIssueReferences"] plus a short
// human-readable extra["deletedReferenceNote"]) so the reviewer and the human
// can see what tracked work the diff is undoing. It is a flag, not a block: it
// never sets, clears, or modifies the verdict.
func recordDeletedIssueReferences(extra map[string]any, unifiedDiff string) {
	if deletedReferenceDisabled() || extra == nil || unifiedDiff == "" {
		return
	}
	refs := deletedIssueReferences(unifiedDiff)
	if len(refs) == 0 {
		return
	}
	extra["deletedIssueReferences"] = refs
	extra["deletedReferenceNote"] = fmt.Sprintf(
		"diff removes code citing issue/PR reference(s) %s; state whether the removal is intentional",
		strings.Join(refs, ", "))
}
