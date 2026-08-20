package agent

import (
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// TestContentReferencesModule covers the deterministic content signal the
// scope-overlap rail falls back to when a test file is named for a feature
// rather than the module it covers (#1610). Each case pairs a module ref the
// issue names with a test body and the expected vouch. The NON-match cases are
// the load-bearing ones: a test importing a different module must never vouch
// for X, or the rail loses the drift signal it exists to keep.
func TestContentReferencesModule(t *testing.T) {
	tests := []struct {
		name        string
		testContent string
		modulePath  string
		want        bool
	}{
		{
			name:        "python import with package prefix",
			testContent: "import pr_reviewer.platform\n\n\ndef test_foo():\n    pass\n",
			modulePath:  "pr_reviewer/platform.py",
			want:        true,
		},
		{
			name:        "python from-import with package prefix",
			testContent: "from pr_reviewer.platform import build_client\n\n\ndef test_foo():\n    pass\n",
			modulePath:  "pr_reviewer/platform.py",
			want:        true,
		},
		{
			name:        "python from-import bare module",
			testContent: "from app import dedup\n\n\ndef test_dedup():\n    pass\n",
			modulePath:  "app.py",
			want:        true,
		},
		{
			name:        "python import bare module",
			testContent: "import app\n\n\ndef test_dedup():\n    pass\n",
			modulePath:  "app.py",
			want:        true,
		},
		{
			name:        "js import from relative",
			testContent: "import { dedup } from './util'\n\nit('dedups', () => {})\n",
			modulePath:  "src/util.ts",
			want:        true,
		},
		{
			name:        "js import from package subpath",
			testContent: "import { buildClient } from '../pr_reviewer/platform'\n\nit('builds', () => {})\n",
			modulePath:  "pr_reviewer/platform.py",
			want:        true,
		},
		{
			name:        "js require",
			testContent: "const { dedup } = require('./util')\n\ntest('dedups', () => {})\n",
			modulePath:  "src/util.ts",
			want:        true,
		},
		{
			name:        "ruby require_relative",
			testContent: "require_relative '../lib/foo'\n\nRSpec.describe Foo do\nend\n",
			modulePath:  "lib/foo.rb",
			want:        true,
		},
		{
			name:        "ruby require with package prefix",
			testContent: "require 'pr_reviewer/platform'\n\nRSpec.describe Platform do\nend\n",
			modulePath:  "pr_reviewer/platform.py",
			want:        true,
		},
		{
			name:        "go import path suffix",
			testContent: "import (\n\t\"example.com/proj/internal/foo\"\n\n\t\"testing\"\n)\n\nfunc TestFoo(t *testing.T) {}\n",
			modulePath:  "internal/foo/foo.go",
			want:        true,
		},
		{
			name:        "go import bare package",
			testContent: "import (\n\t\"foo\"\n\n\t\"testing\"\n)\n\nfunc TestFoo(t *testing.T) {}\n",
			modulePath:  "foo.go",
			want:        true,
		},
		{
			name:        "non-match: python imports a different module",
			testContent: "import other_module\n\n\ndef test_foo():\n    pass\n",
			modulePath:  "platform.py",
			want:        false,
		},
		{
			name:        "non-match: prefix overlap must not vouch",
			testContent: "import foo\n\n\ndef test_foo():\n    pass\n",
			modulePath:  "foo_bar.py",
			want:        false,
		},
		{
			name:        "non-match: js requires a different module",
			testContent: "import { other } from './other'\n\nit('x', () => {})\n",
			modulePath:  "src/util.ts",
			want:        false,
		},
		{
			name:        "non-match: body mentions the name but does not import it",
			testContent: "# the platform module is exercised here\ndef test_foo():\n    pass\n",
			modulePath:  "platform.py",
			want:        false,
		},
		{
			name:        "non-match: empty module path",
			testContent: "import app\n",
			modulePath:  "",
			want:        false,
		},
		{
			name:        "non-match: empty test content",
			testContent: "",
			modulePath:  "platform.py",
			want:        false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contentReferencesModule(tt.testContent, tt.modulePath); got != tt.want {
				t.Errorf("contentReferencesModule(%q, %q) = %v, want %v", tt.testContent, tt.modulePath, got, tt.want)
			}
		})
	}
}

// fakeReader backs the readFile seam with an in-memory map so the
// enforceReviewerScopeOverlap tests need no workspace. A missing path returns
// an error, mirroring os.ReadFile on an absent file.
type fakeReader map[string]string

