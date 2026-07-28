# Test-dilution advisory (#1332) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `tierAdvisory` coder-gate check that surfaces to the reviewer when a submission weakens its own tests (removes assertions or relocates/deletes fixtures covering the changed code), so a green gate cannot be earned by diluting checks.

**Architecture:** One new file, `pkg/foreman/agent/test_dilution_gate.go`, holding small pure functions (diff parsers + two erosion detectors) and one orchestrator `checkTestDilution` matching the `gateCheck.fn` signature. It is registered in `gateCheckRegistry` (in `coder_gate.go`) as a `tierAdvisory` entry, so its finding flows through the existing `attachGateAdvisories` -> `Extra["gateAdvisories"]` -> `renderGateAdvisories` path into the reviewer prompt. It reuses the established `git add -A` + `git diff --cached HEAD` working-tree seam and never fails the gate.

**Tech Stack:** Go 1.25, standard library only (`context`, `strings`, `regexp`, `sort`, `path/filepath`, `fmt`). Tests use the package's existing fake-`commandRunner` pattern (no shelling out). Design doc: `docs/superpowers/specs/2026-07-28-issue-1332-test-dilution-advisory-design.md`.

## Global Constraints

- Package `agent`; all new code in `pkg/foreman/agent/test_dilution_gate.go`, all new tests in `pkg/foreman/agent/test_dilution_gate_test.go`.
- The check MUST be **advisory only** (`tier: tierAdvisory`): it never appends to `failures`, never fails the gate, never enters the coder feedback string.
- **Fail-open**: any git error, or no production change, returns `(false, "")` (silent). Never panic, never block.
- Advisory `Check` name is the exact string `"test-dilution"`; this yields the free kill-switch `FOREMAN_TEST_DILUTION_GATE=0` via the existing `gateCheckEnabled`.
- The `gateCheck.fn` signature is exactly `func(ctx context.Context, workspace string, run commandRunner) (failed bool, output string)` (defined in `pkg/foreman/agent/gate_registry.go`). `commandRunner` is defined in `pkg/foreman/agent/coder_gate.go`.
- Reuse the existing `truncateOutput(output string) string` helper (in `coder_gate.go`, caps at `maxCheckOutputBytes`, keeps the tail with a `...(truncated)...` prefix) to bound the advisory detail. Do not redefine it. Note: `truncateUTF8` lives only in the `mcp` subpackage and is NOT callable from package `agent`.
- Detection is **package-linked**: a test-side erosion is only reported when the same submission changed **non-test** production code in that test's owning package.
- Net-erosion rule for assertions: fire only when `removedAssertions > addedAssertions` in a file.
- No em dashes in comments or the advisory text. `gofmt` clean and `GOOS=linux ./bin/golangci-lint run ./...` clean (errcheck: discard ignored returns with `_, _ =`). DCO `git commit -s` on every commit.
- Do NOT wire this into the Go-toolchain tier or `RunCoderGate`'s inline checks; it goes only into `gateCheckRegistry`.

---

## File Structure

- **Create** `pkg/foreman/agent/test_dilution_gate.go` — all detection logic:
  - types `fileHunks`, `nameStatusEntry`
  - pure parsers `parseUnifiedDiff`, `parseNameStatus`
  - pure helpers `changedProdPackages`, `testdataOwner`, `firstN`
  - pure detectors `isAssertionLine`, `assertionErosion`, `fixtureTokens`, `fixtureLiteralChurn`, `fixtureFileChanges`
  - orchestrator `checkTestDilution`
- **Create** `pkg/foreman/agent/test_dilution_gate_test.go` — unit tests for each of the above.
- **Modify** `pkg/foreman/agent/coder_gate.go` — add one entry to the slice returned by `gateCheckRegistry` (function begins at the `func gateCheckRegistry(` line; append the new `gateCheck` literal to the returned slice).

---

## Interfaces (shared types and signatures used across tasks)

Defined in Task 1 unless noted; later tasks consume them verbatim.

