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
	k := make([]string, 0, len(m))
	for f := range m {
		k = append(k, f)
	}
	return k
}
