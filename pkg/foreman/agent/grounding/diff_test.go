package grounding

import (
	"context"
	"testing"
)

func TestAddedLines_AttributesLineNumbers(t *testing.T) {
	diff := "diff --git a/docs/x.md b/docs/x.md\n" +
		"--- a/docs/x.md\n+++ b/docs/x.md\n" +
		"@@ -0,0 +1,2 @@\n+first line\n+second line\n" +
		"@@ -10,0 +20,1 @@\n+twentieth\n"
	run := func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, error) {
		return diff, nil
	}
	got, err := AddedLines(context.Background(), "/ws", run, "main", []string{"*.md"})
	if err != nil {
		t.Fatal(err)
	}
	want := []AddedLine{
		{File: "docs/x.md", Line: 1, Text: "first line"},
		{File: "docs/x.md", Line: 2, Text: "second line"},
		{File: "docs/x.md", Line: 20, Text: "twentieth"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}