```go
// fileHunks holds the content (prefix stripped) of one file's added and
// removed diff lines from a --unified=0 diff.
type fileHunks struct {
	Added   []string
	Removed []string
}

// nameStatusEntry is one file-level change from `git diff --name-status`.
// For renames/copies OldPath is the source and Path the destination; for
// M/A/D only Path is set.
type nameStatusEntry struct {
	Code    string // "M", "A", "D", "R100", "C75", ...
	Path    string
	OldPath string
}

func parseUnifiedDiff(out string) map[string]*fileHunks          // Task 1
func parseNameStatus(out string) []nameStatusEntry               // Task 4
func changedProdPackages(entries []nameStatusEntry) map[string]bool // Task 4
func testdataOwner(path string) (owner string, ok bool)          // Task 4
func isAssertionLine(s string) bool                              // Task 2
func assertionErosion(fh *fileHunks) (removed, added int, snippets []string) // Task 2
func fixtureTokens(lines []string) map[string]bool               // Task 3
func fixtureLiteralChurn(fh *fileHunks) []string                 // Task 3
func fixtureFileChanges(entries []nameStatusEntry, prodPkgs map[string]bool) []string // Task 4
func firstN(s []string, n int) []string                          // Task 2
func checkTestDilution(ctx context.Context, workspace string, run commandRunner) (bool, string) // Task 5
```

---

### Task 1: Unified-diff parser (`fileHunks`, `parseUnifiedDiff`)

Parses `git diff --unified=0 --src-prefix=a/ --dst-prefix=b/` output into per-file added/removed content lines. This is the workhorse for the line-level signals.

**Files:**
- Create: `pkg/foreman/agent/test_dilution_gate.go`
- Test: `pkg/foreman/agent/test_dilution_gate_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: type `fileHunks`, `func parseUnifiedDiff(out string) map[string]*fileHunks`.

- [ ] **Step 1: Write the failing test**

Create `pkg/foreman/agent/test_dilution_gate_test.go`:

```go
package agent

import (
	"reflect"
	"testing"
)

func TestParseUnifiedDiff_AttributesAddedAndRemoved(t *testing.T) {
	// A modified test file: one assertion removed, one added.
	out := `diff --git a/pkg/model/x_test.go b/pkg/model/x_test.go
index 1111111..2222222 100644
--- a/pkg/model/x_test.go
+++ b/pkg/model/x_test.go
@@ -10 +10 @@ func TestFoo(t *testing.T) {
-	Expect(got).To(Equal(oldWant))
+	Expect(got).To(Equal(newWant))
`
	got := parseUnifiedDiff(out)
	fh := got["pkg/model/x_test.go"]
	if fh == nil {
		t.Fatalf("no hunks for pkg/model/x_test.go; got keys %v", keys(got))
	}
	if !reflect.DeepEqual(fh.Removed, []string{"\tExpect(got).To(Equal(oldWant))"}) {
		t.Errorf("Removed = %q", fh.Removed)
	}
	if !reflect.DeepEqual(fh.Added, []string{"\tExpect(got).To(Equal(newWant))"}) {
		t.Errorf("Added = %q", fh.Added)
	}
}

func TestParseUnifiedDiff_DeletedFileAttributedToOldPath(t *testing.T) {
	out := `diff --git a/pkg/model/y_test.go b/pkg/model/y_test.go
deleted file mode 100644
index 3333333..0000000
--- a/pkg/model/y_test.go
+++ /dev/null
@@ -1 +0,0 @@
-	require.NoError(t, err)
`
	got := parseUnifiedDiff(out)
	fh := got["pkg/model/y_test.go"]
	if fh == nil || len(fh.Removed) != 1 {
		t.Fatalf("deleted file removed lines not attributed to old path; got %v", keys(got))
	}
}

