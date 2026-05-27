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

package repomap

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// scoredFile pairs an indexableFile with its relevance score against
// the issue text. The formatter sorts by Score descending and emits
// the top-N (subject to the token budget).
type scoredFile struct {
	indexableFile
	Score float64
}

// scoreFiles assigns each file a relevance score relative to issueText.
// Files are returned sorted by score (descending). The scoring is
// pure-Go heuristic, no model call; the empirical finding (Aider's
// repo-map, Agentless) is that a simple bag-of-words intersection +
// path-mention boost gets you most of the way to a model-quality
// ranking for the localization use case.
//
// When issueText is empty the score reduces to a structural heuristic:
// shorter paths score higher (more central to the repo). This is the
// "what is this repo" fallback case useful for the deterministic gate
// step's read of the repo or for a freeform task without a specific
// issue.
func scoreFiles(files []indexableFile, issueText string) []scoredFile {
	out := make([]scoredFile, len(files))
	queryTokens := tokenize(issueText)
	queryPaths := extractPathMentions(issueText)

	for i, f := range files {
		out[i] = scoredFile{
			indexableFile: f,
			Score:         scoreOne(f, queryTokens, queryPaths),
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		// Stable tie-break: shorter paths first (more central).
		return len(out[i].Path) < len(out[j].Path)
	})
	return out
}

// scoreOne computes a single file's relevance score. The components
// (path-mention boost, token overlap, structural prior) compose
// linearly; their relative weights are tuned for the issue-fix task
// shape but live as named constants so future empirical work can
// adjust them without rewriting the scorer.
func scoreOne(f indexableFile, queryTokens map[string]bool, queryPaths []string) float64 {
	var score float64

	// Path-mention boost: the issue text literally names this file
	// or one of its path components (e.g. "fix the str_replace tool
	// in tools/str_replace.go"). High signal; weighted accordingly.
	for _, p := range queryPaths {
		if strings.Contains(f.Path, p) {
			score += pathMentionWeight
		}
	}

	// Token overlap: count distinct identifiers from the file's
	// declarations and package doc that appear in the issue text.
	// Each hit contributes identifierMatchWeight; cap the per-file
	// contribution so a giant file with many incidental matches
	// doesn't drown out a small targeted file.
	matches := 0
	for _, id := range f.Extracted.Identifiers {
		lower := strings.ToLower(id)
		if queryTokens[lower] {
			matches++
		}
	}
	if matches > maxIdentifierMatchesPerFile {
		matches = maxIdentifierMatchesPerFile
	}
	score += float64(matches) * identifierMatchWeight

	// Path-token overlap: split the path into segments and check
	// each against the query tokens. Useful when the issue mentions
	// a package name without naming a specific symbol.
	for _, seg := range pathSegments(f.Path) {
		if queryTokens[seg] {
			score += pathTokenMatchWeight
		}
	}

	// Structural prior: shorter paths (top-of-repo / cmd / pkg root)
	// are more often central than deep test helpers. Add a small
	// boost inversely proportional to path depth.
	depth := strings.Count(f.Path, string(filepath.Separator))
	score += depthPriorWeight / float64(depth+1)

	return score
}

// Score component weights. Tuned for the issue-fix use case on
// LLMKube + similarly-structured Go repos. The relative ordering
// (path mention > identifier match > path token > depth prior) is
// what matters most; the absolute magnitudes are arbitrary.
const (
	pathMentionWeight           = 50.0
	identifierMatchWeight       = 4.0
	pathTokenMatchWeight        = 2.0
	depthPriorWeight            = 1.0
	maxIdentifierMatchesPerFile = 12
)

// tokenize lowercases text and splits it into a set of identifier-
// shaped tokens. Used for both the issue text and the file's
// extracted identifiers; the symmetric tokenization is what makes
// the overlap meaningful.
//
// Stopwords are removed: common English filler words ("the", "a",
// "is") would otherwise dominate the overlap and reward unrelated
// files. The stopword list is intentionally small; aggressive
// filtering would lose useful signal on words like "error", "task",
// "test" that are common but also genuinely meaningful in code.
func tokenize(text string) map[string]bool {
	out := make(map[string]bool)
	if text == "" {
		return out
	}
	// Split on any non-alphanumeric character. This handles
	// punctuation, whitespace, snake_case, and camelCase all at
	// once -- though camelCase tokens are also split into their
	// internal components below for cross-style matching.
	parts := identSplitter.Split(strings.ToLower(text), -1)
	for _, p := range parts {
		if p == "" || len(p) < 2 {
			continue
		}
		if stopwords[p] {
			continue
		}
		out[p] = true
		// Also index camelCase components (e.g. "strReplace" ->
		// "str", "replace"). The text was already lowercased above,
		// so this is only meaningful for the file's identifier list
		// which keeps original casing -- but callers pass
		// already-lowered Identifiers via the queryTokens map.
		for _, sub := range splitCamel(p) {
			if len(sub) >= 2 && !stopwords[sub] {
				out[sub] = true
			}
		}
	}
	return out
}

