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
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// checkCommandStringTestDilution is a tierAdvisory gate check (#1346). It
// surfaces to the reviewer when a submission that modifies a shell/exec
// command string in production code adds only string-shape test assertions
// (ContainSubstring, strings.Contains, Equal on a string literal) with no
// behavioral test that executes the produced command or exercises the branch
// at runtime.
//
// Fires when BOTH hold:
//
//	(a) the diff modifies a non-test Go file at a site that builds a shell
//	    or exec command string (exec.Command(, "sh", "-c", or a string
//	    literal with shell metacharacters used as a command), AND
//	(b) the same package has a changed _test.go file whose ADDED assertion
//	    lines are exclusively string-shape assertions.
//
// Never fails the gate; the coder never sees it.
func checkCommandStringTestDilution(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	// Stage the working tree so a pre-commit diff includes new/untracked files.
	if _, err := run(ctx, workspace, nil, "git", "add", "-A"); err != nil {
		return false, ""
	}
	nsOut, err := run(ctx, workspace, nil, "git", "diff", "--name-status", "--cached", "HEAD")
	if err != nil {
		return false, ""
	}
	entries := parseNameStatus(nsOut)
	prodPkgs := changedProdPackages(entries)
	if len(prodPkgs) == 0 {
		return false, ""
	}

	// Get the full diff for all changed files (not just tests).
	diffOut, err := run(ctx, workspace, nil, "git", "diff", "--cached", "--unified=0",
		"--src-prefix=a/", "--dst-prefix=b/", "HEAD")
	if err != nil {
		return false, ""
	}
	byFile := parseUnifiedDiff(diffOut)

	// (a) Find production packages that changed a command-string site.
	cmdPkgs := commandStringChangedPackages(byFile, prodPkgs)
	if len(cmdPkgs) == 0 {
		return false, ""
	}

	// (b) For each such package, check if the test file's added assertions
	// are exclusively string-shape assertions.
	var findings []string
	for pkg := range cmdPkgs {
		if hasOnlyStringShapeAssertions(byFile, pkg) {
			findings = append(findings, fmt.Sprintf(
				"%s: command-string change with only string-shape test assertions (no behavioral test)", pkg))
		}
	}

	if len(findings) == 0 {
		return false, ""
	}
	detail := "production command-string change detected; added tests only assert string shape, not runtime behavior: " +
		strings.Join(findings, "; ")
	return true, truncateOutput(detail)
}

// commandStringChangedPackages returns the set of production package
// directories whose changed non-test Go files contain command-string
// modifications (exec.Command(, "sh", "-c", or shell metacharacters in a
// command string). Only packages already in prodPkgs are considered.
func commandStringChangedPackages(byFile map[string]*fileHunks, prodPkgs map[string]bool) map[string]bool {
	result := map[string]bool{}
	for file, fh := range byFile {
		dir := filepath.Dir(file)
		if !prodPkgs[dir] {
			continue
		}
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		if hasCommandStringChange(fh) {
			result[dir] = true
		}
	}
	return result
}

// hasCommandStringChange reports whether the diff hunks contain a
// modification to a shell/exec command string. It checks added and removed
// lines for exec.Command(, the pair "sh", "-c", or shell metacharacters
// in a string literal used as a command.
func hasCommandStringChange(fh *fileHunks) bool {
	for _, l := range fh.Added {
		if isCommandStringLine(l) {
			return true
		}
	}
	for _, l := range fh.Removed {
		if isCommandStringLine(l) {
			return true
		}
	}
	return false
}

// isCommandStringLine reports whether a diff content line looks like it
// builds a shell or exec command string. It checks for:
//   - exec.Command(
//   - the pair "sh", "-c" (shell invocation)
//   - a string literal containing shell metacharacters ($, |, ;, &, >, <, \`)
//     that is used as a command argument
func isCommandStringLine(s string) bool {
	if strings.Contains(s, "exec.Command(") {
		return true
	}
	// "sh", "-c" pattern: both must appear on the same line.
	if strings.Contains(s, `"sh"`) && strings.Contains(s, `"-c"`) {
		return true
	}
	// Shell metacharacters in a string literal used as a command.
	// Look for a string literal (double-quoted or backtick) containing
	// shell metacharacters that is passed to a command-related function.
	if hasShellMetacharInCommand(s) {
		return true
	}
	return false
}

