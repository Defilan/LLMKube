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

import "strings"

// fileHunks holds the content (leading +/- stripped) of one file's added and
// removed lines from a `git diff --unified=0` output.
type fileHunks struct {
	Added   []string
	Removed []string
}

// parseUnifiedDiff parses `git diff --unified=0 --src-prefix=a/ --dst-prefix=b/`
// output into per-file added and removed content lines. Added lines are keyed
// by the new-file path (+++ b/PATH); removed lines are keyed by the same path,
// or by the old path (--- a/PATH) when the new side is /dev/null (a deletion).
// Diff headers (---, +++) are never counted as content.
func parseUnifiedDiff(out string) map[string]*fileHunks {
	byFile := map[string]*fileHunks{}
	ensure := func(f string) *fileHunks {
		if byFile[f] == nil {
			byFile[f] = &fileHunks{}
		}
		return byFile[f]
	}
	var cur, aPath string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "--- a/"):
			aPath = strings.TrimPrefix(line, "--- a/")
		case strings.HasPrefix(line, "--- "):
			aPath = "" // /dev/null (added file) etc.
		case strings.HasPrefix(line, "+++ b/"):
			cur = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+++ "):
			cur = aPath // deletion: attribute removed lines to the old path
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") && cur != "":
			ensure(cur).Added = append(ensure(cur).Added, line[1:])
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			key := cur
			if key == "" {
				key = aPath
			}
			if key != "" {
				ensure(key).Removed = append(ensure(key).Removed, line[1:])
			}
		}
	}
	return byFile
}

// assertionTokens are substrings that mark a line as an assertion. Kept
// deliberately small and shape-based: Gomega (Expect/Ω), testify
// (assert./require.), the ContainSubstring matcher, the stdlib t.Error/t.Fatal
// failures, and the got/want comparison idiom.
var assertionTokens = []string{
	"Expect(", "Ω(", "assert.", "require.", "ContainSubstring(",
	"t.Error", "t.Fatal", "!= want", "want !=", "got !=", "!= got",
}

// isAssertionLine reports whether a diff content line looks like a test
// assertion. It is intentionally lenient: false positives only add reviewer
// context, never fail a gate.
func isAssertionLine(s string) bool {
	t := strings.TrimSpace(s)
	for _, tok := range assertionTokens {
		if strings.Contains(t, tok) {
			return true
		}
	}
	return false
}

// assertionErosion counts assertion-shaped removed and added lines in one
// file's hunks and returns the trimmed text of the removed assertions (for the
// reviewer message). Net erosion is removed > added; the caller applies that.
func assertionErosion(fh *fileHunks) (removed, added int, snippets []string) {
	for _, l := range fh.Removed {
		if isAssertionLine(l) {
			removed++
			snippets = append(snippets, strings.TrimSpace(l))
		}
	}
	for _, l := range fh.Added {
		if isAssertionLine(l) {
			added++
		}
	}
	return removed, added, snippets
}

// firstN returns the first n elements of s, or all of s when shorter. Used to
// cap how many removed-assertion snippets appear in the advisory.
func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