// keys is a tiny test helper for readable failure messages.
func keys(m map[string]*fileHunks) []string {
	var k []string
	for f := range m {
		k = append(k, f)
	}
	return k
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run TestParseUnifiedDiff -count=1`
Expected: FAIL to compile (`undefined: parseUnifiedDiff`, `undefined: fileHunks`).

- [ ] **Step 3: Write minimal implementation**

Create `pkg/foreman/agent/test_dilution_gate.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run TestParseUnifiedDiff -count=1`
Expected: PASS (`ok  github.com/defilantech/llmkube/pkg/foreman/agent`).

- [ ] **Step 5: Commit**

```bash
git add pkg/foreman/agent/test_dilution_gate.go pkg/foreman/agent/test_dilution_gate_test.go
git commit -s -m "feat(foreman): unified-diff parser for test-dilution detection (#1332)"
```

---

### Task 2: Assertion-erosion detector (`isAssertionLine`, `assertionErosion`, `firstN`)

Classifies assertion-shaped lines and computes net erosion per file.

**Files:**
- Modify: `pkg/foreman/agent/test_dilution_gate.go`
- Test: `pkg/foreman/agent/test_dilution_gate_test.go`

**Interfaces:**
- Consumes: `fileHunks` (Task 1).
- Produces: `func isAssertionLine(s string) bool`, `func assertionErosion(fh *fileHunks) (removed, added int, snippets []string)`, `func firstN(s []string, n int) []string`.

- [ ] **Step 1: Write the failing test**

Append to `pkg/foreman/agent/test_dilution_gate_test.go`:

```go
func TestAssertionErosion_NetRemovalCountedWithSnippets(t *testing.T) {
	fh := &fileHunks{
		Removed: []string{
			"\tExpect(got).To(Equal(want))",
			"\trequire.NoError(t, err)",
			"\t// just a comment, not an assertion",
		},
		Added: []string{
			"\tassert.Equal(t, want, got)",
		},
	}
	removed, added, snippets := assertionErosion(fh)
	if removed != 2 || added != 1 {
		t.Fatalf("removed=%d added=%d, want 2 and 1", removed, added)
	}
	if len(snippets) != 2 || snippets[0] != "Expect(got).To(Equal(want))" {
		t.Errorf("snippets = %q", snippets)
	}
}

func TestAssertionErosion_NonAssertionsIgnored(t *testing.T) {
	fh := &fileHunks{Removed: []string{"\tx := 1", "\treturn nil"}}
	removed, _, _ := assertionErosion(fh)
	if removed != 0 {
		t.Fatalf("removed=%d, want 0 (no assertion-shaped lines)", removed)
	}
}

func TestFirstN(t *testing.T) {
	if got := firstN([]string{"a", "b", "c"}, 2); len(got) != 2 {
		t.Fatalf("firstN cap failed: %v", got)
	}
	if got := firstN([]string{"a"}, 3); len(got) != 1 {
		t.Fatalf("firstN under-length failed: %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run 'TestAssertionErosion|TestFirstN' -count=1`
Expected: FAIL to compile (`undefined: assertionErosion`, `undefined: firstN`).

- [ ] **Step 3: Write minimal implementation**

Append to `pkg/foreman/agent/test_dilution_gate.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run 'TestAssertionErosion|TestFirstN' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/foreman/agent/test_dilution_gate.go pkg/foreman/agent/test_dilution_gate_test.go
git commit -s -m "feat(foreman): assertion-erosion detector for test-dilution (#1332)"
```

---

### Task 3: Fixture-literal churn detector (`fixtureTokens`, `fixtureLiteralChurn`)

Detects a fixture input (URL host or `testdata/` path literal) that disappeared from the removed lines while a different one appeared in the added lines: the #1322 "moved fixtures off huggingface.co" signature.

**Files:**
- Modify: `pkg/foreman/agent/test_dilution_gate.go`
- Test: `pkg/foreman/agent/test_dilution_gate_test.go`

**Interfaces:**
- Consumes: `fileHunks` (Task 1).
- Produces: `func fixtureTokens(lines []string) map[string]bool`, `func fixtureLiteralChurn(fh *fileHunks) []string`.

- [ ] **Step 1: Write the failing test**

Append to `pkg/foreman/agent/test_dilution_gate_test.go`:

```go
func TestFixtureLiteralChurn_HostRelocation(t *testing.T) {
	// The #1322 shape: a fixture URL host moved off huggingface.co.
	fh := &fileHunks{
		Removed: []string{`	src := "https://huggingface.co/org/model/resolve/main/f.gguf"`},
		Added:   []string{`	src := "https://example.com/org/model/resolve/main/f.gguf"`},
	}
	got := fixtureLiteralChurn(fh)
	if len(got) != 1 || !strings.Contains(got[0], "huggingface.co") || !strings.Contains(got[0], "example.com") {
		t.Fatalf("expected a host-churn finding naming both hosts; got %q", got)
	}
}

func TestFixtureLiteralChurn_PureAdditionSilent(t *testing.T) {
	// Adding a new fixture (no matching removal) is not relocation.
	fh := &fileHunks{
		Added: []string{`	src := "https://huggingface.co/org/model/f.gguf"`},
	}
	if got := fixtureLiteralChurn(fh); got != nil {
		t.Fatalf("pure addition must not flag churn; got %q", got)
	}
}

func TestFixtureLiteralChurn_TestdataPathRelocation(t *testing.T) {
	fh := &fileHunks{
		Removed: []string{`	data := load("pkg/model/testdata/real_repo.json")`},
		Added:   []string{`	data := load("pkg/model/testdata/renamed_repo.json")`},
	}
	got := fixtureLiteralChurn(fh)
	if len(got) != 1 || !strings.Contains(got[0], "testdata/") {
		t.Fatalf("expected a testdata path-churn finding; got %q", got)
	}
}
```

Add `"strings"` to the test file's imports if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run TestFixtureLiteralChurn -count=1`
Expected: FAIL to compile (`undefined: fixtureLiteralChurn`).

- [ ] **Step 3: Write minimal implementation**

Add `"fmt"`, `"regexp"`, and `"sort"` to the imports in `pkg/foreman/agent/test_dilution_gate.go` (change the single `import "strings"` to a grouped import block), then append:

```go
var (
	// urlHostRe captures the host of an http(s) URL string literal.
	urlHostRe = regexp.MustCompile(`https?://([^/\s"'` + "`" + `]+)`)
	// testdataPathRe captures a testdata/ file path literal.
	testdataPathRe = regexp.MustCompile(`[\w./-]*testdata/[\w./-]+`)
)

// fixtureTokens extracts fixture-input identifiers from a set of diff lines:
// URL hosts (prefixed "host:") and testdata/ paths (prefixed "path:"). The
// prefix keeps the two kinds from colliding in the set-difference below.
func fixtureTokens(lines []string) map[string]bool {
	set := map[string]bool{}
	for _, l := range lines {
		for _, m := range urlHostRe.FindAllStringSubmatch(l, -1) {
			set["host:"+m[1]] = true
		}
		for _, m := range testdataPathRe.FindAllString(l, -1) {
			set["path:"+m] = true
		}
	}
	return set
}

// fixtureLiteralChurn flags a fixture input that was changed in place: a host
// or testdata path present only on the removed side while a different one
// appears only on the added side. This is relocation (the #1322 signature),
// distinct from a pure fixture addition (added only) or deletion (removed
// only), both of which return nil. Deterministic: tokens are sorted.
func fixtureLiteralChurn(fh *fileHunks) []string {
	rem := fixtureTokens(fh.Removed)
	add := fixtureTokens(fh.Added)
	var gone, appeared []string
	for tkn := range rem {
		if !add[tkn] {
			gone = append(gone, tkn)
		}
	}
	for tkn := range add {
		if !rem[tkn] {
			appeared = append(appeared, tkn)
		}
	}
	if len(gone) == 0 || len(appeared) == 0 {
		return nil
	}
	sort.Strings(gone)
	sort.Strings(appeared)
	return []string{fmt.Sprintf("fixture input changed (removed %v, added %v)", gone, appeared)}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run TestFixtureLiteralChurn -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/foreman/agent/test_dilution_gate.go pkg/foreman/agent/test_dilution_gate_test.go
git commit -s -m "feat(foreman): fixture-literal churn detector for test-dilution (#1332)"
```

---

### Task 4: name-status parser and package linkage (`parseNameStatus`, `changedProdPackages`, `testdataOwner`, `fixtureFileChanges`)

Provides file-level change facts: which packages changed production code, and which fixtures were deleted or renamed under those packages.

**Files:**
- Modify: `pkg/foreman/agent/test_dilution_gate.go`
- Test: `pkg/foreman/agent/test_dilution_gate_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: type `nameStatusEntry`; `func parseNameStatus(out string) []nameStatusEntry`; `func changedProdPackages(entries []nameStatusEntry) map[string]bool`; `func testdataOwner(path string) (string, bool)`; `func fixtureFileChanges(entries []nameStatusEntry, prodPkgs map[string]bool) []string`.

- [ ] **Step 1: Write the failing test**

Append to `pkg/foreman/agent/test_dilution_gate_test.go`:

```go
func TestParseNameStatus_ModifyAndRename(t *testing.T) {
	out := "M\tpkg/model/classifier.go\n" +
		"D\tpkg/model/testdata/real.json\n" +
		"R100\tpkg/model/testdata/a.json\tpkg/model/testdata/b.json\n"
	got := parseNameStatus(out)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	if got[2].Code[0] != 'R' || got[2].OldPath != "pkg/model/testdata/a.json" || got[2].Path != "pkg/model/testdata/b.json" {
		t.Errorf("rename parsed wrong: %+v", got[2])
	}
}

func TestChangedProdPackages_IgnoresTestAndNonGo(t *testing.T) {
	entries := []nameStatusEntry{
		{Code: "M", Path: "pkg/model/classifier.go"},
		{Code: "M", Path: "pkg/model/classifier_test.go"},
		{Code: "M", Path: "pkg/other/x_test.go"}, // test-only pkg: not prod-changed
		{Code: "M", Path: "docs/readme.md"},
	}
	got := changedProdPackages(entries)
	if !got["pkg/model"] {
		t.Errorf("pkg/model should be a changed-prod package")
	}
	if got["pkg/other"] {
		t.Errorf("pkg/other changed only a test file; must not count as prod-changed")
	}
	if len(got) != 1 {
		t.Errorf("got %v, want only pkg/model", got)
	}
}

func TestTestdataOwner(t *testing.T) {
	if o, ok := testdataOwner("pkg/model/testdata/x.json"); !ok || o != "pkg/model" {
		t.Errorf("owner = %q, %v; want pkg/model, true", o, ok)
	}
	if _, ok := testdataOwner("pkg/model/classifier.go"); ok {
		t.Errorf("non-testdata path must not resolve an owner")
	}
}

func TestFixtureFileChanges_DeleteAndRenameUnderChangedPkg(t *testing.T) {
	entries := []nameStatusEntry{
		{Code: "D", Path: "pkg/model/testdata/real.json"},
		{Code: "R100", OldPath: "pkg/model/testdata/a.json", Path: "pkg/model/testdata/b.json"},
		{Code: "D", Path: "pkg/other/testdata/z.json"}, // owner not prod-changed: ignored
	}
	prod := map[string]bool{"pkg/model": true}
	got := fixtureFileChanges(entries, prod)
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %v", len(got), got)
	}
	joined := strings.Join(got, " | ")
	if !strings.Contains(joined, "deleted fixture pkg/model/testdata/real.json") ||
		!strings.Contains(joined, "relocated fixture pkg/model/testdata/a.json -> pkg/model/testdata/b.json") {
		t.Errorf("findings = %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run 'TestParseNameStatus|TestChangedProdPackages|TestTestdataOwner|TestFixtureFileChanges' -count=1`
Expected: FAIL to compile (`undefined: parseNameStatus`, etc.).

- [ ] **Step 3: Write minimal implementation**

Add `"path/filepath"` to the imports in `pkg/foreman/agent/test_dilution_gate.go`, then append:

```go
// nameStatusEntry is one file-level change from `git diff --name-status`.
type nameStatusEntry struct {
	Code    string // "M", "A", "D", "R100", "C75", ...
	Path    string // destination path (rename) or the changed path
	OldPath string // source path, only set for renames/copies
}

// parseNameStatus parses tab-separated `git diff --name-status` output. Rename
// and copy rows carry two paths (old, new); all others carry one.
func parseNameStatus(out string) []nameStatusEntry {
	var entries []nameStatusEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		code := f[0]
		if (strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C")) && len(f) >= 3 {
			entries = append(entries, nameStatusEntry{Code: code, OldPath: f[1], Path: f[2]})
			continue
		}
		entries = append(entries, nameStatusEntry{Code: code, Path: f[len(f)-1]})
	}
	return entries
}

// changedProdPackages returns the set of package directories whose non-test Go
// source changed. A package that changed only its tests is not included: the
// linkage requires a production change to gate the test-dilution signal.
func changedProdPackages(entries []nameStatusEntry) map[string]bool {
	pkgs := map[string]bool{}
	for _, e := range entries {
		p := e.Path
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		pkgs[filepath.Dir(p)] = true
	}
	return pkgs
}

// testdataOwner returns the package directory that owns a testdata path (the
// segment before "/testdata/"), or ("", false) when the path is not under a
// testdata directory. A top-level "testdata/..." is owned by ".".
func testdataOwner(path string) (string, bool) {
	if i := strings.Index(path, "/testdata/"); i >= 0 {
		return path[:i], true
	}
	if strings.HasPrefix(path, "testdata/") {
		return ".", true
	}
	return "", false
}

// fixtureFileChanges reports testdata fixtures that were deleted or renamed
// under a package whose production code changed. Deleting or moving a fixture
// is how coverage of a path can be dropped without touching an assertion.
func fixtureFileChanges(entries []nameStatusEntry, prodPkgs map[string]bool) []string {
	var out []string
	for _, e := range entries {
		owner, ok := testdataOwner(e.Path)
		if !ok {
			if e.OldPath == "" {
				continue
			}
			owner, ok = testdataOwner(e.OldPath)
		}
		if !ok || !prodPkgs[owner] {
			continue
		}
		switch {
		case strings.HasPrefix(e.Code, "D"):
			out = append(out, "deleted fixture "+e.Path)
		case strings.HasPrefix(e.Code, "R"):
			out = append(out, "relocated fixture "+e.OldPath+" -> "+e.Path)
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run 'TestParseNameStatus|TestChangedProdPackages|TestTestdataOwner|TestFixtureFileChanges' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/foreman/agent/test_dilution_gate.go pkg/foreman/agent/test_dilution_gate_test.go
git commit -s -m "feat(foreman): name-status parser and package linkage for test-dilution (#1332)"
```

---

### Task 5: Orchestrator `checkTestDilution`

Wires the git calls and combines both signals under package linkage, with fail-open behavior and a bounded single-line advisory. Includes the #1322-shaped bite-check.

**Files:**
- Modify: `pkg/foreman/agent/test_dilution_gate.go`
- Test: `pkg/foreman/agent/test_dilution_gate_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-4, plus `commandRunner` and `truncateOutput` (defined in `coder_gate.go`).
- Produces: `func checkTestDilution(ctx context.Context, workspace string, run commandRunner) (bool, string)`.

- [ ] **Step 1: Write the failing test**

Append to `pkg/foreman/agent/test_dilution_gate_test.go` (add `"context"` to the test imports):

```go
// dilutionRunner fakes the three git calls checkTestDilution makes:
// `git add -A` (no-op), `git diff --name-status --cached HEAD`, and the
// `git diff --cached --unified=0 ... -- *_test.go` line diff.
func dilutionRunner(nameStatus, testDiff string, addErr, nsErr, diffErr error) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil
		}
		switch {
		case len(args) >= 2 && args[0] == "add" && args[1] == "-A":
			return "", addErr
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--name-status":
			return nameStatus, nsErr
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--cached":
			return testDiff, diffErr
		default:
			return "", nil
		}
	}
}

func TestCheckTestDilution_FiresOnNetRemovedAssertions(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -10 +10 @@
-	Expect(classify(u)).To(Equal(RepoSource))
-	require.NoError(t, err)
+	// removed the assertions above
`
	failed, out := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil))
	if !failed {
		t.Fatal("expected an advisory when a changed-prod package net-removes assertions")
	}
	if !strings.Contains(out, "classifier_test.go") || !strings.Contains(out, "assertion") {
		t.Errorf("detail = %q", out)
	}
}

func TestCheckTestDilution_FiresOnFixtureRelocation_1322Shape(t *testing.T) {
	// #1322 bite-check: prod classifier changed, and a fixture URL host moved
	// off huggingface.co in the same package's test.
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +20 @@
-	src := "https://huggingface.co/org/model/resolve/main/f.gguf"
+	src := "https://example.com/org/model/resolve/main/f.gguf"
`
	failed, out := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil))
	if !failed {
		t.Fatal("expected an advisory for the #1322 fixture-relocation shape")
	}
	if !strings.Contains(out, "huggingface.co") {
		t.Errorf("detail should name the moved host; got %q", out)
	}
}

