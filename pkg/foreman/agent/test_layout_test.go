package agent

import (
	"reflect"
	"testing"
)

// TestTestLayoutIsZero covers the IsZero helper: empty and whitespace-only
// layouts are zero; anything with a non-blank root is not.
func TestTestLayoutIsZero(t *testing.T) {
	cases := []struct {
		name string
		l    TestLayout
		want bool
	}{
		{"both empty", TestLayout{}, true},
		{"whitespace only", TestLayout{TestRoot: "  ", SourceRoot: "\t"}, true},
		{"test root set", TestLayout{TestRoot: "tests", SourceRoot: "src"}, false},
		{"source root set", TestLayout{TestRoot: "src/test/java", SourceRoot: "src/main/java"}, false},
		{"only test root", TestLayout{TestRoot: "tests"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.l.IsZero(); got != c.want {
				t.Fatalf("IsZero() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestTestTargetsWithLayoutZeroDelegates proves the zero layout is exactly
// the built-in beside-the-code behaviour: it must return byte-for-byte what
// testTargetsForPath returns, for each convention the built-in knows.
func TestTestTargetsWithLayoutZeroDelegates(t *testing.T) {
	zero := TestLayout{}
	for _, p := range []string{"foo_test.go", "a/b/x.test.ts", "test_x.py"} {
		want := testTargetsForPath(p)
		got := testTargetsWithLayout(p, zero)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("zero layout for %q = %v, want testTargetsForPath %v", p, got, want)
		}
	}
}

// TestTestTargetsWithLayoutParallelTree covers the parallel test-tree
// mappings this feature exists to provide: the directory is rewritten to the
// subject root and the test decoration is stripped from the basename.
func TestTestTargetsWithLayoutParallelTree(t *testing.T) {
	cases := []struct {
		name string
		l    TestLayout
		p    string
		want []string
	}{
		{
			name: "java parallel tree",
			l:    TestLayout{TestRoot: "src/test/java", SourceRoot: "src/main/java"},
			p:    "src/test/java/a/FooTest.java",
			want: []string{"src/main/java/a/Foo.java"},
		},
		{
			name: "python parallel tree",
			l:    TestLayout{TestRoot: "tests", SourceRoot: "src"},
			p:    "tests/test_foo.py",
			want: []string{"src/foo.py"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := testTargetsWithLayout(c.p, c.l); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestTestTargetsWithLayoutOutsideTestRoot verifies a path that is not under
// TestRoot falls back to the beside-the-code behaviour and still resolves.
func TestTestTargetsWithLayoutOutsideTestRoot(t *testing.T) {
	l := TestLayout{TestRoot: "tests", SourceRoot: "src"}
	if got := testTargetsWithLayout("pkg/x_test.go", l); !reflect.DeepEqual(got, []string{"pkg/x.go"}) {
		t.Fatalf("got %v, want [pkg/x.go]", got)
	}
}

// TestTestTargetsWithLayoutPrefixTrap verifies TestRoot matching is on whole
// path segments: "testsuite/x_test.go" is NOT under TestRoot "tests", so it
// must fall back to the beside-the-code mapping, not be rewritten to "src".
func TestTestTargetsWithLayoutPrefixTrap(t *testing.T) {
	l := TestLayout{TestRoot: "tests", SourceRoot: "src"}
	got := testTargetsWithLayout("testsuite/x_test.go", l)
	want := testTargetsForPath("testsuite/x_test.go")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want testTargetsForPath %v (must not treat testsuite as under tests)", got, want)
	}
	if len(got) != 0 && got[0] == "src/x.go" {
		t.Fatalf("got %v: the substring trap fired, testsuite was wrongly rewritten to src", got)
	}
}

// TestTestTargetsWithLayoutNonTestFile verifies a non-test file yields no
// candidates, whether under the zero layout or a declared layout.
func TestTestTargetsWithLayoutNonTestFile(t *testing.T) {
	if got := testTargetsWithLayout("README.md", TestLayout{}); len(got) != 0 {
		t.Fatalf("zero layout non-test file: got %v, want empty", got)
	}
	// A source file that sits in the subject tree, not the test tree: not a
	// test file, so no candidates.
	l := TestLayout{TestRoot: "src/test/java", SourceRoot: "src/main/java"}
	if got := testTargetsWithLayout("src/main/java/a/Foo.java", l); len(got) != 0 {
		t.Fatalf("layout non-test source file: got %v, want empty", got)
	}
}

// TestTestTargetsWithLayoutEmptyPath verifies an empty path with a zero-value
// layout does not panic and returns nothing.
func TestTestTargetsWithLayoutEmptyPath(t *testing.T) {
	if got := testTargetsWithLayout("", TestLayout{}); len(got) != 0 {
		t.Fatalf("empty path: got %v, want empty", got)
	}
}

// TestStripTestDecoration pins the decoration-stripping rules, including the
// Java suffix form that the built-in testTargetsForPath does not handle.
func TestStripTestDecoration(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"foo_test.go", "foo.go"},
		{"x.test.ts", "x.ts"},
		{"x.spec.tsx", "x.tsx"},
		{"test_foo.py", "foo.py"},
		{"foo_test.py", "foo.py"},
		{"FooTest.java", "Foo.java"},
		{"Foo.java", ""}, // not a test file
		{"README.md", ""},
		{"Makefile", ""},
	}
	for _, c := range cases {
		if got := stripTestDecoration(c.in); got != c.want {
			t.Errorf("stripTestDecoration(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
