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
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/repomap"
)

// envtestPackagePrefixes are workspace-relative package path prefixes whose
// tests require KUBEBUILDER_ASSETS (envtest) or a live cluster, which the
// coder workspace does not have. The fast gate's unit-test tier skips them
// (running them hangs); CI runs them separately.
var envtestPackagePrefixes = []string{
	"internal/controller/",
	"internal/foreman/controller/",
	"test/",
}

// maxCheckOutputBytes bounds the captured output included in the gate
// feedback for each failing check, so a noisy compiler or linter cannot
// produce an unbounded user message. Output longer than this is truncated.
const maxCheckOutputBytes = 8 * 1024

// commandRunner runs one command in dir with extra env vars (KEY=VALUE)
// appended to the process environment, returning combined stdout+stderr
// and the exec error. Injectable so tests do not shell out.
type commandRunner func(
	ctx context.Context,
	dir string,
	extraEnv []string,
	name string,
	args ...string,
) (output string, err error)

// execCommandRunner is the production commandRunner backed by os/exec. It
// appends extraEnv to the inherited process environment and captures
// combined stdout+stderr. Wired into the coder agent loop via
// makeCoderGateVerifier as the runner passed to RunCoderGate.
var execCommandRunner commandRunner = func(
	ctx context.Context,
	dir string,
	extraEnv []string,
	name string,
	args ...string,
) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// checkFailure records a single failing verification check and the output
// that explains the failure.
type checkFailure struct {
	name   string
	output string
}

// RunCoderGate runs the fast in-workspace verification tier against a
// coder's workspace and reports whether every check passed. On failure,
// feedback is a directive the agent loop injects as a user message:
// it names the failing check(s) and includes their output so the model
// can fix the issue and resubmit. golangciPath is the path to the
// golangci-lint binary (e.g. "./bin/golangci-lint"). run is the command
// runner (production callers pass execCommandRunner). issueText is the
// text the scope guard uses to rank files; empty string disables the
// scope check (backward compatible).
//
// The gate runs seven deterministic checks in order: gofmt, go vet,
// go build, golangci-lint, a fast unit-test tier on changed packages,
// a codegen-drift check, and a scope-overlap check. Heavy envtest or
// integration tests are intentionally out of scope; they run in a
// separate clean-room Kubernetes Job. All checks run regardless of
// earlier failures so the feedback reports everything wrong at once.
func RunCoderGate(
	ctx context.Context, workspace, golangciPath string, run commandRunner, issueText string,
) (pass bool, feedback string) {
	var failures []checkFailure

	// 1. gofmt -l . lists misformatted files on stdout and exits 0 even
	// when files are listed, so the failure signal is non-empty output,
	// not the exec error.
	if out, _ := run(ctx, workspace, nil, "gofmt", "-l", "."); strings.TrimSpace(out) != "" {
		failures = append(failures, checkFailure{name: "gofmt -l .", output: out})
	}

	// 2. go vet ./... fails with a non-nil error.
	if out, err := run(ctx, workspace, nil, "go", "vet", "./..."); err != nil {
		failures = append(failures, checkFailure{name: "go vet ./...", output: out})
	}

	// 3. go build ./... fails with a non-nil error.
	if out, err := run(ctx, workspace, nil, "go", "build", "./..."); err != nil {
		failures = append(failures, checkFailure{name: "go build ./...", output: out})
	}

	// 4. golangci-lint run ./... fails with a non-nil error. GOOS=linux is
	// required: on macOS, plain lint silently skips //go:build !darwin
	// files and would not match CI. GOLANGCI_LINT_CACHE is scoped to a
	// per-workspace sibling directory so stale analysis results from
	// another coder workspace cannot pollute this run's lint (#759); the
	// sibling location keeps the cache out of the workspace git tree.
	lintEnv := []string{"GOOS=linux", "GOLANGCI_LINT_CACHE=" + workspace + ".golangci-cache"}
	if out, err := run(ctx, workspace, lintEnv, golangciPath, "run", "./..."); err != nil {
		failures = append(failures, checkFailure{name: golangciPath + " run ./...", output: out})
	}

	// 5. Fast unit-test tier: go test on the non-envtest packages the coder
	// changed. The static checks above cannot catch a failing or panicking
	// unit test, so a broken test would otherwise reach a GO and only fail
	// in CI (#762). Envtest/integration packages are excluded (they need
	// KUBEBUILDER_ASSETS / a cluster the workspace lacks; CI runs them).
	if pkgs := changedTestPackages(ctx, workspace, run); len(pkgs) > 0 {
		args := append([]string{"test", "-count=1", "-timeout=180s"}, pkgs...)
		if out, err := run(ctx, workspace, nil, "go", args...); err != nil {
			failures = append(failures, checkFailure{name: "go test " + strings.Join(pkgs, " "), output: out})
		}
	}

	// 6. Codegen-drift check: regenerate manifests/CRDs and fail if the
	// tree is dirty. This catches changes to API types, kubebuilder
	// markers, or field doc comments that alter generated CRDs or
	// role.yaml before they reach CI (#775). Skipped gracefully if
	// controller-gen is unavailable.
	if drifted, out := checkCodegenDrift(ctx, workspace, run); drifted {
		failures = append(failures, checkFailure{name: "codegen drift", output: out})
	}

	// 7. Scope-overlap check: flag a submit whose changed-file set has
	// near-zero overlap with the files the issue text implies are
	// relevant. This catches a coder that drifts to an unrelated
	// subsystem (e.g. editing pkg/cli/cache.go for an issue about
	// pkg/agent/). Skipped when issueText is empty (backward
	// compatible). See issue #782.
	if scopeFail, out := checkScopeOverlap(ctx, workspace, run, issueText); scopeFail {
		failures = append(failures, checkFailure{name: "scope overlap", output: out})
	}

	if len(failures) == 0 {
		return true, ""
	}

	return false, buildFeedback(failures)
}

