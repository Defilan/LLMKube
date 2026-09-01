package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A Go coverprofile block is `importpath/file.go:sLine.sCol,eLine.eCol nStmt count`.
// The paths are import paths; the diff gives repo-relative paths, so matching is
// by suffix.
const sampleProfile = `mode: set
github.com/defilantech/llmkube/internal/controller/deployment_builder.go:717.32,720.3 2 1
github.com/defilantech/llmkube/internal/controller/deployment_builder.go:721.4,723.5 1 0
github.com/defilantech/llmkube/internal/controller/deployment_builder.go:730.2,732.16 3 5
github.com/defilantech/llmkube/pkg/other/thing.go:10.1,12.2 1 0
`

func TestParseCoverProfile(t *testing.T) {
	blocks := parseCoverProfile(sampleProfile)
	if len(blocks) != 4 {
		t.Fatalf("blocks: want 4 got %d", len(blocks))
	}
	b := blocks[1]
	if b.startLine != 721 || b.endLine != 723 || b.count != 0 {
		t.Errorf("block[1] = %+v, want start 721 end 723 count 0", b)
	}
	if b.file != "github.com/defilantech/llmkube/internal/controller/deployment_builder.go" {
		t.Errorf("block[1].file = %q", b.file)
	}
}

// The whole point of the gate: a line the diff ADDED that no test executes.
func TestUncoveredAddedLines_FlagsAnUnexecutedAddedBranch(t *testing.T) {
	added := map[string]map[int]bool{
		"internal/controller/deployment_builder.go": {722: true},
	}
	got := uncoveredAddedLines(parseCoverProfile(sampleProfile), added)
	if len(got["internal/controller/deployment_builder.go"]) != 1 ||
		got["internal/controller/deployment_builder.go"][0] != 722 {
		t.Errorf("want line 722 flagged, got %+v", got)
	}
}

// An added line inside an EXECUTED block is fine.
func TestUncoveredAddedLines_IgnoresCoveredLines(t *testing.T) {
	added := map[string]map[int]bool{
		"internal/controller/deployment_builder.go": {718: true, 731: true},
	}
	if got := uncoveredAddedLines(parseCoverProfile(sampleProfile), added); len(got) != 0 {
		t.Errorf("covered lines must not be flagged, got %+v", got)
	}
}

// Comments, blank lines, imports and func signatures are not statements and
// appear in NO block. Flagging them would make the gate fire on every diff and
// it would be turned off within a day.
func TestUncoveredAddedLines_IgnoresNonStatementLines(t *testing.T) {
	added := map[string]map[int]bool{
		"internal/controller/deployment_builder.go": {700: true, 999: true},
	}
	if got := uncoveredAddedLines(parseCoverProfile(sampleProfile), added); len(got) != 0 {
		t.Errorf("non-statement lines must not be flagged, got %+v", got)
	}
}

// Blocks belonging to ANOTHER file must not be attributed to this one.
//
// The obvious version of this test ("pkg/other/thing.go is absent from the
// output") is vacuous: output keys come from `added`, so an untouched file can
// never appear no matter how broken the matcher is. It passed with the file
// matching replaced by `if true`.
//
// This version has teeth. Line 11 sits inside thing.go's zero-count block
// (10-12) and inside NO block of deployment_builder.go. Correct behaviour
// leaves it unflagged, because a line in no block of its own file is a comment
// or declaration. A matcher that ignores the filename attributes thing.go's
// uncovered block to deployment_builder.go and reports a false positive.
func TestUncoveredAddedLines_DoesNotAttributeOtherFilesBlocks(t *testing.T) {
	added := map[string]map[int]bool{
		"internal/controller/deployment_builder.go": {11: true},
	}
	got := uncoveredAddedLines(parseCoverProfile(sampleProfile), added)
	if len(got) != 0 {
		t.Errorf("line 11 is only in ANOTHER file's uncovered block; "+
			"flagging it means blocks are matched without regard to filename: %+v", got)
	}
}

// The real regression this gate exists for, using the shape of the #1724
// pre-revision defect: a ParseQuantity error path that no test entered, which
// silently produced a container with neither a memory request nor a limit.
//
// Go records the `err == nil` body as executed and the surrounding block as
// partially unexecuted; the added line inside the zero-count block is the
// finding.
func TestUncoveredAddedLines_CatchesTheParseErrorPathFrom1724(t *testing.T) {
	profile := `mode: set
github.com/defilantech/llmkube/internal/controller/deployment_builder.go:720.36,722.4 1 1
github.com/defilantech/llmkube/internal/controller/deployment_builder.go:723.4,725.5 1 0
github.com/defilantech/llmkube/internal/controller/deployment_builder.go:727.19,730.3 2 1
`
	// The coder added all of 720-730; only 723-725 (the error path) never ran.
	added := map[string]map[int]bool{
		"internal/controller/deployment_builder.go": {
			720: true, 721: true, 723: true, 724: true, 727: true, 728: true,
		},
	}
	got := uncoveredAddedLines(parseCoverProfile(profile), added)
	lines := got["internal/controller/deployment_builder.go"]
	if len(lines) != 2 {
		t.Fatalf("want the two error-path lines flagged, got %v", lines)
	}
	for _, want := range []int{723, 724} {
		found := false
		for _, l := range lines {
			if l == want {
				found = true
			}
		}
		if !found {
			t.Errorf("line %d (unexecuted error path) not flagged; got %v", want, lines)
		}
	}
}