func (f fakeReader) read(relPath string) ([]byte, error) {
	s, ok := f[relPath]
	if !ok {
		return nil, &readErr{path: relPath}
	}
	return []byte(s), nil
}

type readErr struct{ path string }

func (e *readErr) Error() string { return "fakeReader: no such file " + e.path }

// TestScopeOverlap_ContentVouchFeatureNamedTestIs the end-to-end #1610 case:
// the issue names pr_reviewer/platform.py, the diff adds a test named for the
// feature (test_platform_gh_api_forgejo.py) that does not fold to the module,
// and the test's body imports the module. Name-based folding leaves the ref
// unmatched, the content check reads the added test file and vouches, so the
// GO stands and the vouch is recorded distinctly under scopeMatchedViaContent.
func TestScopeOverlap_ContentVouchFeatureNamedTest(t *testing.T) {
	body := "Add Forgejo API support to `pr_reviewer/platform.py` and cover it with tests."
	added := []string{"tests/test_platform_gh_api_forgejo.py"}
	vouching := "from pr_reviewer.platform import ForgejoClient\n" +
		"\n\n" +
		"def test_forgejo_round_trip():\n" +
		"    client = ForgejoClient()\n" +
		"    assert client is not None\n"
	reader := fakeReader{
		"tests/test_platform_gh_api_forgejo.py": vouching,
	}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, added,
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".py"},
		TestLayout{TestRoot: "tests", SourceRoot: "pr_reviewer"},
		added, reader.read)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("a feature-named test whose body imports the named module must keep GO; got %v (reason: %v)",
			got, extra["demotionReason"])
	}
	if v, _ := extra["scopeDriftDetected"].(bool); v {
		t.Errorf("scopeDriftDetected should be false when the added test vouches by content; extra=%v", extra)
	}
	if v, _ := extra["verdictDemoted"].(bool); v {
		t.Errorf("the feature-named-test diff must not be demoted; extra=%v", extra)
	}
	matched, _ := extra["scopeMatched"].([]string)
	if len(matched) != 1 || matched[0] != "pr_reviewer/platform.py" {
		t.Errorf("scopeMatched = %v, want the module the test covers", matched)
	}
	via, ok := extra[scopeMatchedViaContentKey].([]string)
	if !ok {
		t.Fatalf("scopeMatchedViaContent must be recorded; extra=%v", extra)
	}
	if len(via) != 1 || via[0] != "pr_reviewer/platform.py" {
		t.Errorf("scopeMatchedViaContent = %v, want the content-vouched ref", via)
	}
}

// TestScopeOverlap_ContentVouchNoMatchingImportStillDemotes is the guard that
// the vouch does not overreach: a feature-named test that imports a DIFFERENT
// module must not vouch for the named module, so a GO on a diff that touches
// none of the named files is still demoted.
func TestScopeOverlap_ContentVouchNoMatchingImportStillDemotes(t *testing.T) {
	body := "Add Forgejo API support to `pr_reviewer/platform.py` and cover it with tests."
	added := []string{"tests/test_platform_gh_api_forgejo.py"}
	reader := fakeReader{
		// Named for the feature, but imports a helper, not the module under test.
		"tests/test_platform_gh_api_forgejo.py": "import unrelated_helper\n\n\ndef test_forgejo_round_trip():\n    pass\n",
	}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, added,
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".py"},
		TestLayout{TestRoot: "tests", SourceRoot: "pr_reviewer"},
		added, reader.read)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("a feature-named test that imports a different module must not vouch; got %v", got)
	}
	if v, _ := extra["scopeDriftDetected"].(bool); !v {
		t.Errorf("scopeDriftDetected must be true when no vouch fires; extra=%v", extra)
	}
	if v, _ := extra["verdictDemoted"].(bool); !v {
		t.Errorf("the non-vouching diff must be demoted; extra=%v", extra)
	}
	if _, ok := extra[scopeMatchedViaContentKey]; ok {
		t.Errorf("scopeMatchedViaContent must not be recorded when the content vouch fires; extra=%v", extra)
	}
}