// changedTestPackages returns the workspace-relative Go package directories
// (as "./<dir>/" patterns) that have uncommitted changes per
// `git status --porcelain` and are not envtest-backed. It dedups packages
// and ignores non-Go files and root-level (package main) changes. A git
// error yields no packages (the tier is skipped rather than failing the
// gate spuriously).
func changedTestPackages(ctx context.Context, workspace string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "status", "--porcelain")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		// porcelain is "XY <path>" (renames end with "-> <new>"); the
		// final field is the current path.
		path := fields[len(fields)-1]
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		dir := filepath.Dir(path)
		if dir == "." {
			continue // root package main; not part of the unit-test tier
		}
		dirKey := dir + "/"
		excluded := false
		for _, pfx := range envtestPackagePrefixes {
			if strings.HasPrefix(dirKey, pfx) {
				excluded = true
				break
			}
		}
		if excluded || seen[dirKey] {
			continue
		}
		seen[dirKey] = true
		pkgs = append(pkgs, "./"+dirKey)
	}
	return pkgs
}

// checkCodegenDrift regenerates manifests and CRDs, then checks whether
// the workspace tree is dirty. It returns (drifted, output) where output
// lists the drifted files. Skipped (returns false, "") if
// bin/controller-gen is not present in the workspace.
func checkCodegenDrift(ctx context.Context, workspace string, run commandRunner) (drifted bool, output string) {
	controllerGen := filepath.Join(workspace, "bin", "controller-gen")
	if _, err := run(ctx, workspace, nil, "test", "-f", controllerGen); err != nil {
		// controller-gen not available; skip gracefully.
		return false, ""
	}

	// Regenerate manifests, CRDs, and sync to Helm charts.
	if out, err := run(ctx, workspace, nil, "make", "manifests", "chart-crds", "foreman-chart-crds"); err != nil {
		// If make itself fails, report it as a drift failure.
		return true, "make manifests chart-crds foreman-chart-crds failed:\n" + out
	}

	// Check whether the tree is dirty after regeneration.
	if _, err := run(ctx, workspace, nil, "git", "diff", "--quiet"); err == nil {
		return false, ""
	}

	// Tree is dirty; collect the list of drifted files.
	out, _ := run(ctx, workspace, nil, "git", "diff", "--name-only")
	msg := "Generated files drifted after regeneration. " +
		"Run 'make manifests chart-crds foreman-chart-crds' and commit the changes.\n\n" +
		"Drifted files:\n"
	return true, msg + out
}

// buildFeedback renders the directive and a per-check section for every
// failing check, truncating each check's output to maxCheckOutputBytes.
func buildFeedback(failures []checkFailure) string {
	var b strings.Builder
	b.WriteString("The verification gate failed. Fix the issues below and resubmit.\n")
	for _, f := range failures {
		b.WriteString("\n## ")
		b.WriteString(f.name)
		b.WriteString("\n")
		b.WriteString(truncateOutput(f.output))
		b.WriteString("\n")
	}
	return b.String()
}