// Ranges keep a large uncovered block readable in the gate output.
func TestFormatLineList_CollapsesRuns(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{
		{[]int{5}, "5"},
		{[]int{5, 6, 7}, "5-7"},
		{[]int{5, 7}, "5, 7"},
		{[]int{1, 2, 3, 9, 20, 21}, "1-3, 9, 20-21"},
	} {
		if got := formatLineList(tc.in); got != tc.want {
			t.Errorf("formatLineList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// coverageFakeRunner answers the three command shapes checkDiffCoverage drives:
// `git status -z` for the changed-file list, `git diff -U0 HEAD -- <file>` for a
// file's added lines, and `go test` for the coverage run. goTestErr keys are
// package dirs whose test run should fail. Every `go test` invocation is
// recorded in *ran so a test can assert a package was NOT tested.
func coverageFakeRunner(
	status string, diffs map[string]string, goTestErr map[string]bool, ran *[]string,
) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 2 && args[0] == "status" {
			return status, nil
		}
		if name == "git" && len(args) >= 1 && args[0] == "diff" {
			file := args[len(args)-1]
			return diffs[file], nil
		}
		if name == "go" && len(args) >= 1 && args[0] == "test" {
			pkg := args[len(args)-1]
			*ran = append(*ran, pkg)
			if goTestErr[pkg] {
				return "BeforeSuite failed: no such file or directory", errFakeGoTest
			}
			return "ok", nil
		}
		return "", nil
	}
}

var errFakeGoTest = fmt.Errorf("exit status 1")

// addedLineDiff builds the minimal `git diff -U0` shape parseAddedLines reads.
func addedLineDiff(start int, n int) string {
	d := fmt.Sprintf("@@ -%d,0 +%d,%d @@\n", start-1, start, n)
	for i := 0; i < n; i++ {
		d += "+\tsomeStatement()\n"
	}
	return d
}

// TestCheckDiffCoverage_ReportsEnvtestPackagesAsNotEvaluated is the #1733
// regression. An envtest-backed package cannot be measured in the coder
// workspace (no KUBEBUILDER_ASSETS), and the gate must SAY so. Emitting nothing
// makes "could not look" render exactly like "looked and found nothing", which
// is the failure class this gate exists to catch.
func TestCheckDiffCoverage_ReportsEnvtestPackagesAsNotEvaluated(t *testing.T) {
	file := "internal/foreman/controller/agentictask_controller.go"
	var ran []string
	run := coverageFakeRunner(
		file,
		map[string]string{file: addedLineDiff(591, 3)},
		nil, &ran,
	)

	found, msg := checkDiffCoverage(context.Background(), t.TempDir(), run)
	if !found {
		t.Fatalf("envtest package went unreported; got found=false msg=%q", msg)
	}
	if !strings.Contains(msg, "internal/foreman/controller") {
		t.Errorf("advisory does not name the unevaluated package: %q", msg)
	}
	// It must not even attempt the run: the fast tier skips these because
	// running them in the coder workspace hangs or fails in BeforeSuite.
	for _, p := range ran {
		if strings.Contains(p, "internal/foreman/controller") {
			t.Errorf("ran go test on an envtest package (%q); it should be "+
				"classified via isEnvtestPackage, not discovered by failing", p)
		}
	}
}

// TestCheckDiffCoverage_ReportsAFailedTestRunAsNotEvaluated covers the other
// way the check can be blind: a non-envtest package whose tests do not run.
func TestCheckDiffCoverage_ReportsAFailedTestRunAsNotEvaluated(t *testing.T) {
	file := "pkg/foreman/agent/thing.go"
	var ran []string
	run := coverageFakeRunner(
		file,
		map[string]string{file: addedLineDiff(10, 2)},
		map[string]bool{"./pkg/foreman/agent/": true}, &ran,
	)

	found, msg := checkDiffCoverage(context.Background(), t.TempDir(), run)
	if !found {
		t.Fatalf("a package whose tests did not run went unreported: msg=%q", msg)
	}
	if !strings.Contains(msg, "pkg/foreman/agent") {
		t.Errorf("advisory does not name the unevaluated package: %q", msg)
	}
}

// TestCheckDiffCoverage_NoAddedLinesRunsNoTests asserts the ordering change:
// added lines are computed BEFORE the coverage run, so a package whose diff
// cannot produce a finding costs no `go test`.
func TestCheckDiffCoverage_NoAddedLinesRunsNoTests(t *testing.T) {
	file := "pkg/foreman/agent/thing.go"
	var ran []string
	run := coverageFakeRunner(file, map[string]string{file: ""}, nil, &ran)

	found, msg := checkDiffCoverage(context.Background(), t.TempDir(), run)
	if found {
		t.Errorf("reported something for a diff with no added lines: %q", msg)
	}
	if len(ran) != 0 {
		t.Errorf("ran %v; a package with no added lines needs no coverage run", ran)
	}
}
