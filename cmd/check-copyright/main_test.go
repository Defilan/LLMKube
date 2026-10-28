/*
Copyright 2026.

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

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// header renders a file whose leading comment is the given copyright line
// followed by the template's license block.
func header(line string) string {
	if line == "" {
		return "package x\n"
	}
	return "/*\n" + line + "\n\nLicensed under the Apache License, Version 2.0.\n*/\n\npackage x\n"
}

// newRepo builds a git repo with one committed base file and returns its path
// plus the base commit. The base is recorded before any case adds a file, so
// diffing against it is what a branch's merge base is in the real repository.
func newRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "config", "user.email", "check-copyright@example.com")
	gitIn(t, dir, "config", "user.name", "check-copyright")
	writeFile(t, dir, "base.go", header("Copyright 2025."))
	commit(t, dir, "base")
	return dir, strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD"))
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func commit(t *testing.T, dir, msg string) {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "--no-verify", "-m", msg)
}

// gitIn runs git in dir with the user's global config and hooks disabled, so a
// developer's dotfiles hooks cannot rewrite the fixture repository mid-test.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// runCheck drives run against dir, requiring 2026 so the cases do not drift
// with the calendar.
func runCheck(t *testing.T, dir, base string) (code int, stderr string) {
	t.Helper()
	var stdout, errOut bytes.Buffer
	code = run(dir, base, 2026, &stdout, &errOut)
	return code, errOut.String()
}

func TestRunCurrentYearPasses(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/new.go", header("Copyright 2026."))
	commit(t, dir, "add new")

	code, stderr := runCheck(t, dir, base)
	if code != 0 {
		t.Fatalf("want exit 0, got %d: %s", code, stderr)
	}
}

func TestRunStaleYearFails(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/cli/new_thing.go", header("Copyright 2025."))
	commit(t, dir, "add new")

	code, stderr := runCheck(t, dir, base)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "pkg/cli/new_thing.go") {
		t.Errorf("output must name the file:\n%s", stderr)
	}
	if !strings.Contains(stderr, `"Copyright 2026."`) {
		t.Errorf("output must name the year it wants:\n%s", stderr)
	}
}

func TestRunMissingHeaderFails(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/noheader.go", header(""))
	commit(t, dir, "add new")

	code, stderr := runCheck(t, dir, base)
	if code != 1 {
		t.Fatalf("want exit 1 for a file with no header, got %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no copyright header") {
		t.Errorf("output must say the header is missing:\n%s", stderr)
	}
}

func TestRunRangeIsRejected(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/ranged.go", header("Copyright 2025-2026."))
	commit(t, dir, "add new")

	code, stderr := runCheck(t, dir, base)
	if code != 1 {
		t.Fatalf("a year range is not the template's form; want exit 1, got %d: %s", code, stderr)
	}
}

func TestRunFutureYearFails(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/future.go", header("Copyright 2027."))
	commit(t, dir, "add new")

	code, stderr := runCheck(t, dir, base)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %s", code, stderr)
	}
}

func TestRunTestdataExempt(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/fixture/testdata/plain.go", "package plain\n")
	commit(t, dir, "add fixture")

	code, stderr := runCheck(t, dir, base)
	if code != 0 {
		t.Fatalf("a testdata fixture is exempt; want exit 0, got %d: %s", code, stderr)
	}
}

func TestRunNonGoIgnored(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "charts/README.md", "Copyright 2025.\n")
	commit(t, dir, "add doc")

	code, stderr := runCheck(t, dir, base)
	if code != 0 {
		t.Fatalf("only Go files are checked; want exit 0, got %d: %s", code, stderr)
	}
}

func TestRunRenameIgnored(t *testing.T) {
	dir, base := newRepo(t)
	gitIn(t, dir, "mv", "base.go", "moved.go")
	commit(t, dir, "rename")

	code, stderr := runCheck(t, dir, base)
	if code != 0 {
		t.Fatalf("a rename is not an addition; want exit 0, got %d: %s", code, stderr)
	}
}

func TestRunStagedRenameIgnored(t *testing.T) {
	dir, base := newRepo(t)
	gitIn(t, dir, "mv", "base.go", "moved.go")

	code, stderr := runCheck(t, dir, base)
	if code != 0 {
		t.Fatalf("a staged rename is not an addition; want exit 0, got %d: %s", code, stderr)
	}
}