// truncateOutput caps output at maxCheckOutputBytes, keeping the tail
// (most recent output) and prefixing a marker when truncation occurs.
func truncateOutput(output string) string {
	if len(output) <= maxCheckOutputBytes {
		return output
	}
	return "...(truncated)...\n" + output[len(output)-maxCheckOutputBytes:]
}

// scopeOverlapChecker resolves the set of relevant files for a given
// workspace and issue text. It returns a scored-order slice and a
// membership map. Injectable so tests can supply canned relevant files
// without walking the real filesystem.
type scopeOverlapChecker func(workspace, issueText string) (paths []string, set map[string]bool)

// defaultScopeChecker is the production scopeOverlapChecker backed by
// the repomap scorer.
var defaultScopeChecker scopeOverlapChecker = func(workspace, issueText string) (paths []string, set map[string]bool) {
	files, err := repomap.Walk(workspace, nil)
	if err != nil {
		return nil, nil
	}
	scored := repomap.ScoreFiles(files, issueText)
	out := make([]string, 0, len(scored))
	s := make(map[string]bool, len(scored))
	for _, sf := range scored {
		out = append(out, sf.Path)
		s[sf.Path] = true
	}
	return out, s
}

// checkScopeOverlap verifies that the coder's changed files have
// meaningful overlap with the files the issue text implies are
// relevant. It returns (scopeFail, output) where scopeFail is true
// when the overlap is near-zero. output is the feedback text.
//
// The check works by:
//  1. Collecting the set of changed files from git status.
//  2. Collecting the set of relevant files from the repomap scorer
//     (scored against issueText).
//  3. Computing the overlap ratio: |changed ∩ relevant| / |changed|.
//     If the ratio is below the threshold (currently 0, i.e., zero
//     overlap), the check fails.
//
// The threshold is intentionally conservative (zero overlap) to avoid
// false positives. A coder that touches one relevant file plus several
// unrelated files will still pass; only a coder that touches zero
// relevant files is flagged.
//
// Skipped when issueText is empty (backward compatible).
func checkScopeOverlap(
	ctx context.Context, workspace string, run commandRunner, issueText string,
) (scopeFail bool, output string) {
	if issueText == "" {
		return false, ""
	}

	// 1. Collect changed files.
	changed := changedFiles(ctx, workspace, run)
	if len(changed) == 0 {
		return false, ""
	}

	// 2. Collect relevant files from the repomap scorer.
	relevantPaths, relevantSet := defaultScopeChecker(workspace, issueText)
	if len(relevantPaths) == 0 {
		return false, ""
	}

	// 3. Compute overlap.
	overlap := 0
	for _, c := range changed {
		if relevantSet[c] {
			overlap++
		}
	}

	// Zero overlap means the coder touched no files the issue implies
	// are relevant. This is the drift signal (#782).
	if overlap > 0 {
		return false, ""
	}

	// Build feedback that tells the coder which files they changed
	// vs. which files are relevant.
	var b strings.Builder
	b.WriteString("Your changes have no overlap with the files relevant to this issue.\n")
	b.WriteString("Changed files:\n")
	for _, c := range changed {
		b.WriteString("  - ")
		b.WriteString(c)
		b.WriteString("\n")
	}
	b.WriteString("\nRelevant files (top scored by issue text):\n")
	limit := minRelevantFiles
	if len(relevantPaths) < limit {
		limit = len(relevantPaths)
	}
	for _, r := range relevantPaths[:limit] {
		b.WriteString("  - ")
		b.WriteString(r)
		b.WriteString("\n")
	}
	if len(relevantPaths) > minRelevantFiles {
		b.WriteString(fmt.Sprintf("  ... and %d more\n", len(relevantPaths)-minRelevantFiles))
	}
	return true, b.String()
}

// changedFiles returns the set of workspace-relative paths that have
// uncommitted changes per `git status --porcelain`. It returns the
// paths as a slice (order is not guaranteed). A git error yields an
// empty slice.
func changedFiles(ctx context.Context, workspace string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "status", "--porcelain")
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		// porcelain is "XY <path>" (renames end with "-> <new>"); the
		// final field is the current path.
		paths = append(paths, fields[len(fields)-1])
	}
	return paths
}

// minRelevantFiles caps how many relevant files are shown in the
// scope-overlap feedback. The repomap can return dozens of files;
// showing the top 10 is enough for the coder to see the drift.
const minRelevantFiles = 10
