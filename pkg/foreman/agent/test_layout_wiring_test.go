package agent

import (
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// #1579: a declared test layout must let the scope-overlap rail fold a test
// file back to its subject when tests do not sit beside the code. Without it
// these five language families reproduce #1447 unchanged: the coder writes the
// right tests, the rail says the diff touches none of the named files, and the
// task burns its budget with no PR.

func TestScopeLayout_LanguageConventions(t *testing.T) {
	cases := []struct {
		lang     string
		issueRef string
		testFile string
		ext      string
		layout   TestLayout
	}{
		{"ruby", "lib/foo.rb", "spec/foo_spec.rb", ".rb",
			TestLayout{TestRoot: "spec", SourceRoot: "lib"}},
		{"java", "src/main/java/Foo.java", "src/test/java/FooTest.java", ".java",
			TestLayout{TestRoot: "src/test/java", SourceRoot: "src/main/java"}},
		{"csharp", "src/Foo.cs", "tests/FooTests.cs", ".cs",
			TestLayout{TestRoot: "tests", SourceRoot: "src"}},
		{"scala", "src/Foo.scala", "test/FooSpec.scala", ".scala",
			TestLayout{TestRoot: "test", SourceRoot: "src"}},
		{"php", "src/Foo.php", "tests/FooTest.php", ".php",
			TestLayout{TestRoot: "tests", SourceRoot: "src"}},
	}
	for _, c := range cases {
		t.Run(c.lang, func(t *testing.T) {
			extra := map[string]any{}
			got := enforceReviewerScopeOverlap(logr.Discard(), extra,
				"Add tests for `"+c.issueRef+"`.", []string{c.testFile},
				foremanv1alpha1.AgenticTaskVerdictGo, []string{c.ext}, c.layout)
			if got != foremanv1alpha1.AgenticTaskVerdictGo {
				t.Errorf("%s demoted: matched=%v refs=%v", c.lang,
					extra["scopeMatched"], extra["scopeRefs"])
			}
		})
	}
}

// A zero layout must behave exactly as before: beside-the-code only.
func TestScopeLayout_ZeroLayoutPreservesBesideTheCode(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra,
		"Add tests for `src/lib/foo.ts`.", []string{"src/lib/foo.test.ts"},
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".ts"}, TestLayout{})
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("beside-the-code must still match, matched=%v", extra["scopeMatched"])
	}
}

// Jory's #1447 case must stay green with a layout declared and without one.
func TestScopeLayout_Issue1447StaysGreen(t *testing.T) {
	body := "Add unit tests for `src/lib/automation-sync.ts`, " +
		"`src/lib/groomer/groomer-lock.ts`, `src/lib/resolve-actor.ts`."
	diff := []string{
		"src/lib/automation-sync.test.ts",
		"src/lib/groomer/groomer-lock.test.ts",
		"src/lib/resolve-actor.test.ts",
	}
	for _, l := range []TestLayout{{}, {TestRoot: "tests", SourceRoot: "src"}} {
		extra := map[string]any{}
		got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, diff,
			foremanv1alpha1.AgenticTaskVerdictGo, []string{".ts"}, l)
		if got != foremanv1alpha1.AgenticTaskVerdictGo {
			t.Errorf("#1447 regressed with layout %+v: matched=%v", l, extra["scopeMatched"])
		}
	}
}

// Real drift must still demote with a layout declared: the rail's whole point.
func TestScopeLayout_RealDriftStillDemotes(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra,
		"Fix `src/main/java/Foo.java`.", []string{"src/main/java/Unrelated.java"},
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".java"},
		TestLayout{TestRoot: "src/test/java", SourceRoot: "src/main/java"})
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Errorf("real drift must demote, got %v", got)
	}
}

func TestStripTestDecoration_NewConventions(t *testing.T) {
	cases := map[string]string{
		"foo_spec.rb":   "foo.rb",    // Ruby RSpec
		"FooTests.cs":   "Foo.cs",    // C# / NUnit
		"FooSpec.scala": "Foo.scala", // Scala
		"FooTest.java":  "Foo.java",  // pre-existing, must not regress
		"foo_test.go":   "foo.go",    // pre-existing
		"test_foo.py":   "foo.py",    // pre-existing
		"foo.test.ts":   "foo.ts",    // pre-existing
		"plain.go":      "",          // not a test file
	}
	for in, want := range cases {
		if got := stripTestDecoration(in); got != want {
			t.Errorf("stripTestDecoration(%q)=%q want %q", in, got, want)
		}
	}
}
