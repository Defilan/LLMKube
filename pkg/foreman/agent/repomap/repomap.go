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

// Package repomap builds a token-budgeted summary of a cloned workspace
// scored against an issue's text. The foreman-agent's executor calls
// Build after the workspace clone but before the model loop starts;
// the summary is prepended to the coder Agent's user prompt so the
// model knows where to look without having to grep + read_file its
// way through the tree.
//
// v0.3 #560 introduces this as the harness-tightening repo-map pass.
// References: Aider's repo-map (ctags-based weighted file summary);
// Agentless (arXiv 2407.01489) "localize then edit" two-pass; Moatless
// hybrid. The empirical finding across all three is that local models
// gain 15-25 percentage points of resolve rate when handed a
// pre-computed map rather than left to discover the relevant files
// via tool calls.
//
// v0.3 scope: Go source files only, regex-light extraction via the
// stdlib go/ast + go/parser (no CGO dependency). Other languages
// (Python, TS, YAML, Bash) are tracked as v0.3.x follow-ups.
package repomap

import (
	"context"
	"fmt"
)

// DefaultTokenBudget is the soft cap on the summary's approximate
// token size. 4K tokens (~16 KB) is enough to surface the top ~15-25
// files of a medium repo with file-level signatures. Larger budgets
// hit diminishing returns: the model's lost-in-the-middle blind spot
// grows faster than its ability to use additional structure.
const DefaultTokenBudget = 4096

// DefaultMaxFiles caps how many files appear in the summary, even
// when the token budget would allow more. Some repos are very large
// and a 4K budget would happily include 100+ tiny files at the
// expense of fewer big files with more useful structure. Empirically
// 25-40 files is the sweet spot for issue-fix tasks.
const DefaultMaxFiles = 30

// Options tune the Build behavior. Zero values fall back to the
// Default* constants above.
type Options struct {
	// TokenBudget bounds the summary size (approximate chars/4).
	// Zero uses DefaultTokenBudget.
	TokenBudget int

	// MaxFiles bounds the count of distinct files the summary
	// includes, even when TokenBudget allows more. Zero uses
	// DefaultMaxFiles.
	MaxFiles int

	// SkipPatterns lists path globs to exclude from the walk (in
	// addition to the default skip set: vendor/, node_modules/,
	// .git/, generated, etc.). Useful for repo-specific exclusions.
	// Glob syntax matches filepath.Match.
	SkipPatterns []string
}

// Build walks workspace, scores each indexable file against the
// issue text, and returns a token-budgeted markdown summary suitable
// for prepending to a coder Agent's user prompt.
//
// Empty issueText returns a generic top-of-repo summary scored by
// file size + path depth (smaller path = more central). Empty
// workspace returns an empty string with nil error so callers can
// degrade gracefully.
//
// The summary is markdown with the shape:
//
//	## Repository overview
//
//	<repo-summary count>: top files weighted by relevance to the issue.
//	Re-read any file in full with the `read_file` tool when you need
//	more context.
//
//	### path/to/file.go
//	(package doc, first 1-2 lines)
//	- func Foo(args) ret
//	- type Bar struct
//	- func (b *Bar) Method()
//
// Build is read-only on the workspace; it never modifies files. It
// is safe to call concurrently against different workspaces (every
// invocation builds its own walker state). It does NOT call into
// the model; the scoring is purely heuristic.
func Build(_ context.Context, workspace string, issueText string, opts Options) (string, error) {
	if workspace == "" {
		return "", nil
	}
	budget := opts.TokenBudget
	if budget <= 0 {
		budget = DefaultTokenBudget
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}

	files, err := Walk(workspace, opts.SkipPatterns)
	if err != nil {
		return "", fmt.Errorf("repomap walk: %w", err)
	}
	if len(files) == 0 {
		return "", nil
	}

	scored := ScoreFiles(files, issueText)
	if len(scored) > maxFiles {
		scored = scored[:maxFiles]
	}

	out := format(workspace, scored, budget)
	return out, nil
}