func TestCheckTestDilution_SilentWhenTestsOnlyGrow(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -10 +11 @@
+	Expect(classify(u)).To(Equal(RepoSource))
`
	if failed, _ := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil)); failed {
		t.Fatal("adding assertions must not fire the dilution advisory")
	}
}

func TestCheckTestDilution_SilentWhenNoProdChange(t *testing.T) {
	// Test-only submission: assertions removed but no production code changed.
	ns := "M\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -10 +9 @@
-	Expect(classify(u)).To(Equal(RepoSource))
`
	if failed, _ := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil)); failed {
		t.Fatal("package-linkage: no production change means no advisory")
	}
}

func TestCheckTestDilution_FailOpenOnGitError(t *testing.T) {
	if failed, out := checkTestDilution(context.Background(), "/w",
		dilutionRunner("", "", nil, errors.New("boom"), nil)); failed || out != "" {
		t.Fatalf("git error must fail open (silent); got failed=%v out=%q", failed, out)
	}
}
```

This task adds `"context"` and `"errors"` to the test file's import block
(final imports: `"context"`, `"errors"`, `"reflect"`, `"strings"`, `"testing"`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run TestCheckTestDilution -count=1`
Expected: FAIL to compile (`undefined: checkTestDilution`).

- [ ] **Step 3: Write minimal implementation**

