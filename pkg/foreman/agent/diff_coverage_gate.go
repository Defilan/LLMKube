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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Diff-coverage gate: a branch the diff ADDED that no test ever executes.
//
// This exists because the existing gates check that a FUNCTION is tested, not
// that its BRANCHES are, and three separate coder runs shipped defects through
// that gap:
//
//   - a nil guard whose `return` no test ever took, so a transport failure
//     silently discarded the coder's work
//   - a `ParseQuantity` error path no test ever entered, so a malformed value
//     produced a container with neither a memory request nor a limit, which is
//     the exact unbounded condition the change was fixing
//
// mutationGate Layer 1 passed both: it requires a net-new function to be
// REFERENCED by name in a changed test, and it exempts body-modified functions
// entirely. A function can be referenced by a test, and body-modified, while
// its new error path is never entered.
//
// The check is deliberately narrow. It reports only lines the diff added that
// fall inside a coverage block Go recorded as executed ZERO times. It says
// nothing about assertion quality, which is mutationGate's job.

// coverageBlock is one line of a `go test -coverprofile` file. The format is
//
//	<import path>/<file>.go:<sLine>.<sCol>,<eLine>.<eCol> <numStmt> <count>
//
// count is the number of times the block executed, so count == 0 is the signal
// this gate is built on.
type coverageBlock struct {
	file      string
	startLine int
	endLine   int
	count     int
}

// parseCoverProfile parses coverprofile text into blocks, skipping the leading
// "mode:" line and anything malformed. Parsing is lenient on purpose: a gate
// that hard-errors on one odd line would block a coder run over a tooling
// quirk, and a missed block only costs a missed finding.
func parseCoverProfile(out string) []coverageBlock {
	var blocks []coverageBlock
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// "<file>:<s>.<sc>,<e>.<ec> <n> <count>"
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		colon := strings.LastIndex(fields[0], ":")
		if colon < 0 {
			continue
		}
		file, span := fields[0][:colon], fields[0][colon+1:]
		comma := strings.Index(span, ",")
		if comma < 0 {
			continue
		}
		start, ok1 := lineOfSpanPart(span[:comma])
		end, ok2 := lineOfSpanPart(span[comma+1:])
		if !ok1 || !ok2 {
			continue
		}
		blocks = append(blocks, coverageBlock{file: file, startLine: start, endLine: end, count: count})
	}
	return blocks
}

// lineOfSpanPart pulls the line number out of a "<line>.<col>" half of a span.
func lineOfSpanPart(s string) (int, bool) {
	dot := strings.Index(s, ".")
	if dot < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:dot])
	if err != nil {
		return 0, false
	}
	return n, true
}

// uncoveredAddedLines returns, per repo-relative file, the added lines that sit
// inside a coverage block that executed zero times.
//
// Two rules keep this from being noise:
//
//   - An added line inside NO block is ignored. Comments, blank lines, imports,
//     declarations and func signatures are not statements and appear in no
//     block. Flagging them would fire on every diff and the gate would be
//     switched off within a day.
//   - An added line inside ANY executed block is ignored, even if it also sits
//     inside an unexecuted one. Go emits nested and overlapping blocks; treating
//     a line as covered when any enclosing block ran avoids reporting a line a
//     test demonstrably reaches.
//
// Coverprofile paths are import paths and diff paths are repo-relative, so
// matching is by suffix.
func uncoveredAddedLines(blocks []coverageBlock, added map[string]map[int]bool) map[string][]int {
	out := map[string][]int{}
	for file, lines := range added {
		relevant := blocksForFile(blocks, file)
		if len(relevant) == 0 {
			// No coverage data for this file at all: the package may not have
			// been tested. Report nothing rather than flagging every added
			// line, which would be indistinguishable from a genuine finding.
			continue
		}
		for line := range lines {
			covered, inAnyBlock := false, false
			for _, b := range relevant {
				if line < b.startLine || line > b.endLine {
					continue
				}
				inAnyBlock = true
				if b.count > 0 {
					covered = true
					break
				}
			}
			if inAnyBlock && !covered {
				out[file] = append(out[file], line)
			}
		}
		if len(out[file]) == 0 {
			delete(out, file)
		}
	}
	return out
}

// blocksForFile selects the coverage blocks belonging to a repo-relative path.
// Suffix match with a separator guard so "controller/thing.go" cannot match
// "othercontroller/thing.go".
func blocksForFile(blocks []coverageBlock, relPath string) []coverageBlock {
	var out []coverageBlock
	for _, b := range blocks {
		if b.file == relPath || strings.HasSuffix(b.file, "/"+relPath) {
			out = append(out, b)
		}
	}
	return out
}

