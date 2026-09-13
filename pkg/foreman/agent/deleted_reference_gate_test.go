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

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestDeletedIssueReferences_RemovedBareRef(t *testing.T) {
	diff := `@@ -1,3 +1,2 @@
 context line
-// this field exists because of #1234
+// nothing here
`
	got := deletedIssueReferences(diff)
	want := []string{"#1234"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeletedIssueReferences_RemovedFullRef(t *testing.T) {
	diff := `@@ -1,3 +1,2 @@
 context line
-// see defilantech/LLMKube#987 for why this must stay
+// gone
`
	got := deletedIssueReferences(diff)
	want := []string{"defilantech/LLMKube#987"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeletedIssueReferences_AddedNotReported(t *testing.T) {
	diff := `@@ -1,3 +1,2 @@
 context line
-// old
+// this one cites #1234 but is ADDED
`
	got := deletedIssueReferences(diff)
	if len(got) != 0 {
		t.Fatalf("expected no removed refs, got %v", got)
	}
}

func TestDeletedIssueReferences_ContextNotReported(t *testing.T) {
	diff := `@@ -1,3 +1,2 @@
 // context line cites #1234 but is unchanged
-// removed line has no ref
`
	got := deletedIssueReferences(diff)
	if len(got) != 0 {
		t.Fatalf("expected no removed refs, got %v", got)
	}
}

func TestDeletedIssueReferences_FileHeaderNotRemovedLine(t *testing.T) {
	// The "--- a/foo.go" header must not be mistaken for a removed line, and
	// even if it were scanned it carries no #N anyway. Guard the shape too: a
	// removed-looking header with a bare "#N" must still be skipped.
	diff := `--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,2 @@
-// removed line cites #4242
`
	got := deletedIssueReferences(diff)
	want := []string{"#4242"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (file header should not be scanned)", got, want)
	}
}

func TestDeletedIssueReferences_DuplicatesCollapse(t *testing.T) {
	diff := `@@ -1,6 +1,1 @@
-// first cite of #7
-// second cite of #7
-// another cite of defilantech/LLMKube#7
`
	got := deletedIssueReferences(diff)
	want := []string{"#7", "defilantech/LLMKube#7"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeletedIssueReferences_StableOrder(t *testing.T) {
	diff := `@@ -1,6 +1,1 @@
-// cites #500
-// cites defilantech/LLMKube#10
-// cites #200
`
	want := []string{"#200", "#500", "defilantech/LLMKube#10"}
	for i := 0; i < 100; i++ {
		got := deletedIssueReferences(diff)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: got %v, want %v (must be deterministic)", i, got, want)
		}
		if !sort.StringsAreSorted(got) {
			t.Fatalf("run %d: got %v, not sorted", i, got)
		}
	}
}

func TestDeletedIssueReferences_EmptyAndNoRefs(t *testing.T) {
	if got := deletedIssueReferences(""); got != nil {
		t.Fatalf("expected nil for empty diff, got %v", got)
	}
	diff := `@@ -1,3 +1,2 @@
 context
-// no references here at all
+added
`
	if got := deletedIssueReferences(diff); got != nil {
		t.Fatalf("expected nil when no removed refs, got %v", got)
	}
}

func TestRecordDeletedIssueReferences_WritesFlag(t *testing.T) {
	extra := map[string]any{}
	diff := `--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,2 @@
-// removed, cites #1234 and defilantech/LLMKube#987
`
	recordDeletedIssueReferences(extra, diff)

	refs, ok := extra["deletedIssueReferences"].([]string)
	if !ok {
		t.Fatalf("deletedIssueReferences missing or wrong type: %T", extra["deletedIssueReferences"])
	}
	want := []string{"#1234", "defilantech/LLMKube#987"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs got %v, want %v", refs, want)
	}
	note, ok := extra["deletedReferenceNote"].(string)
	if !ok || note == "" {
		t.Fatalf("expected a non-empty deletedReferenceNote string, got %q", note)
	}
}

func TestRecordDeletedIssueReferences_NoRefsNoKeys(t *testing.T) {
	extra := map[string]any{}
	recordDeletedIssueReferences(extra, "no removed lines here")
	if len(extra) != 0 {
		t.Fatalf("expected no keys when there are no removed refs, got %v", extra)
	}
}

func TestRecordDeletedReferenceDisabled_ToggleOffReturnsNothing(t *testing.T) {
	t.Setenv("FOREMAN_DELETED_REFERENCE", "0")
	extra := map[string]any{}
	diff := `@@ -1,3 +1,2 @@
-// removed, cites #1234
`
	recordDeletedIssueReferences(extra, diff)
	if len(extra) != 0 {
		t.Fatalf("expected nothing recorded when the rail is disabled, got %v", extra)
	}
}

func TestDeletedReferenceDisabled_DefaultEnabled(t *testing.T) {
	t.Setenv("FOREMAN_DELETED_REFERENCE", "")
	if deletedReferenceDisabled() {
		t.Fatal("expected the rail to be enabled by default (unset)")
	}
	t.Setenv("FOREMAN_DELETED_REFERENCE", "0")
	if !deletedReferenceDisabled() {
		t.Fatal("expected the rail to be disabled at FOREMAN_DELETED_REFERENCE=0")
	}
	t.Setenv("FOREMAN_DELETED_REFERENCE", "1")
	if deletedReferenceDisabled() {
		t.Fatal("only \"0\" disables the rail")
	}
}

// applyDeletedReferenceRailForTask degrades to the payload base branch name
// when the executor resolved no literal base SHA (non-coder-role agent, or the
// upstream fetch failed): the flag must still be computed from that branch's
// diff, the pre-#1769 posture.
func TestApplyDeletedReferenceRailForTask_FallsBackToBranchNameWhenNoResolvedBase(t *testing.T) {
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && strings.Join(args, " ") == "diff main...HEAD" {
			return "@@ -1,3 +1,2 @@\n-// removed, cites #1234\n", nil
		}
		return "", context.Canceled
	}
	task := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
			Payload: foremanv1alpha1.AgenticTaskPayload{BaseBranch: "main"},
		},
	}
	loopRes := &LoopResult{Terminal: &ToolResult{Extra: map[string]any{}}}

	applyDeletedReferenceRailForTask(context.Background(), task, "/ws", "", loopRes)

	refs, ok := loopRes.Terminal.Extra["deletedIssueReferences"].([]string)
	if !ok || len(refs) != 1 || refs[0] != "#1234" {
		t.Fatalf("fallback base branch must still be scanned; deletedIssueReferences = %v",
			loopRes.Terminal.Extra["deletedIssueReferences"])
	}
}
