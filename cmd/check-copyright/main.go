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

// Command check-copyright fails when a branch adds a Go file whose copyright
// header does not carry the year hack/boilerplate.go.txt does. A new file
// copies its header from a neighboring file rather than from the template, so
// a stale year spreads unless a check stops it (#2298).
//
// Only files the branch adds are inspected. The files already in the tree keep
// the year they were written with, and their headers must not fail the check.
//
// Usage:
//
//	go run ./cmd/check-copyright -base upstream/main
//
// It exits 1 and prints one line per file whose header is stale, missing, or
// not the form the template uses.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// boilerplate is the header every new Go file starts from. Only the copyright
// line is checked; the license block below it has drifted into several
// variants over the project's life and is not this check's subject.
const boilerplate = "hack/boilerplate.go.txt"

// copyrightLine matches the template's copyright line exactly.
var copyrightLine = regexp.MustCompile(`^Copyright (\d{4})\.$`)

// anyCopyright matches a copyright line in any other form, so a header that is
// present but not the template's (a year range, a slash-style comment, a
// missing period) is reported as what it is rather than as missing.
var anyCopyright = regexp.MustCompile(`Copyright\s+\S`)

// packageClause matches the start of the Go source after the leading comment
// block, which bounds where a header may appear.
var packageClause = regexp.MustCompile(`^package\s`)

func main() {
	root := flag.String("root", ".", "repo root to check")
	base := flag.String("base", "upstream/main", "base ref to diff against")
	year := flag.Int("year", 0, "copyright year to require (0: the year in "+boilerplate+")")
	flag.Parse()

	if code := run(*root, *base, *year, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// run reports every file added since base whose header does not carry want.
// A want of 0 means the year hack/boilerplate.go.txt carries, which is the
// single source of truth: AGENTS.md tells contributors to copy that header, so
// the check must accept exactly what the template produces. Reading the clock
// instead would fail every new file from 1 January until someone bumped the
// template, and controller-gen output with it.
func run(root, base string, want int, stdout, stderr io.Writer) int {
	top, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check-copyright: %v\n", err)
		return 1
	}
	if want == 0 {
		want, err = templateYear(top)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "check-copyright: %v\n", err)
			return 1
		}
	}
	if _, err := git(top, "rev-parse", "--verify", base); err != nil {
		_, _ = fmt.Fprintf(stderr,
			"check-copyright: base ref %q does not resolve: %v\n", base, err)
		_, _ = fmt.Fprintf(stderr,
			"set BASE_REF to the branch this work targets (a fork clone has no upstream/main)\n")
		return 1
	}
	files, err := addedGoFiles(top, base)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check-copyright: %v\n", err)
		return 1
	}

	failed := 0
	checked := 0
	for _, path := range files {
		if exempt(path) {
			continue
		}
		checked++
		src, err := os.ReadFile(filepath.Join(top, path))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "check-copyright: %v\n", err)
			return 1
		}
		got, any := inspectHeader(src)
		if got == want {
			continue
		}
		failed++
		printOffender(stderr, path, want, got, any)
	}

	if failed > 0 {
		_, _ = fmt.Fprintf(stderr,
			"copy the header from %s and set its year to %d:"+
				" new files take the header from the template, not from a neighboring file\n",
			boilerplate, want)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "copyright headers carry Copyright %d. in %d added Go file(s)\n", want, checked)
	return 0
}

// printOffender writes the one-line reason a file's header is rejected.
func printOffender(w io.Writer, path string, want, got int, any bool) {
	wantLine := fmt.Sprintf("Copyright %d.", want)
	switch {
	case got != 0:
		_, _ = fmt.Fprintf(w, "FAIL: %s has %q; want %q\n",
			path, fmt.Sprintf("Copyright %d.", got), wantLine)
	case any:
		_, _ = fmt.Fprintf(w, "FAIL: %s has a copyright line that is not the template's form; want %q\n",
			path, wantLine)
	default:
		_, _ = fmt.Fprintf(w, "FAIL: %s has no copyright header; want %q\n", path, wantLine)
	}
}