// checkDiffCoverage runs the package tests with coverage and reports lines the
// diff added that no test executes.
//
// ADVISORY, not blocking. A brand-new gate that fails a run is a good way to
// stall the whole fleet on its first false positive; this surfaces to the
// reviewer and the audit record instead. Promote it to tierBlock once it has
// run quietly against real diffs for a while.
//
// Cost is one `go test -coverprofile` per changed package. The mutation gate
// already re-runs package tests, so this is not a new class of expense.
func checkDiffCoverage(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	files := changedNonTestGoFiles(ctx, workspace, run)
	if len(files) == 0 {
		return false, ""
	}

	// One coverage run per package, not per file: a package's tests cover all
	// of its files at once, and re-running per file would multiply the cost.
	pkgs := map[string][]string{}
	for _, f := range files {
		dir := filepath.Dir(f)
		if dir == "." {
			continue
		}
		pkgs[dir] = append(pkgs[dir], f)
	}

	var findings []string
	// notEvaluated collects packages this check could not measure. Reporting
	// them is the whole point of #1733: a silent skip renders identically to a
	// clean result, so "we could not look" becomes indistinguishable from "we
	// looked and found nothing" -- the exact failure class this gate exists to
	// catch, occurring inside the gate.
	var notEvaluated []string
	for dir, dirFiles := range pkgs {
		// Added lines FIRST, before any coverage run. A package whose diff adds
		// no statements cannot produce a finding, so testing it is pure cost.
		added := map[string]map[int]bool{}
		for _, f := range dirFiles {
			if lines := changedNewLines(ctx, workspace, f, run); len(lines) > 0 {
				added[f] = lines
			}
		}
		if len(added) == 0 {
			continue
		}

		pkgPath := "./" + dir + "/"

		// Envtest-backed packages are classified, not discovered by failing.
		// envtestPackagePrefixes exists because the coder workspace has no
		// KUBEBUILDER_ASSETS, and the fast unit-test tier already skips these
		// for the same reason. Running them here would fail in BeforeSuite
		// after burning the timeout.
		//
		// The post-push gate Job DOES run these with envtest assets, but it
		// runs `make test`, not diff coverage. So for these packages this
		// check happens nowhere, and saying so is the honest report.
		if isEnvtestPackage(pkgPath) {
			notEvaluated = append(notEvaluated,
				dir+" (envtest package; not measurable in the coder workspace)")
			continue
		}

		profile, err := os.CreateTemp("", "diffcov-*.out")
		if err != nil {
			notEvaluated = append(notEvaluated, dir+" (could not create a coverage profile)")
			continue
		}
		profilePath := profile.Name()
		_ = profile.Close()

		// A failing or absent test suite is NOT this gate's finding: the coder
		// gate already runs build and test, and reporting the failure here as
		// well would double-report one problem under two names. What IS this
		// gate's business is that coverage went unmeasured, so the package is
		// recorded rather than dropped.
		_, terr := run(ctx, workspace, nil, "go", "test", "-count=1", "-timeout=300s",
			"-coverprofile="+profilePath, pkgPath)
		if terr != nil {
			_ = os.Remove(profilePath)
			notEvaluated = append(notEvaluated, dir+" (its tests did not run here)")
			continue
		}
		raw, rerr := os.ReadFile(profilePath) //nolint:gosec // G304: path from os.CreateTemp
		_ = os.Remove(profilePath)
		if rerr != nil {
			notEvaluated = append(notEvaluated, dir+" (coverage profile unreadable)")
			continue
		}

		for file, lines := range uncoveredAddedLines(parseCoverProfile(string(raw)), added) {
			sort.Ints(lines)
			findings = append(findings, fmt.Sprintf("%s: added lines never executed by any test: %s",
				file, formatLineList(lines)))
		}
	}
	if len(findings) == 0 && len(notEvaluated) == 0 {
		return false, ""
	}
	sort.Strings(findings)
	sort.Strings(notEvaluated)

	var b strings.Builder
	if len(findings) > 0 {
		b.WriteString("Added code that no test reaches:\n  " + strings.Join(findings, "\n  "))
		b.WriteString("\n\nThese lines were added by this change and ran zero times. An error path " +
			"or guard that no test enters is not constrained by the suite: it can be " +
			"wrong in either direction and every gate still passes.")
	}
	if len(notEvaluated) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Coverage NOT evaluated for:\n  " + strings.Join(notEvaluated, "\n  "))
		b.WriteString("\n\nThis is not a finding about the code, it is the limit of this check. " +
			"These packages were added to by this change and their diff coverage was " +
			"never measured, so a silent pass here says nothing about them either way.")
	}
	return true, b.String()
}

// formatLineList renders line numbers compactly, collapsing runs into ranges so
// a 40-line uncovered block reads as "120-159" rather than forty numbers.
func formatLineList(lines []int) string {
	if len(lines) == 0 {
		return ""
	}
	var parts []string
	start, prev := lines[0], lines[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.Itoa(start))
			return
		}
		parts = append(parts, strconv.Itoa(start)+"-"+strconv.Itoa(prev))
	}
	for _, l := range lines[1:] {
		if l == prev+1 {
			prev = l
			continue
		}
		flush()
		start, prev = l, l
	}
	flush()
	return strings.Join(parts, ", ")
}