// hasShellMetacharInCommand reports whether a line contains a string literal
// with shell metacharacters that is used as a command argument. This catches
// cases like `cmd := "curl -I -w '%{size_download}'"` or similar.
func hasShellMetacharInCommand(s string) bool {
	shellMeta := "$|;&><`"
	// Check for string literals containing shell metacharacters.
	// We look for a double-quoted string or backtick string that contains
	// at least one shell metacharacter, and the line references a command
	// or exec-related context.
	if !hasCommandContext(s) {
		return false
	}
	// Check for shell metacharacters inside a string literal.
	inDoubleQuote := false
	inBacktick := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && !inBacktick:
			inDoubleQuote = !inDoubleQuote
		case c == '`' && !inDoubleQuote:
			inBacktick = !inBacktick
		case (inDoubleQuote || inBacktick) && strings.ContainsRune(shellMeta, rune(c)):
			return true
		}
	}
	return false
}

// hasCommandContext reports whether a line has command-related context
// (references to Command, Run, Output, CombinedOutput, or similar).
func hasCommandContext(s string) bool {
	contexts := []string{
		"Command(", "Run(", "Output(", "CombinedOutput(",
		"exec.", "shell.", "cmd.", "command",
	}
	for _, ctx := range contexts {
		if strings.Contains(s, ctx) {
			return true
		}
	}
	return false
}

// hasOnlyStringShapeAssertions checks whether a changed _test.go file in
// the given package has added assertions that are exclusively string-shape
// assertions. Returns true if the test file exists and all its added
// assertions are string-shape only.
func hasOnlyStringShapeAssertions(byFile map[string]*fileHunks, pkg string) bool {
	for file, fh := range byFile {
		if filepath.Dir(file) != pkg {
			continue
		}
		if !strings.HasSuffix(file, "_test.go") {
			continue
		}
		// Check if this test file has any added assertions.
		if !hasAddedAssertions(fh) {
			continue
		}
		// All added assertions must be string-shape only.
		if allAddedAssertionsAreStringShape(fh) {
			return true
		}
	}
	return false
}

// hasAddedAssertions reports whether the file hunks have any added
// assertion lines.
func hasAddedAssertions(fh *fileHunks) bool {
	for _, l := range fh.Added {
		if isAssertionLine(l) {
			return true
		}
	}
	return false
}

// allAddedAssertionsAreStringShape reports whether all added assertion
// lines in the file hunks are string-shape assertions (ContainSubstring,
// strings.Contains, or Equal on a string literal). If there are no added
// assertions, returns false (no test to judge).
func allAddedAssertionsAreStringShape(fh *fileHunks) bool {
	hasAssertion := false
	for _, l := range fh.Added {
		if !isAssertionLine(l) {
			continue
		}
		hasAssertion = true
		if !isStringShapeAssertion(l) {
			return false
		}
	}
	return hasAssertion
}

// isStringShapeAssertion reports whether an assertion line is a
// string-shape assertion (as opposed to a behavioral assertion like
// checking exit codes, parsed results, or runtime behavior).
func isStringShapeAssertion(s string) bool {
	t := strings.TrimSpace(s)
	// ContainSubstring is always string-shape.
	if strings.Contains(t, "ContainSubstring(") {
		return true
	}
	// strings.Contains is always string-shape.
	if strings.Contains(t, "strings.Contains(") {
		return true
	}
	// Equal( on a string literal: the assertion compares against a
	// string literal, which is a shape check, not a behavioral check.
	// We detect this by looking for Equal( followed by a string literal
	// in the same line.
	if strings.Contains(t, "Equal(") && hasStringLiteral(t) {
		return true
	}
	return false
}

// hasStringLiteral reports whether a line contains a double-quoted string
// literal (a simple heuristic: at least one pair of double quotes).
func hasStringLiteral(s string) bool {
	count := 0
	for _, c := range s {
		if c == '"' {
			count++
		}
	}
	return count >= 2
}