// inspectHeader reads the leading comment block and reports the year on the
// template's copyright line, plus whether any copyright line at all was found.
func inspectHeader(src []byte) (year int, any bool) {
	for _, line := range strings.Split(string(src), "\n") {
		if packageClause.MatchString(line) {
			return 0, any
		}
		trimmed := strings.TrimSpace(line)
		if m := copyrightLine.FindStringSubmatch(trimmed); m != nil {
			y, err := strconv.Atoi(m[1])
			if err != nil {
				return 0, true
			}
			return y, true
		}
		if anyCopyright.MatchString(trimmed) {
			any = true
		}
	}
	return 0, any
}

// zeroRef reports whether base is the all-zero SHA a forge sends as
// event.before on a branch's first push. Every existing file would count as
// added there, so every existing header would fail; there is nothing to
// compare against, and the check has nothing to say.
func zeroRef(base string) bool {
	return base != "" && strings.Trim(base, "0") == ""
}

// templateYear returns the year on the copyright line of boilerplate. A
// template whose line does not parse is an error, not a zero year: silently
// accepting every header would turn the check off.
func templateYear(top string) (int, error) {
	src, err := os.ReadFile(filepath.Join(top, boilerplate))
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", boilerplate, err)
	}
	year, ok := inspectHeader(src)
	if !ok || year == 0 {
		return 0, fmt.Errorf("%s has no parsable `Copyright <year>.` line", boilerplate)
	}
	return year, nil
}

// exempt reports whether a file is excluded from the check. A testdata file is
// a fixture, and a fixture may legitimately carry no header at all.
func exempt(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == "testdata" {
			return true
		}
	}
	return false
}

// addedGoFiles lists the .go files the branch adds, relative to the module
// root: committed since the merge base, staged, and untracked. The gate runs
// on a worktree before the commit exists, so the last two belong here.
//
// --diff-filter=A is what keeps a moved file out: git reports a rename as a
// single renames entry, never as an addition, whether or not a pathspec
// narrows the diff, so `git mv` does not fail the check. .go is filtered in Go
// rather than by a pathspec so the filter reads the same for all three specs.
func addedGoFiles(top, base string) ([]string, error) {
	if zeroRef(base) {
		return nil, nil
	}
	// The diff base is the merge base, never the base tip. A tree diff against
	// a tip also lists every file the base deleted after the fork, which this
	// branch never touched; there is no fallback, because every narrowed
	// version of one answered the wrong question instead.
	mb, err := git(top, "merge-base", base, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("merge-base with %s: %w", base, err)
	}
	if mb == "" {
		return nil, fmt.Errorf("merge-base with %s: no common ancestor (shallow or unrelated history)", base)
	}
	out, err := git(top, "diff", "--name-only", "--diff-filter=A", mb, "HEAD")
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	files := make([]string, 0, 8)
	add := func(f string) {
		if f == "" || seen[f] || !strings.HasSuffix(f, ".go") {
			return
		}
		seen[f] = true
		files = append(files, f)
	}
	for _, f := range strings.Split(out, "\n") {
		add(f)
	}
	// Staged and untracked files are additions by definition, whatever the
	// base says, so they bypass the merge-base diff.
	for _, spec := range [][]string{
		{"diff", "--cached", "--name-only", "--diff-filter=A"},
		{"ls-files", "--others", "--exclude-standard"},
	} {
		out, err := git(top, spec...)
		if err != nil {
			return nil, err
		}
		for _, f := range strings.Split(out, "\n") {
			add(f)
		}
	}
	return files, nil
}

// gitConfig pins the settings this check's decisions rest on, so they hold
// regardless of the caller's git config: rename detection is what keeps a
// moved file out of the added set, and unquoted paths are what makes the
// listed path usable by os.ReadFile.
var gitConfig = []string{
	"-c", "diff.renames=true",
	"-c", "core.quotePath=false",
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string{}, gitConfig...), args...)...)
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err,
			strings.TrimSpace(errOut.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
