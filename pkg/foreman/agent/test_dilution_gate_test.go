package agent

import (
	"reflect"
	"strings"
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
	k := make([]string, 0, len(m))
	for f := range m {
		k = append(k, f)
	}
	return k
}

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

func TestParseNameStatus_ModifyAndRename(t *testing.T) {
	out := "M\tpkg/model/classifier.go\n" +
		"D\tpkg/model/testdata/real.json\n" +
		"R100\tpkg/model/testdata/a.json\tpkg/model/testdata/b.json\n"
	got := parseNameStatus(out)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	wantOldPath := "pkg/model/testdata/a.json"
	wantNewPath := "pkg/model/testdata/b.json"
	if got[2].Code[0] != 'R' || got[2].OldPath != wantOldPath || got[2].Path != wantNewPath {
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

func TestFixtureFileChanges_CrossPackageRenameOutOfChangedPkgFires(t *testing.T) {
	// Moved OUT of a changed package into an untouched one: the changed
	// package lost the fixture, so this must fire (was a false negative).
	entries := []nameStatusEntry{
		{Code: "R100", OldPath: "pkg/model/testdata/a.json", Path: "pkg/other/testdata/a.json"},
	}
	prod := map[string]bool{"pkg/model": true}
	got := fixtureFileChanges(entries, prod)
	if len(got) != 1 || !strings.Contains(got[0], "pkg/model/testdata/a.json -> pkg/other/testdata/a.json") {
		t.Fatalf("expected a relocation finding attributed to the changed source package; got %v", got)
	}
}

func TestFixtureFileChanges_CrossPackageRenameIntoChangedPkgSilent(t *testing.T) {
	// Moved INTO a changed package from an untouched one: nothing was lost
	// from the changed package, so this must stay silent (was a false positive).
	entries := []nameStatusEntry{
		{Code: "R100", OldPath: "pkg/other/testdata/a.json", Path: "pkg/model/testdata/a.json"},
	}
	prod := map[string]bool{"pkg/model": true}
	if got := fixtureFileChanges(entries, prod); len(got) != 0 {
		t.Fatalf("a fixture moved INTO the changed package must not fire; got %v", got)
	}
}