Append to `pkg/foreman/agent/test_dilution_gate.go` (add `"context"` to the imports):

```go
// checkTestDilution is a tierAdvisory gate check (#1332). It surfaces to the
// reviewer when a submission that changes production code also weakens the
// tests covering that code: net-removed assertions, a relocated fixture input,
// or a deleted/renamed testdata fixture, all scoped to a package whose
// production code changed. It never fails the gate and never feeds the coder.
//
// Fail-open: any git error, or no production change in the submission, returns
// (false, "") so a bad diff signal or a docs-only change stays silent.
func checkTestDilution(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	// Stage the working tree so a pre-commit diff includes new/untracked files.
	// Idempotent with the executor's later `git add -A`; the -A exit status is
	// not actionable here, so a stage error simply fails the check open below.
	if _, err := run(ctx, workspace, nil, "git", "add", "-A"); err != nil {
		return false, ""
	}
	nsOut, err := run(ctx, workspace, nil, "git", "diff", "--name-status", "--cached", "HEAD")
	if err != nil {
		return false, ""
	}
	entries := parseNameStatus(nsOut)
	prodPkgs := changedProdPackages(entries)
	if len(prodPkgs) == 0 {
		return false, "" // no production change: not a green-gate-earning dilution
	}

	diffOut, err := run(ctx, workspace, nil, "git", "diff", "--cached", "--unified=0",
		"--src-prefix=a/", "--dst-prefix=b/", "HEAD", "--", "*_test.go")
	if err != nil {
		return false, ""
	}
	byFile := parseUnifiedDiff(diffOut)

	var findings []string
	for file, fh := range byFile {
		if !prodPkgs[filepath.Dir(file)] {
			continue // package linkage: only judge tests of changed-prod packages
		}
		if removed, added, snippets := assertionErosion(fh); removed > added {
			findings = append(findings, fmt.Sprintf(
				"%s net-removed %d assertion(s): %s",
				file, removed-added, strings.Join(firstN(snippets, 3), "; ")))
		}
		for _, c := range fixtureLiteralChurn(fh) {
			findings = append(findings, file+" "+c)
		}
	}
	findings = append(findings, fixtureFileChanges(entries, prodPkgs)...)

	if len(findings) == 0 {
		return false, ""
	}
	sort.Strings(findings) // deterministic order across map iteration
	detail := "production code changed and its tests weakened their own coverage " +
		"(confirm the changed behavior is still covered, not dodged): " +
		strings.Join(findings, "; ")
	return true, truncateOutput(detail)
}
```

