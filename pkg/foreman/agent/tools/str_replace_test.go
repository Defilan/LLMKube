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

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execStrReplace marshals the args and runs the tool against ws.
func execStrReplace(t *testing.T, ws, path, oldString, newString string) error {
	t.Helper()
	tool := &StrReplaceTool{Workspace: ws}
	buf, err := json.Marshal(map[string]string{
		"path": path, "old_string": oldString, "new_string": newString,
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	_, err = tool.Execute(context.Background(), buf)
	return err
}

func execStrReplaceWithExpected(t *testing.T, ws, path, oldString, newString string, expected int) error {
	t.Helper()
	tool := &StrReplaceTool{Workspace: ws}
	buf, err := json.Marshal(map[string]any{
		"path": path, "old_string": oldString, "new_string": newString,
		"expected_replacements": expected,
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	_, err = tool.Execute(context.Background(), buf)
	return err
}

func seedFile(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

func readBack(t *testing.T, ws, name string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(ws, name))
	if err != nil {
		t.Fatalf("readback %s: %v", name, err)
	}
	return string(got)
}

// --- Tier 1: exact match (unchanged) --------------------------------------

func TestStrReplace_ExactMatch(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	err := execStrReplace(t, ws, "f.go", "\treturn userCount", "\treturn userCount + 1")
	if err != nil {
		t.Fatalf("expected exact match, got error: %v", err)
	}
	got := readBack(t, ws, "f.go")
	if !strings.Contains(got, "return userCount + 1") {
		t.Errorf("replacement not applied: %q", got)
	}
}

func TestStrReplace_ExactMatchReportsMatchedVia(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	tool := &StrReplaceTool{Workspace: ws}
	buf, _ := json.Marshal(map[string]string{
		"path": "f.go", "old_string": "\treturn userCount",
		"new_string": "\treturn 0",
	})
	res, err := tool.Execute(context.Background(), buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("unexpected output type %T", res.Output)
	}
	if out["matched_via"] != "exact" {
		t.Errorf("expected matched_via=exact, got %q", out["matched_via"])
	}
}

func TestStrReplace_ExactWinsOverTrailingWS(t *testing.T) {
	// When both exact and trailing-ws would match, exact must win.
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	tool := &StrReplaceTool{Workspace: ws}
	buf, _ := json.Marshal(map[string]string{
		"path": "f.go", "old_string": "\treturn userCount",
		"new_string": "\treturn 0",
	})
	res, err := tool.Execute(context.Background(), buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["matched_via"] != "exact" {
		t.Errorf("expected matched_via=exact (not trailing-ws), got %q",
			out["matched_via"])
	}
}

// --- Tier 2: trailing-whitespace-insensitive match -------------------------

func TestStrReplace_TrailingWSRecovery(t *testing.T) {
	ws := makeWorkspace(t)
	// File has trailing spaces on line 2; old_string has none.
	// Use multi-line so old_string is NOT a substring of file content.
	src := "func compute() int {\n\treturn userCount   \n}\n"
	seedFile(t, ws, "f.go", src)
	old := "func compute() int {\n\treturn userCount\n}"
	newS := "func compute() int {\n\treturn userCount + 1\n}"
	err := execStrReplace(t, ws, "f.go", old, newS)
	if err != nil {
		t.Fatalf("expected trailing-ws recovery, got error: %v", err)
	}
	got := readBack(t, ws, "f.go")
	if !strings.Contains(got, "return userCount + 1") {
		t.Errorf("replacement not applied: %q", got)
	}
	// The trailing spaces from the original file should be gone.
	if strings.Contains(got, "userCount   ") {
		t.Errorf("trailing spaces leaked: %q", got)
	}
}

func TestStrReplace_TrailingWSReportsMatchedVia(t *testing.T) {
	ws := makeWorkspace(t)
	// File has trailing spaces; old_string has none.
	// Multi-line so old_string is NOT a substring of file content.
	src := "func compute() int {\n\treturn userCount   \n}\n"
	seedFile(t, ws, "f.go", src)
	tool := &StrReplaceTool{Workspace: ws}
	old := "func compute() int {\n\treturn userCount\n}"
	newS := "func compute() int {\n\treturn 0\n}"
	buf, _ := json.Marshal(map[string]string{
		"path": "f.go", "old_string": old, "new_string": newS,
	})
	res, err := tool.Execute(context.Background(), buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["matched_via"] != "trailing-ws" {
		t.Errorf("expected matched_via=trailing-ws, got %q", out["matched_via"])
	}
}

func TestStrReplace_TrailingWSRecoveryMultiLine(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount   \n"
	src += "\treturn otherCount\t\n}\n"
	seedFile(t, ws, "f.go", src)
	old := "\treturn userCount\n\treturn otherCount"
	newS := "\treturn userCount + 1\n\treturn otherCount + 1"
	err := execStrReplace(t, ws, "f.go", old, newS)
	if err != nil {
		t.Fatalf("expected trailing-ws recovery, got error: %v", err)
	}
	got := readBack(t, ws, "f.go")
	if !strings.Contains(got, "return userCount + 1") {
		t.Errorf("replacement not applied: %q", got)
	}
}

func TestStrReplace_TrailingWSRefusesAmbiguous(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func alpha() int {\n\treturn count   \n}\n"
	src += "func beta() int {\n\treturn count\t\n}\n"
	seedFile(t, ws, "f.go", src)
	err := execStrReplace(t, ws, "f.go", "\treturn count", "\treturn total")
	if err == nil {
		t.Fatal("expected refusal on ambiguous trailing-ws match, got nil error")
	}
	if got := readBack(t, ws, "f.go"); got != src {
		t.Errorf("file must be unchanged on refusal, got %q", got)
	}
}

// --- Tier 3: uniform-indent match -----------------------------------------

func TestStrReplace_IndentRecovery(t *testing.T) {
	ws := makeWorkspace(t)
	// File has 4-space indent.
	src := "func compute() int {\n    return userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	// Model's old_string has 2-space indent (off by 2).
	// Multi-line so old_string is NOT a substring of file content.
	old := "func compute() int {\n  return userCount\n}"
	newS := "func compute() int {\n  return userCount + 1\n}"
	err := execStrReplace(t, ws, "f.go", old, newS)
	if err != nil {
		t.Fatalf("expected indent recovery, got error: %v", err)
	}
	got := readBack(t, ws, "f.go")
	// new_string should be re-indented to the file's actual indent (4 spaces).
	if !strings.Contains(got, "    return userCount + 1") {
		t.Errorf("replacement not re-indented correctly: %q", got)
	}
}

func TestStrReplace_IndentRecoveryReportsMatchedVia(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n    return userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	tool := &StrReplaceTool{Workspace: ws}
	// Multi-line so old_string is NOT a substring of file content.
	old := "func compute() int {\n  return userCount\n}"
	newS := "func compute() int {\n  return 0\n}"
	buf, _ := json.Marshal(map[string]string{
		"path": "f.go", "old_string": old, "new_string": newS,
	})
	res, err := tool.Execute(context.Background(), buf)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Output.(map[string]any)
	if out["matched_via"] != "indent" {
		t.Errorf("expected matched_via=indent, got %q", out["matched_via"])
	}
}

func TestStrReplace_IndentRecoveryMultiLine(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n    return userCount\n"
	src += "    return otherCount\n}\n"
	seedFile(t, ws, "f.go", src)
	old := "return userCount\nreturn otherCount"
	newS := "return userCount + 1\nreturn otherCount + 1"
	err := execStrReplace(t, ws, "f.go", old, newS)
	if err != nil {
		t.Fatalf("expected indent recovery, got error: %v", err)
	}
	got := readBack(t, ws, "f.go")
	if !strings.Contains(got, "    return userCount + 1") {
		t.Errorf("replacement not re-indented correctly: %q", got)
	}
	if !strings.Contains(got, "    return otherCount + 1") {
		t.Errorf("second line not re-indented correctly: %q", got)
	}
}

func TestStrReplace_IndentRecoveryWithTabs(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	// Multi-line so old_string is NOT a substring of file content.
	old := "func compute() int {\nreturn userCount\n}"
	newS := "func compute() int {\nreturn userCount + 1\n}"
	err := execStrReplace(t, ws, "f.go", old, newS)
	if err != nil {
		t.Fatalf("expected indent recovery, got error: %v", err)
	}
	got := readBack(t, ws, "f.go")
	if !strings.Contains(got, "\treturn userCount + 1") {
		t.Errorf("replacement not re-indented with tab: %q", got)
	}
}

func TestStrReplace_IndentRefusesAmbiguous(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func alpha() int {\n    return count\n}\n"
	src += "func beta() int {\n    return count\n}\n"
	seedFile(t, ws, "f.go", src)
	err := execStrReplace(t, ws, "f.go", "return count", "return total")
	if err == nil {
		t.Fatal("expected refusal on ambiguous indent match, got nil error")
	}
	if got := readBack(t, ws, "f.go"); got != src {
		t.Errorf("file must be unchanged on refusal, got %q", got)
	}
}

// --- Safety rails ---------------------------------------------------------

func TestStrReplace_ExpectedReplacementsBypassesFallbacks(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount   \n}\n"
	seedFile(t, ws, "f.go", src)
	err := execStrReplaceWithExpected(t, ws, "f.go",
		"\treturn userCount", "x", 2)
	if err == nil {
		t.Fatal("expected count-mismatch error for multi-replace, got nil")
	}
	if got := readBack(t, ws, "f.go"); got != src {
		t.Errorf("file must be unchanged, got %q", got)
	}
}

func TestStrReplace_NoOpRejectionUnchanged(t *testing.T) {
	ws := makeWorkspace(t)
	src := "func compute() int {\n\treturn userCount\n}\n"
	seedFile(t, ws, "f.go", src)
	err := execStrReplace(t, ws, "f.go", "\treturn userCount",
		"\treturn userCount")
	if err == nil {
		t.Fatal("expected an error for identical old_string/new_string, got nil")
	}
	if !strings.Contains(err.Error(), "identical") {
		t.Errorf("expected an 'identical' no-op error, got: %v", err)
	}
	if got := readBack(t, ws, "f.go"); got != src {
		t.Errorf("file must be unchanged, got %q", got)
	}
}

// --- Schema description mentions fallbacks --------------------------------

func TestStrReplace_SchemaMentionsFallbacks(t *testing.T) {
	tool := &StrReplaceTool{}
	desc := tool.Schema().Description
	if !strings.Contains(desc, "trailing") {
		t.Errorf("schema description should mention trailing-whitespace "+
			"fallback, got: %q", desc)
	}
	if !strings.Contains(desc, "indentation") {
		t.Errorf("schema description should mention indentation fallback, "+
			"got: %q", desc)
	}
}

// --- Failure escalation preserved -----------------------------------------

func TestStrReplace_FailureEscalationPreserved(t *testing.T) {
	ws := makeWorkspace(t)
	seedFile(t, ws, "f.go", "package x\n\nfunc A() {}\n")
	tool := &StrReplaceTool{Workspace: ws}
	bad := strReplaceArgsJSON(t, "f.go",
		"ZZZ_NO_SUCH_LINE_ANYWHERE_ZZZ", "func B() {}")

	_, err1 := tool.Execute(context.Background(), bad)
	if err1 == nil {
		t.Fatal("first failing edit should error")
	}
	if strings.Contains(err1.Error(), "STOP calling str_replace") {
		t.Fatalf("first failure should be a normal hint, not escalated: %v",
			err1)
	}

	_, err2 := tool.Execute(context.Background(), bad)
	if err2 == nil {
		t.Fatal("second failing edit should error")
	}
	if !strings.Contains(err2.Error(), "STOP calling str_replace") {
		t.Errorf("second failure should escalate with a STOP directive; "+
			"got: %v", err2)
	}
}

// --- Unit tests for new helper functions ----------------------------------

func TestApplyTrailingWSMatch_SingleLine(t *testing.T) {
	tool := &StrReplaceTool{}
	content := "func compute() int {\n\treturn userCount   \n}\n"
	old := "\treturn userCount"
	newS := "\treturn userCount + 1"
	result, note, ok := tool.applyTrailingWSMatch(content, old, newS)
	if !ok {
		t.Fatal("applyTrailingWSMatch should match")
	}
	if !strings.Contains(result, "return userCount + 1") {
		t.Errorf("replacement not applied: %q", result)
	}
	if !strings.Contains(note, "trailing-whitespace") {
		t.Errorf("note should mention trailing-whitespace: %q", note)
	}
}

func TestApplyTrailingWSMatch_Ambiguous(t *testing.T) {
	tool := &StrReplaceTool{}
	content := "func a() {\n\treturn x   \n}\nfunc b() {\n\treturn x\t\n}\n"
	old := "\treturn x"
	_, _, ok := tool.applyTrailingWSMatch(content, old, "\treturn y")
	if ok {
		t.Fatal("applyTrailingWSMatch should refuse ambiguous match")
	}
}

func TestApplyIndentMatch_SingleLine(t *testing.T) {
	tool := &StrReplaceTool{}
	content := "func compute() int {\n    return userCount\n}\n"
	old := "  return userCount"
	newS := "  return userCount + 1"
	result, note, ok := tool.applyIndentMatch(content, old, newS)
	if !ok {
		t.Fatal("applyIndentMatch should match")
	}
	if !strings.Contains(result, "    return userCount + 1") {
		t.Errorf("replacement not re-indented correctly: %q", result)
	}
	if !strings.Contains(note, "uniform-indent") {
		t.Errorf("note should mention uniform-indent: %q", note)
	}
}

func TestApplyIndentMatch_NoIndentInOld(t *testing.T) {
	tool := &StrReplaceTool{}
	content := "func compute() int {\n\treturn userCount\n}\n"
	old := "return userCount"
	newS := "return userCount + 1"
	result, _, ok := tool.applyIndentMatch(content, old, newS)
	if !ok {
		t.Fatal("applyIndentMatch should match")
	}
	if !strings.Contains(result, "\treturn userCount + 1") {
		t.Errorf("replacement not re-indented with tab: %q", result)
	}
}

func TestApplyIndentMatch_Ambiguous(t *testing.T) {
	tool := &StrReplaceTool{}
	content := "func a() {\n    return x\n}\nfunc b() {\n    return x\n}\n"
	old := "return x"
	_, _, ok := tool.applyIndentMatch(content, old, "return y")
	if ok {
		t.Fatal("applyIndentMatch should refuse ambiguous match")
	}
}

func TestCommonLeadingIndent(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{"all same indent", []string{"    a", "    b"}, "    "},
		{"mixed indent", []string{"    a", "  b"}, "  "},
		{"no indent", []string{"a", "b"}, ""},
		{"with empty line", []string{"    a", "", "    b"}, "    "},
		{"all empty", []string{"", ""}, ""},
		{"tabs", []string{"\ta", "\tb"}, "\t"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commonLeadingIndent(tt.lines)
			if got != tt.want {
				t.Errorf("commonLeadingIndent(%q) = %q, want %q",
					tt.lines, got, tt.want)
			}
		})
	}
}

func TestStripLeadingIndent(t *testing.T) {
	lines := []string{"    a", "    b", ""}
	got := stripLeadingIndent(lines, "    ")
	want := []string{"a", "b", ""}
	if len(got) != len(want) {
		t.Fatalf("stripLeadingIndent: got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stripLeadingIndent[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAddLeadingIndent(t *testing.T) {
	lines := []string{"a", "b", ""}
	got := addLeadingIndent(lines, "    ")
	want := []string{"    a", "    b", "    "}
	if len(got) != len(want) {
		t.Fatalf("addLeadingIndent: got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("addLeadingIndent[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
