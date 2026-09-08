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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPRBody_NoTemplateIsByteIdenticalToLegacy is the regression guard
// (#1541): a repo with no PR template must produce exactly the body
// Foreman always emitted — the reviewer's summary, the issue link, and the
// provenance line — byte for byte. The template code path must not perturb
// this shape at all.
func TestPRBody_NoTemplateIsByteIdenticalToLegacy(t *testing.T) {
	summary := "Adds provider details to the SSO error path so failures are diagnosable."
	issue := int32(7)
	wl := "wl-x"

	got := PRBody("", summary, issue, wl)
	want := summary + "\n\n" +
		"Fixes #7\n\n" +
		"_Opened by foreman on review GO (workload wl-x)._"
	if got != want {
		t.Fatalf("no-template body changed:\n got %q\nwant %q", got, want)
	}
}

// TestPRBody_NoTemplateEmptySummary: an empty summary falls back to just the
// issue link + provenance, exactly as the pre-#1541 code did.
func TestPRBody_NoTemplateEmptySummary(t *testing.T) {
	got := PRBody("", "   ", 12, "wl-y")
	want := "Fixes #12\n\n" +
		"_Opened by foreman on review GO (workload wl-y)._"
	if got != want {
		t.Fatalf("empty-summary no-template body changed:\n got %q\nwant %q", got, want)
	}
}