Note on imports: after this task the file's grouped import block is
`"context"`, `"fmt"`, `"path/filepath"`, `"regexp"`, `"sort"`, `"strings"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run TestCheckTestDilution -count=1`
Expected: PASS.

- [ ] **Step 5: Bite-check the two signals**

Temporarily change `removed > added` to `removed > added+100` in `checkTestDilution` and run `go test ./pkg/foreman/agent/ -run TestCheckTestDilution_FiresOnNetRemovedAssertions -count=1`; expect FAIL. Restore. Temporarily make `fixtureLiteralChurn` return `nil` unconditionally and run `TestCheckTestDilution_FiresOnFixtureRelocation_1322Shape`; expect FAIL. Restore. Confirm both tests pass again.

- [ ] **Step 6: Commit**

```bash
git add pkg/foreman/agent/test_dilution_gate.go pkg/foreman/agent/test_dilution_gate_test.go
git commit -s -m "feat(foreman): checkTestDilution orchestrator, package-linked, fail-open (#1332)"
```

---

### Task 6: Register the check as `tierAdvisory` and verify end to end

Wire `checkTestDilution` into the gate registry and prove it reaches the reviewer path as an advisory (not a blocking failure).

**Files:**
- Modify: `pkg/foreman/agent/coder_gate.go` (the slice returned by `gateCheckRegistry`)
- Test: `pkg/foreman/agent/test_dilution_gate_test.go`

