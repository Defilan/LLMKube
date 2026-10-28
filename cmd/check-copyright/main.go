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
// header is not from hack/boilerplate.go.txt with the current year. A new file
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
	"time"
)

// boilerplate is the header every new Go file starts from. Only the copyright
// line is checked; the license block below it has drifted into several
// variants over the project's life and is not this check's subject.
const boilerplate = "hack/boilerplate.go.txt"

// copyrightLine matches the template's copyright line exactly.
var copyrightLine = regexp.MustCompile(`^Copyright (\d{4})\.$`)

// packageClause matches the start of the Go source after the leading comment
// block, which bounds where a header may appear.
var packageClause = regexp.MustCompile(`^package\s`)

func main() {
	root := flag.String("root", ".", "repo root to check")
	base := flag.String("base", "upstream/main", "base ref to diff against")
	year := flag.Int("year", 0, "copyright year to require (0: the current year)")
	flag.Parse()

	if code := run(*root, *base, *year, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// run reports every file added since base whose header does not carry want.
// A want of 0 means the year at run time.
func run(root, base string, want int, stdout, stderr io.Writer) int {
	if want == 0 {
		want = time.Now().Year()
	}
	top, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check-copyright: %v\n", err)
		return 1
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
		got := copyrightYear(src)
		if got == want {
			continue
		}
		failed++
		printOffender(stderr, path, want, got)
	}

	if failed > 0 {
		_, _ = fmt.Fprintf(stderr,
			"copy the header from %s and set its year to %d:"+
				" new files take the header from the template, not from a neighboring file\n",
			boilerplate, want)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "copyright headers are the current year in %d added Go file(s)\n", checked)
	return 0
}

// printOffender writes the one-line reason a file's header is rejected.
func printOffender(w io.Writer, path string, want, got int) {
	switch got {
	case 0:
		_, _ = fmt.Fprintf(w, "FAIL: %s has no copyright header; want %q\n",
			path, fmt.Sprintf("Copyright %d.", want))
	default:
		_, _ = fmt.Fprintf(w, "FAIL: %s has %q; want %q\n",
			path, fmt.Sprintf("Copyright %d.", got), fmt.Sprintf("Copyright %d.", want))
	}
}

// copyrightYear returns the year on the file's copyright line, or 0 when the
// file has none. Only the leading comment block is read, so a copyright string
// in a test fixture or a doc comment below the package clause is not a header.
func copyrightYear(src []byte) int {
	for _, line := range strings.Split(string(src), "\n") {
		if packageClause.MatchString(line) {
			return 0
		}
		if m := copyrightLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			year, err := strconv.Atoi(m[1])
			if err != nil {
				return 0
			}
			return year
		}
	}
	return 0
}

// zeroRef reports whether base is the all-zero SHA a forge sends as
// event.before on a branch's first push. Every existing file would count as
// added there, so every existing header would fail; there is nothing to
// compare against, and the check has nothing to say.
func zeroRef(base string) bool {
	return base != "" && strings.Trim(base, "0") == ""
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
// The committed diff carries no pathspec. A pathspec makes git pair renames
// only among the listed paths, which turns a rename into a pair of additions
// and a deletion; filtering .go in Go leaves rename detection intact so a moved
// file is not reported as added. A staged rename is named by its old path,
// which exists nowhere and is dropped below.
func addedGoFiles(top, base string) ([]string, error) {
	if zeroRef(base) {
		return nil, nil
	}
	mb, err := git(top, "merge-base", base, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("merge-base with %s: %w", base, err)
	}
	specs := [][]string{
		{"diff", "--name-only", "--diff-filter=A", mb, "HEAD"},
		{"diff", "--cached", "--name-only", "--diff-filter=A"},
		{"ls-files", "--others", "--exclude-standard"},
	}
	seen := map[string]bool{}
	files := make([]string, 0, 8)
	for _, spec := range specs {
		out, err := git(top, spec...)
		if err != nil {
			return nil, err
		}
		for _, f := range strings.Split(out, "\n") {
			if f == "" || seen[f] || !strings.HasSuffix(f, ".go") {
				continue
			}
			seen[f] = true
			files = append(files, f)
		}
	}
	return files, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
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