// TestPRBody_UsesTemplate pins the merge behavior (#1701): when the target
// repo ships a PR template it is used as the body's scaffolding (checkboxes
// left as authored — a wrongly-ticked box would be a false claim), the
// reviewer's authored summary is spliced in rather than discarded, and the
// provenance is always appended so an agent PR is never mistaken for a
// hand-written one (#1541).
func TestPRBody_UsesTemplate(t *testing.T) {
	summary := "Reviewer summary that must survive into the posted PR."
	tmpl := "## What\n\nChanged X.\n\n## Checklist\n\n- [ ] tests\n- [ ] docs"
	got := PRBody(tmpl, summary, 42, "wl-z")

	// The reviewer's prose must not be thrown away when a template exists.
	if !strings.Contains(got, summary) {
		t.Errorf("reviewer summary must survive into a PR that has a template; got %q", got)
	}
	// The template body must appear verbatim (checkboxes untouched).
	if !strings.Contains(got, "- [ ] tests\n- [ ] docs") {
		t.Errorf("template content (incl. checkboxes) must appear verbatim; got %q", got)
	}
	// Provenance always appended.
	if !strings.Contains(got, "Fixes #42") ||
		!strings.Contains(got, "_Opened by foreman on review GO (workload wl-z)._") {
		t.Errorf("provenance must always be appended; got %q", got)
	}
	// Template leads, the reviewer's prose is spliced between template and
	// provenance, which trails.
	want := tmpl + "\n\n" +
		summary + "\n\n" +
		"Fixes #42\n\n" +
		"_Opened by foreman on review GO (workload wl-z)._"
	if got != want {
		t.Errorf("template body mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestPRBody_TemplateTrailingNewlinesTrimmed: a template file that ends with
// trailing newlines is normalized so exactly one blank line separates the
// template from the provenance (no runaway blank lines).
func TestPRBody_TemplateTrailingNewlinesTrimmed(t *testing.T) {
	got := PRBody("## What\n\nChanged X.\n\n\n\n", "sum", 3, "w")
	if strings.Count(got, "\n\n\n") > 1 {
		t.Errorf("template trailing newlines not normalized; got %q", got)
	}
	if !strings.HasSuffix(got, "(workload w)._") {
		t.Errorf("provenance must trail; got %q", got)
	}
}

// writeTree lays out files under dir for FindTemplate cases.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPRBody_EmptySummaryWithTemplate: with a template and no reviewer prose
// the body is just the template + provenance, exactly as the pre-#1701 code
// emitted (an empty summary must not inject a blank line before provenance).
func TestPRBody_EmptySummaryWithTemplate(t *testing.T) {
	tmpl := "## What\n\nChanged X.\n\n## Checklist\n\n- [ ] tests"
	got := PRBody(tmpl, "", 5, "w")
	want := tmpl + "\n\n" +
		"Fixes #5\n\n" +
		"_Opened by foreman on review GO (workload w)._"
	if got != want {
		t.Errorf("template+empty-summary body mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestFindTemplate_ResolutionOrder pins GitHub's own discovery order: the
// .github/ single-file template beats the repo-root one, which beats docs/.
func TestFindTemplate_ResolutionOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "github dir preferred over root and docs",
			files: map[string]string{
				".github/PULL_REQUEST_TEMPLATE.md": "GITHUB-DIR",
				"PULL_REQUEST_TEMPLATE.md":         "ROOT",
				"docs/PULL_REQUEST_TEMPLATE.md":    "DOCS",
			},
			want: "GITHUB-DIR",
		},
		{
			name: "root preferred over docs when github absent",
			files: map[string]string{
				"PULL_REQUEST_TEMPLATE.md":      "ROOT",
				"docs/PULL_REQUEST_TEMPLATE.md": "DOCS",
			},
			want: "ROOT",
		},
		{
			name: "docs alone",
			files: map[string]string{
				"docs/PULL_REQUEST_TEMPLATE.md": "DOCS",
			},
			want: "DOCS",
		},
		{
			name: "txt accepted",
			files: map[string]string{
				".github/PULL_REQUEST_TEMPLATE.txt": "TXT",
			},
			want: "TXT",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tc.files)
			if got := FindTemplate(dir); got != tc.want {
				t.Fatalf("FindTemplate = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFindTemplate_NoTemplate: an empty tree and an empty workspace both
// yield "" so the caller falls back to Foreman's own body.
func TestFindTemplate_NoTemplate(t *testing.T) {
	if got := FindTemplate(t.TempDir()); got != "" {
		t.Fatalf("empty tree must yield \"\", got %q", got)
	}
	if got := FindTemplate(""); got != "" {
		t.Fatalf("empty workspace must yield \"\", got %q", got)
	}
	if got := FindTemplate("/nonexistent/path"); got != "" {
		t.Fatalf("missing workspace must yield \"\", got %q", got)
	}
}

// TestFindTemplate_MultiTemplateDir: the .github/PULL_REQUEST_TEMPLATE/
// directory is honored, preferring "default" and otherwise the first entry
// (ReadDir orders by name, so the choice is deterministic).
func TestFindTemplate_MultiTemplateDir(t *testing.T) {
	t.Run("default preferred", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			filepath.Join(".github", "PULL_REQUEST_TEMPLATE", "bug.md"):     "BUG",
			filepath.Join(".github", "PULL_REQUEST_TEMPLATE", "default.md"): "DEFAULT",
			filepath.Join(".github", "PULL_REQUEST_TEMPLATE", "feature.md"): "FEATURE",
		})
		if got := FindTemplate(dir); got != "DEFAULT" {
			t.Fatalf("default must win; got %q", got)
		}
	})
	t.Run("first by name when no default", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			filepath.Join(".github", "PULL_REQUEST_TEMPLATE", "bug.md"):     "BUG",
			filepath.Join(".github", "PULL_REQUEST_TEMPLATE", "feature.md"): "FEATURE",
		})
		if got := FindTemplate(dir); got != "BUG" {
			t.Fatalf("alphabetically-first must win; got %q", got)
		}
	})
	t.Run("no dir no files", func(t *testing.T) {
		if got := FindTemplate(t.TempDir()); got != "" {
			t.Fatalf("no template anywhere must yield \"\", got %q", got)
		}
	})
}

// TestDescriptionBody is the #1768 renderer contract: a description an agent
// authored is posted as-is, without the repository template prepended, with
// the issue link added only when the author did not write one, and always
// with the provenance line.
func TestDescriptionBody(t *testing.T) {
	const provenance = "_Opened by foreman on review GO (workload wl-x)._"
	cases := []struct {
		name    string
		desc    string
		issue   int32
		wantIn  []string
		wantOut []string
	}{
		{
			// A multi-slice issue must survive its first slice: the
			// coder wrote Refs deliberately, so no Fixes may be added.
			name:    "desc already referencing the issue gets no Fixes line",
			desc:    "## What\n\nResumes the local slice.\n\nRefs #7",
			wantIn:  []string{"## What", "Refs #7"},
			wantOut: []string{"Fixes #7"},
		},
		{
			name:   "desc without the issue reference gets Fixes appended",
			desc:   "## What\n\nFixes the SSO error path.",
			wantIn: []string{"## What", "Fixes #7"},
		},
		{
			name:    "surrounding whitespace is trimmed",
			desc:    "\n\n  Trimmed description.  \n\n",
			wantIn:  []string{"Trimmed description.", "Fixes #7"},
			wantOut: []string{"\n\n\n"},
		},
		{
			name:   "empty description still links the issue",
			desc:   "",
			wantIn: []string{"Fixes #7"},
		},
		{
			// The reference match is digit-bounded: a body citing a
			// sibling issue must not suppress this issue's Fixes line,
			// or this PR would never auto-close its own issue (#1768
			// revision).
			name:    "a longer sibling reference does not suppress the Fixes line",
			desc:    "## What\n\nFollows up on #1768.",
			issue:   176,
			wantIn:  []string{"Fixes #176"},
			wantOut: nil,
		},
		{
			name:    "a shorter prefix-like reference does not suppress the Fixes line",
			desc:    "## What\n\nSee #17680 for context.",
			issue:   1768,
			wantIn:  []string{"Fixes #1768"},
			wantOut: nil,
		},
		{
			name:    "an exact reference still suppresses the Fixes line",
			desc:    "## What\n\nRefs #1768.",
			issue:   1768,
			wantIn:  []string{"Refs #1768."},
			wantOut: []string{"Fixes #1768"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := tc.issue
			if issue == 0 {
				issue = 7
			}
			got := DescriptionBody(tc.desc, issue, "wl-x")
			for _, s := range tc.wantIn {
				if !strings.Contains(got, s) {
					t.Errorf("body must contain %q; got %q", s, got)
				}
			}
			for _, s := range tc.wantOut {
				if strings.Contains(got, s) {
					t.Errorf("body must NOT contain %q; got %q", s, got)
				}
			}
			if !strings.HasSuffix(got, provenance) {
				t.Errorf("body must end with the provenance line %q; got %q", provenance, got)
			}
		})
	}
}

// TestDescriptionBody_NeverCarriesTemplateText pins the difference from
// PRBody that #1768 exists to create: the rendered description stands alone,
// so the target repo's template scaffolding never reaches the PR.
func TestDescriptionBody_NeverCarriesTemplateText(t *testing.T) {
	const template = "## What this PR does\n<!-- Describe your change -->\n- [ ] AI assistance disclosed"
	got := DescriptionBody("## What\n\nResumes a truncated download.\n", 7, "wl-x")
	if strings.Contains(got, "Describe your change") ||
		strings.Contains(got, "AI assistance disclosed") {
		t.Errorf("DescriptionBody must not prepend template text; got %q", got)
	}
	withTemplate := PRBody(template, "Resumes a truncated download.", 7, "wl-x")
	if !strings.Contains(withTemplate, "Describe your change") {
		t.Errorf("PRBody must keep honouring the template; got %q", withTemplate)
	}
}