// TestScopeOverlap_ContentVouchOnlyReadsAddedTestFiles proves the vouch is
// scoped to the diff-ADDED set: a test file that references the module but was
// already present on base (not in addedFiles) is not read, so it cannot vouch.
func TestScopeOverlap_ContentVouchOnlyReadsAddedTestFiles(t *testing.T) {
	body := "Add Forgejo API support to `pr_reviewer/platform.py` and cover it with tests."
	// The reader has the referencing file, but it is NOT in the added set.
	reader := fakeReader{
		"tests/test_platform_gh_api_forgejo.py": "from pr_reviewer.platform import ForgejoClient\n",
	}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body,
		[]string{"pr_reviewer/other.py"}, // a source diff that touches nothing named
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".py"},
		TestLayout{TestRoot: "tests", SourceRoot: "pr_reviewer"},
		[]string{}, // nothing added: the vouch cannot read anything
		reader.read)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("a test file outside the added set must not vouch; got %v", got)
	}
	if _, ok := extra[scopeMatchedViaContentKey]; ok {
		t.Errorf("scopeMatchedViaContent must not be recorded when no added file is read; extra=%v", extra)
	}
}

// TestScopeOverlap_ContentVouchReadErrorDegradesToName proves the vouch degrades
// to name-only when a file cannot be read: a read error must never flip a ref
// from unmatched to matched, so the GO is demoted rather than invented as
// covered.
func TestScopeOverlap_ContentVouchReadErrorDegradesToName(t *testing.T) {
	body := "Add Forgejo API support to `pr_reviewer/platform.py` and cover it with tests."
	added := []string{"tests/test_platform_gh_api_forgejo.py"}
	// The reader has no entry for the added file, so reading it errors.
	reader := fakeReader{}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, added,
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".py"},
		TestLayout{TestRoot: "tests", SourceRoot: "pr_reviewer"},
		added, reader.read)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("an unreadable test file must not vouch; got %v", got)
	}
	if _, ok := extra[scopeMatchedViaContentKey]; ok {
		t.Errorf("scopeMatchedViaContent must not be recorded on a read error; extra=%v", extra)
	}
}

// TestScopeOverlap_NameMatchNotReattributedToContent proves ordering: when a
// ref is already satisfied by name-based folding, the content vouch does not
// also claim it, so scopeMatchedViaContent stays empty.
func TestScopeOverlap_NameMatchNotReattributedToContent(t *testing.T) {
	body := "Add Forgejo API support to `pr_reviewer/platform.py` and cover it with tests."
	// The diff adds the module itself (name match) AND a test importing it.
	added := []string{
		"pr_reviewer/platform.py",
		"tests/test_platform_gh_api_forgejo.py",
	}
	reader := fakeReader{
		"tests/test_platform_gh_api_forgejo.py": "from pr_reviewer.platform import ForgejoClient\n",
	}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, added,
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".py"},
		TestLayout{TestRoot: "tests", SourceRoot: "pr_reviewer"},
		added, reader.read)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("a name match must keep GO; got %v", got)
	}
	if _, ok := extra[scopeMatchedViaContentKey]; ok {
		t.Errorf("a ref satisfied by name must not be re-attributed to the content vouch; extra=%v", extra)
	}
}

// TestScopeOverlap_NilAddedOrReaderKeepsNameBehaviour locks in that the vouch
// is opt-in: passing nil for the added set or the reader leaves the rail's
// behaviour identical to the name-only path (no content read, no vouch key).
func TestScopeOverlap_NilAddedOrReaderKeepsNameBehaviour(t *testing.T) {
	body := "Add Forgejo API support to `pr_reviewer/platform.py` and cover it with tests."
	added := []string{"tests/test_platform_gh_api_forgejo.py"}
	reader := fakeReader{
		"tests/test_platform_gh_api_forgejo.py": "from pr_reviewer.platform import ForgejoClient\n",
	}

	// nil added set: the vouch is disabled, the ref stays unmatched, GO demotes.
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, added,
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".py"}, TestLayout{},
		nil, reader.read)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("nil added set must keep name-only behaviour (demote); got %v", got)
	}
	if _, ok := extra[scopeMatchedViaContentKey]; ok {
		t.Errorf("nil added set must not record a content vouch; extra=%v", extra)
	}

	// nil reader: the vouch is disabled even though the added set is present.
	extra = map[string]any{}
	got = enforceReviewerScopeOverlap(logr.Discard(), extra, body, added,
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".py"}, TestLayout{},
		added, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("nil reader must keep name-only behaviour (demote); got %v", got)
	}
	if _, ok := extra[scopeMatchedViaContentKey]; ok {
		t.Errorf("nil reader must not record a content vouch; extra=%v", extra)
	}
}
