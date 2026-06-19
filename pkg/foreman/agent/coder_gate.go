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
	"os/exec"
	"path"
	"strings"
)

// envtestPackagePrefixes are repo paths whose tests need envtest
// (KUBEBUILDER_ASSETS) or a live cluster and therefore CANNOT run in the
// coder's macOS workspace: they hang waiting for a control plane that is
// not there (the failure that spiraled #734). They are excluded from the
// fast in-workspace test tier and left to the clean-room gate Job.
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
// runner (production callers pass execCommandRunner).
//
// The gate runs five deterministic checks in order: gofmt, go vet,
// go build, golangci-lint, and the fast unit tests for the changed
// non-envtest packages. Heavy envtest or integration tests are
// intentionally out of scope (they need a control plane the workspace
// lacks); they run in a separate clean-room Kubernetes Job. Every check
// runs regardless of earlier failures so the feedback reports everything
// wrong at once.
func RunCoderGate(ctx context.Context, workspace, golangciPath string, run commandRunner) (pass bool, feedback string) {
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
	// files and would not match CI.
	if out, err := run(ctx, workspace, []string{"GOOS=linux"}, golangciPath, "run", "./..."); err != nil {
		failures = append(failures, checkFailure{name: golangciPath + " run ./...", output: out})
	}

	// 5. Fast unit tests for the changed, non-envtest packages. This is what
	// lets the coder stop hand-running `go test` (the loop that thrashed
	// #731): it submits, the gate runs the tests, and a failure comes back
	// as feedback. Envtest/e2e packages are excluded (see
	// envtestPackagePrefixes); the clean-room gate Job covers those.
	if pkgs := changedTestPackages(ctx, workspace, run); len(pkgs) > 0 {
		testArgs := append([]string{"test", "-count=1", "-timeout=180s"}, pkgs...)
		if out, err := run(ctx, workspace, nil, "go", testArgs...); err != nil {
			failures = append(failures, checkFailure{
				name:   "go test " + strings.Join(pkgs, " "),
				output: out,
			})
		}
	}

	if len(failures) == 0 {
		return true, ""
	}

	return false, buildFeedback(failures)
}

// changedTestPackages returns the `./<dir>/...` test patterns for packages
// that have changed or new .go files in the workspace, excluding envtest
// and e2e packages. It reads `git status --porcelain` so it sees the
// coder's uncommitted edits (the gate runs before the executor commits).
// Returns nil when nothing testable changed (e.g. a docs-only edit), in
// which case the gate skips the test tier.
func changedTestPackages(ctx context.Context, workspace string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "status", "--porcelain")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Porcelain lines are "XY <path>"; renames are "XY old -> new".
		fields := strings.Fields(line)
		p := fields[len(fields)-1]
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		excluded := false
		for _, prefix := range envtestPackagePrefixes {
			if strings.HasPrefix(p, prefix) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		dir := path.Dir(p)
		pattern := "./" + dir + "/..."
		if dir == "." {
			pattern = "./..."
		}
		if !seen[pattern] {
			seen[pattern] = true
			pkgs = append(pkgs, pattern)
		}
	}
	return pkgs
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