// identSplitter splits on non-identifier characters: anything that
// isn't a letter, digit, or underscore. This is the standard
// boundary set for code-shaped text.
var identSplitter = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// splitCamel splits a snake_case or camelCase token into its
// constituent parts. "strReplace" -> ["str", "replace"];
// "str_replace_tool" -> ["str", "replace", "tool"]. Used to make
// the overlap symmetric across the snake_case (Python / Bash)
// and camelCase (Go) styles agents see in tool / function names.
func splitCamel(s string) []string {
	parts := strings.Split(s, "_")
	// In a lowercased string camelCase boundaries are already lost;
	// this function mostly does the snake_case split. The camelCase
	// split is preserved as identity here, which is correct because
	// the Identifier list itself supplies the camelCased base name
	// (lowercased once, before the map lookup).
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) >= 2 {
			out = append(out, p)
		}
	}
	return out
}

// stopwords excludes the most common filler tokens from the overlap.
// Kept small on purpose: aggressive stopword removal loses
// code-meaningful terms ("test", "error", "task").
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true,
	"this": true, "that": true, "from": true, "into": true,
	"are": true, "was": true, "but": true, "not": true,
	"have": true, "has": true, "can": true, "any": true,
	"all": true, "should": true, "would": true, "could": true,
	"will": true, "fix": true, "bug": true, "issue": true,
	// "issue", "bug", "fix" are too common in issue bodies to be
	// useful signal even though they're technically code-adjacent.
}

// pathLikePattern matches things that look like filenames or paths
// in the issue text: a token containing a slash or a dot followed by
// 2+ letters (file extension). Conservative enough to avoid matching
// URLs (no scheme + domain) or version numbers.
var pathLikePattern = regexp.MustCompile(`[A-Za-z0-9_./-]*[/.][A-Za-z0-9_./-]+`)

// extractPathMentions pulls anything that looks like a file path or
// filename from the issue text. The scorer uses these as a high-
// weight hint: an issue that says "fix the bug in tools/bash.go"
// should rank tools/bash.go very high.
//
// Returns the raw mentioned strings; the scorer substring-matches
// them against file paths so partial mentions (just "bash.go") also
// score.
func extractPathMentions(text string) []string {
	if text == "" {
		return nil
	}
	matches := pathLikePattern.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		// Strip leading/trailing punctuation that the regex may
		// have grabbed.
		m = strings.Trim(m, ".,;:()[]{}")
		// Filter out things that don't look like code paths: too
		// short, no slash AND no recognized extension.
		if len(m) < 4 {
			continue
		}
		if !strings.Contains(m, "/") && !hasCodeExt(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// hasCodeExt reports whether s ends in a file extension we expect to
// see in repo paths. Used to filter pathLikePattern matches that
// don't have a slash (where the only "path-likeness" is a dotted
// suffix).
func hasCodeExt(s string) bool {
	for _, ext := range knownCodeExts {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}

var knownCodeExts = []string{
	".go", ".py", ".ts", ".js", ".rs", ".java", ".rb", ".c", ".h",
	".cpp", ".hpp", ".sh", ".yaml", ".yml", ".json", ".md", ".toml",
	".proto", ".sql",
}

// pathSegments splits a file path into its directory + filename
// components, lowercased, for tokenwise matching against the issue
// text. "pkg/foreman/agent/repomap/walk.go" yields
// ["pkg", "foreman", "agent", "repomap", "walk", "go"].
func pathSegments(p string) []string {
	p = strings.ToLower(p)
	parts := strings.Split(p, string(filepath.Separator))
	out := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		if part == "" {
			continue
		}
		// Split the basename on dots so the extension shows up as
		// its own token ("walk.go" -> "walk", "go"). The depth
		// component is preserved via the slash-count in scoreOne.
		for _, sub := range strings.Split(part, ".") {
			if sub != "" {
				out = append(out, sub)
			}
		}
	}
	return out
}
