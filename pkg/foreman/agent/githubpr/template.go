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

package githubpr

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// templateFiles is the conventional set of single-file PR template
// locations GitHub resolves for a repository, in resolution order. Each is
// probed in both .md and .txt; on case-insensitive filesystems this also
// matches the lower-cased forms, and the explicit casing keeps Linux
// (case-sensitive) matching the exact GitHub-canonical names.
var templateFiles = []string{
	filepath.Join(".github", "PULL_REQUEST_TEMPLATE.md"),
	filepath.Join(".github", "PULL_REQUEST_TEMPLATE.txt"),
	"PULL_REQUEST_TEMPLATE.md",
	"PULL_REQUEST_TEMPLATE.txt",
	filepath.Join("docs", "PULL_REQUEST_TEMPLATE.md"),
	filepath.Join("docs", "PULL_REQUEST_TEMPLATE.txt"),
}

// templateDir is the multi-template directory GitHub reads when a repo ships
// several per-area templates instead of one file.
var templateDir = filepath.Join(".github", "PULL_REQUEST_TEMPLATE")

// FindTemplate returns the raw content of the target repository's PR
// template, discovered in the coder's checked-out workspace, or "" when the
// repository has none. The lookup follows GitHub's own resolution order:
// the conventional single-file locations (case-insensitive where the
// filesystem allows, .txt accepted), then the .github/PULL_REQUEST_TEMPLATE/
// directory, where "default" wins and otherwise the first entry is chosen
// deterministically (os.ReadDir orders by file name).
//
// An empty or non-existent workspace yields "" so the caller falls back to
// Foreman's own body shape (the pre-#1541 behavior).
func FindTemplate(workspace string) string {
	if strings.TrimSpace(workspace) == "" {
		return ""
	}
	for _, rel := range templateFiles {
		if b, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(rel))); err == nil {
			if strings.TrimSpace(string(b)) != "" {
				return string(b)
			}
		}
	}
	if b, ok := readDefaultTemplate(workspace); ok {
		return b
	}
	return ""
}

// readDefaultTemplate reads the .github/PULL_REQUEST_TEMPLATE/ directory and
// returns (content, true) when a template file is present. "default" (any
// extension) is preferred; otherwise the alphabetically-first regular file
// wins so the choice is deterministic across runs.
func readDefaultTemplate(workspace string) (string, bool) {
	dir := filepath.Join(workspace, filepath.FromSlash(templateDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	first := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if first == "" {
			first = e.Name()
		}
		if strings.EqualFold(e.Name(), "default.md") || strings.EqualFold(e.Name(), "default.txt") {
			first = e.Name()
		}
	}
	if first == "" {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, first))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// PRBody composes the pull request body Foreman posts.
//
// When the target repository has a PR template (template non-blank), it is
// used as the body's scaffolding and the reviewer's authored prose follows it,
// rather than being discarded as it was before #1701.
//
// The prose is APPENDED after the template, not interpolated into its
// sections: filling "## What" would mean parsing the repo's headings, which
// is fragile and differs per repo. The checkboxes are deliberately left
// exactly as the repo authored them — Foreman cannot verify the claims they
// make, and a wrongly-ticked box would be a false one — so a human still
// ticks them when promoting the draft.
//
// The issue link and provenance are appended last so an agent PR is never
// mistaken for a hand-written one (#1541).
//
// When template is blank the body is Foreman's own fixed shape, byte-for-byte
// identical to the pre-#1541 output: the reviewer's (diff-grounded, #1411)
// summary, then the issue link, then the provenance line. This is the
// regression guard: repos without a template see no change.
func PRBody(template, summary string, issue int32, workload string) string {
	var bodyB strings.Builder
	if t := strings.TrimSpace(template); t != "" {
		bodyB.WriteString(strings.TrimRight(template, "\r\n"))
		bodyB.WriteString("\n\n")
		if s := strings.TrimSpace(summary); s != "" {
			bodyB.WriteString(s)
			bodyB.WriteString("\n\n")
		}
	} else if s := strings.TrimSpace(summary); s != "" {
		bodyB.WriteString(s)
		bodyB.WriteString("\n\n")
	}
	// Provenance always appended, template or not (#1541 step 4).
	fmt.Fprintf(&bodyB, "Fixes #%d\n\n_Opened by foreman on review GO (workload %s)._",
		issue, workload)
	return bodyB.String()
}

// DescriptionBody composes the pull request body when an agent authored the
// complete description (#1768) — the coder's prBody on the review-GO open
// path, or the reviewer's on the pre-#1568 shape.
//
// Unlike PRBody it does NOT prepend the repository template. The description
// is already a finished, repo-shaped document (the coder is prompted with the
// target repo's template and fills it in); prepending the template on top of
// it is what shipped PRs whose body was the raw unfilled template with every
// HTML comment intact and every checklist box empty (#1768).
//
// The issue link is appended only when the description does not already
// mention the issue. The match is digit-bounded, so a body citing #1768
// does not satisfy the reference for issue 176 — without the boundary the
// sibling reference would suppress the Fixes line and the issue would
// never auto-close. An agent that wrote `Refs #<N>` deliberately chose not
// to close the issue on merge — a multi-slice issue must survive its first
// slice — and an unconditional `Fixes #<N>` would silently close it.
//
// The provenance line is always appended, so an agent PR is never mistaken
// for a hand-written one.
func DescriptionBody(desc string, issue int32, workload string) string {
	var bodyB strings.Builder
	if d := strings.TrimSpace(desc); d != "" {
		bodyB.WriteString(d)
		bodyB.WriteString("\n\n")
	}
	issueRef := fmt.Sprintf("#%d", issue)
	ref := regexp.MustCompile(fmt.Sprintf(`#%d(\D|$)`, issue))
	if !ref.MatchString(desc) {
		fmt.Fprintf(&bodyB, "Fixes %s\n\n", issueRef)
	}
	fmt.Fprintf(&bodyB, "_Opened by foreman on review GO (workload %s)._", workload)
	return bodyB.String()
}
