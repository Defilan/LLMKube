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
	"reflect"
	"strings"
	"testing"
)

// setToggle sets FOREMAN_STALENESS_CHECK for the duration of a test and
// restores the prior value on cleanup.
func setToggle(t *testing.T, val string) {
	t.Helper()
	t.Setenv(stalenessToggleEnv, val)
}

func TestCommitsReferencingIssue_TwoCommits(t *testing.T) {
	out := `a1b2c3d fix: handle case from #1550
e4f5g6h revert: #1550 workaround`
	got := commitsReferencingIssue(out, 1550)
	want := []string{"a1b2c3d", "e4f5g6h"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commitsReferencingIssue = %q, want %q", got, want)
	}
}

func TestCommitsReferencingIssue_Empty(t *testing.T) {
	got := commitsReferencingIssue("", 1550)
	if got == nil {
		t.Fatalf("commitsReferencingIssue(\"\") = nil, want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("commitsReferencingIssue(\"\") = %q, want empty", got)
	}
}

func TestCodeCitingIssue_FourteenTwo(t *testing.T) {
	out := "internal/foo/bar.go:42: // see #1550"
	got := codeCitingIssue(out, 1550)
	want := []string{"internal/foo/bar.go:42"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codeCitingIssue = %q, want %q", got, want)
	}
}

func TestCodeCitingIssue_NoLongerNumberPrefix(t *testing.T) {
	// #15500 shares the numeric prefix 1550 but is a different issue number.
	out := "pkg/foo.go:10: // see #15500 for the real fix"
	got := codeCitingIssue(out, 1550)
	if len(got) != 0 {
		t.Fatalf("codeCitingIssue matched #15500 for issue 1550 = %q, want empty", got)
	}
}

func TestCodeCitingIssue_NoShorterNumberSuffix(t *testing.T) {
	// #155 is a different issue number; it must not match issue 1550.
	out := "pkg/foo.go:10: // see #155 for context"
	got := codeCitingIssue(out, 1550)
	if len(got) != 0 {
		t.Fatalf("codeCitingIssue matched #155 for issue 1550 = %q, want empty", got)
	}
}

func TestCodeCitingIssue_DuplicatesCollapse(t *testing.T) {
	out := "internal/foo/bar.go:42: // see #1550\n" +
		"internal/foo/bar.go:42: // see #1550\n" +
		"internal/foo/bar.go:42: // again #1550"
	got := codeCitingIssue(out, 1550)
	want := []string{"internal/foo/bar.go:42"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codeCitingIssue duplicates = %q, want %q", got, want)
	}
}

func TestStalenessStableOrder(t *testing.T) {
	// Deduplication and sorted order must be stable: the same multiset of input
	// lines, in different orders, yields identical output.
	logA := "zeta111 fix: #1550\nalpha000 fix: #1550\nzeta111 fix: #1550\n"
	logB := "alpha000 fix: #1550\nzeta111 fix: #1550\nalpha000 fix: #1550\n"
	if got := commitsReferencingIssue(logA, 1550); !reflect.DeepEqual(got, []string{"alpha000", "zeta111"}) {
		t.Fatalf("commitsReferencingIssue order (A) = %q, want [alpha000 zeta111]", got)
	}
	if got := commitsReferencingIssue(logB, 1550); !reflect.DeepEqual(got, []string{"alpha000", "zeta111"}) {
		t.Fatalf("commitsReferencingIssue order (B) = %q, want [alpha000 zeta111]", got)
	}

	grepA := "zzz.go:9: #1550\naaa.go:1: #1550\nzzz.go:9: #1550\n"
	grepB := "aaa.go:1: #1550\nzzz.go:9: #1550\naaa.go:1: #1550\n"
	want := []string{"aaa.go:1", "zzz.go:9"}
	if got := codeCitingIssue(grepA, 1550); !reflect.DeepEqual(got, want) {
		t.Fatalf("codeCitingIssue order (A) = %q, want %q", got, want)
	}
	if got := codeCitingIssue(grepB, 1550); !reflect.DeepEqual(got, want) {
		t.Fatalf("codeCitingIssue order (B) = %q, want %q", got, want)
	}
}

func TestStalenessDisabledReportsNothing(t *testing.T) {
	setToggle(t, "0")
	log := "a1b2c3d fix: #1550"
	grep := "internal/foo/bar.go:42: // see #1550"
	if got := checkStaleness(1550, log, grep); got != "" {
		t.Fatalf("checkStaleness with toggle=0 = %q, want empty string", got)
	}
}

func TestCheckStalenessEnabledReportsNote(t *testing.T) {
	setToggle(t, "")
	log := "a1b2c3d fix: #1550"
	grep := "internal/foo/bar.go:42: // see #1550"
	got := checkStaleness(1550, log, grep)
	if got == "" {
		t.Fatalf("checkStaleness with evidence returned empty, want a non-empty note")
	}
}

func TestStalenessNote_NamesFindings(t *testing.T) {
	sig := stalenessSignals{
		Issue:    1550,
		Commits:  []string{"a1b2c3d"},
		CodeRefs: []string{"internal/foo/bar.go:42"},
	}
	note := stalenessNote(sig)
	for _, want := range []string{"#1550", "a1b2c3d", "internal/foo/bar.go:42"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note missing %q; got:\n%s", want, note)
		}
	}
}

func TestStalenessNote_EmptyWhenNothingFound(t *testing.T) {
	if got := stalenessNote(stalenessSignals{Issue: 1550}); got != "" {
		t.Fatalf("stalenessNote with no evidence = %q, want empty", got)
	}
}