func TestRunModifiedExistingIgnored(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "base.go", header("Copyright 2025.")+"// touched\n")
	commit(t, dir, "modify")

	code, stderr := runCheck(t, dir, base)
	if code != 0 {
		t.Fatalf("an existing file keeps its year; want exit 0, got %d: %s", code, stderr)
	}
}

func TestRunExistingStaleHeadersIgnored(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/current.go", header("Copyright 2026."))
	commit(t, dir, "add new")

	code, stderr := runCheck(t, dir, base)
	if code != 0 {
		t.Fatalf("base.go carries a 2025 header but is not added; want exit 0, got %d: %s", code, stderr)
	}
}

func TestRunUntrackedChecked(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/untracked.go", header("Copyright 2025."))

	code, stderr := runCheck(t, dir, base)
	if code != 1 {
		t.Fatalf("an untracked .go file is added; want exit 1, got %d: %s", code, stderr)
	}
}

func TestRunStagedChecked(t *testing.T) {
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/staged.go", header("Copyright 2025."))
	gitIn(t, dir, "add", "pkg/staged.go")

	code, stderr := runCheck(t, dir, base)
	if code != 1 {
		t.Fatalf("a staged .go file is added; want exit 1, got %d: %s", code, stderr)
	}
}

func TestRunUnresolvableBaseFails(t *testing.T) {
	dir, _ := newRepo(t)

	var stdout, errOut bytes.Buffer
	code := run(dir, "no-such-ref", 2026, &stdout, &errOut)
	if code != 1 {
		t.Fatalf("an unresolvable base must fail, got %d", code)
	}
	if !strings.Contains(errOut.String(), "BASE_REF") {
		t.Errorf("the message must name BASE_REF:\n%s", errOut.String())
	}
}

func TestRunZeroSHAIsNoop(t *testing.T) {
	dir, _ := newRepo(t)
	writeFile(t, dir, "pkg/first.go", header("Copyright 2025."))
	commit(t, dir, "first push")

	// A branch's first push arrives with event.before set to the all-zero SHA;
	// every existing file would otherwise count as added.
	var stdout, errOut bytes.Buffer
	code := run(dir, strings.Repeat("0", 40), 2026, &stdout, &errOut)
	if code != 0 {
		t.Fatalf("an all-zero base has nothing to compare against; want exit 0, got %d: %s", code, errOut.String())
	}
}

func TestRunZeroYearUsesClock(t *testing.T) {
	src := []byte(header("Copyright 2026."))
	if got := copyrightYear(src); got != 2026 {
		t.Errorf("copyrightYear = %d, want 2026", got)
	}
	// run's want==0 path resolves the current year; drive it against a repo
	// whose only added file carries this machine's year.
	dir, base := newRepo(t)
	writeFile(t, dir, "pkg/current.go", header("Copyright 2026."))
	commit(t, dir, "add new")
	var stdout, errOut bytes.Buffer
	if code := run(dir, base, 0, &stdout, &errOut); code != 0 {
		t.Fatalf("want exit 0 with the clock year, got %d: %s", code, errOut.String())
	}
}

func TestCopyrightYearIgnoresBody(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			name: "header after build tag",
			src:  "//go:build darwin\n\n/*\nCopyright 2025.\n\nApache.\n*/\n\npackage x\n",
			want: 2025,
		},
		{
			name: "second copyright below package is a test fixture, not a header",
			src:  "//go:build darwin\n\n/*\nCopyright 2026.\n*/\n\npackage x\n\nconst lic = \"Copyright 2025.\"\n",
			want: 2026,
		},
		{
			name: "no header at all",
			src:  "package x\n\n// Copyright 2025.\n",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := copyrightYear([]byte(tc.src)); got != tc.want {
				t.Errorf("copyrightYear = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExempt(t *testing.T) {
	cases := map[string]bool{
		"pkg/a.go":                         false,
		"pkg/testdata/a.go":                true,
		"pkg/agent/testdata/mutation/a.go": true,
		"pkg/testing/a.go":                 false,
	}
	for path, want := range cases {
		if got := exempt(path); got != want {
			t.Errorf("exempt(%q) = %v, want %v", path, got, want)
		}
	}
}