**Interfaces:**
- Consumes: `checkTestDilution` (Task 5); `gateCheck`, `tierAdvisory`, `runGateChecks`, `gateCheckRegistry` (existing).
- Produces: a registered `"test-dilution"` advisory check.

- [ ] **Step 1: Write the failing test**

Append to `pkg/foreman/agent/test_dilution_gate_test.go`:

```go
func TestTestDilution_RegisteredAsAdvisory(t *testing.T) {
	var found bool
	var tier gateTier
	for _, c := range gateCheckRegistry("", "", nil) {
		if c.name == "test-dilution" {
			found = true
			tier = c.tier
		}
	}
	if !found {
		t.Fatal(`gateCheckRegistry is missing the "test-dilution" check`)
	}
	if tier != tierAdvisory {
		t.Errorf("test-dilution tier = %v, want tierAdvisory", tier)
	}
}

func TestTestDilution_SurfacesAsAdvisoryNotBlocking(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +20 @@
-	src := "https://huggingface.co/org/model/f.gguf"
+	src := "https://example.com/org/model/f.gguf"
`
	run := dilutionRunner(ns, diff, nil, nil, nil)
	blocking, advisories := runGateChecks(context.Background(), "/w", run,
		[]gateCheck{{name: "test-dilution", tier: tierAdvisory, fn: checkTestDilution}})
	if len(blocking) != 0 {
		t.Errorf("test-dilution must never block; got %d blocking", len(blocking))
	}
	if len(advisories) != 1 || advisories[0].Check != "test-dilution" {
		t.Fatalf("expected one test-dilution advisory; got %+v", advisories)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/foreman/agent/ -run 'TestTestDilution_Registered|TestTestDilution_Surfaces' -count=1`
Expected: `TestTestDilution_RegisteredAsAdvisory` FAILS (`missing the "test-dilution" check`); `TestTestDilution_SurfacesAsAdvisoryNotBlocking` already passes (it builds its own registry).

- [ ] **Step 3: Write minimal implementation**

In `pkg/foreman/agent/coder_gate.go`, in the slice returned by `gateCheckRegistry`, add this entry after the `issue-example` entry (keep it last):

```go
		{
			// test-dilution (#1332): advisory. Surfaces to the reviewer when a
			// submission that changes production code also weakens the tests
			// covering it (net-removed assertions, relocated/deleted fixtures).
			// No lang: fixture and assertion erosion are not Go-specific in
			// principle, and the check is fail-open on any language it cannot
			// parse. Never blocks; the coder never sees it.
			name: "test-dilution",
			tier: tierAdvisory,
			fn:   checkTestDilution,
		},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/foreman/agent/ -run 'TestTestDilution_Registered|TestTestDilution_Surfaces' -count=1`
Expected: PASS.

- [ ] **Step 5: Full-package test, lint, gofmt**

Run each, expect clean:

```bash
gofmt -l pkg/foreman/agent/test_dilution_gate.go pkg/foreman/agent/test_dilution_gate_test.go pkg/foreman/agent/coder_gate.go
GOOS=linux ./bin/golangci-lint run pkg/foreman/agent/
go test ./pkg/foreman/agent/ -count=1
```

Expected: `gofmt` prints nothing; lint prints `0 issues.`; `go test` prints `ok  github.com/defilantech/llmkube/pkg/foreman/agent`.

- [ ] **Step 6: Commit**

```bash
git add pkg/foreman/agent/coder_gate.go pkg/foreman/agent/test_dilution_gate_test.go
git commit -s -m "feat(foreman): register test-dilution advisory in the coder gate (#1332)"
```

---

## Post-implementation

After all six tasks are green:

1. **File the deferred-signal follow-ups** (from the spec's non-goals) so they are not lost: (a) string-match-only detection for command-string/shell changes (#1309 mode); (b) a coverage/mutation-delta investigation, noting it does not subsume this slice; (c) assertion-value churn.
2. **Open the PR** into `defilantech/LLMKube` (base `main`, head `Defilan:<branch>`), `Fixes #1332`, band-3 AI-assisted disclosure. The PR body should note the two signals shipped and that #1309-mode is a deferred follow-up.

## Self-Review notes (author)

- **Spec coverage:** advisory posture (Tasks 5-6), removed-assertions signal with net-erosion rule (Task 2 + Task 5), relocated/deleted fixtures both literal-level (Task 3) and file-level (Task 4), package linkage (Task 4 + Task 5), reviewer surfacing (Task 6), fail-open (Task 5), kill-switch (Task 6, via the `"test-dilution"` name), #1322 bite-check (Task 5). Deferred non-goals recorded in Post-implementation.
- **Type consistency:** `fileHunks`, `nameStatusEntry`, `commandRunner`, `assertionErosion`/`fixtureLiteralChurn`/`fixtureFileChanges`/`checkTestDilution` signatures match across the Interfaces block and every task.
- **No placeholders:** every code and test step is complete and runnable.
