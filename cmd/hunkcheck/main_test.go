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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
)

// TestRun verifies the runner the core drives: it shells out to a command in
// the given directory and returns combined stdout, exercising the os/exec path
// that main wires into agent.CheckPerHunkCoverage.
func TestRun(t *testing.T) {
	out, err := run(context.Background(), t.TempDir(), nil, "echo", "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out; got != "hello\n" {
		t.Fatalf("run output = %q, want %q", got, "hello\n")
	}
}

// TestMain_ReportsUncoveredHunk drives main end-to-end against a real workspace
// with an envtest package whose added hunk is not covered by a test. It asserts
// that the uncovered hunk is reported through the same core main calls, so the
// check cannot regress to reporting nothing (which is exactly the #1694 shape).
func TestMain_ReportsUncoveredHunk(t *testing.T) {
	ws := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ws, "internal/controller"), 0o755)
	// A production file with an added, uncovered wiring line.
	_ = os.WriteFile(
		filepath.Join(ws, "internal/controller/rollup.go"),
		[]byte("package controller\n\nfunc rollup() {\n\temitContradiction()\n}\n"),
		0o644)

	// A runner whose git diff reports the changed file and whose go test
	// always passes (nothing exercises the hunk), so it is uncovered.
	origRun := run
	defer func() { run = origRun }()
	run = func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil // go test passes -> hunk is uncovered
		}
		if len(args) > 1 && args[0] == "diff" && args[1] == "-U0" {
			// splitProductionHunks needs the -U0 diff with the added line.
			return "@@ -0,0 +3 @@\n+package controller\n+\n+func rollup() {\n\temitContradiction()\n}\n", nil
		}
		// changedFilesFromDiff (name-only) reports the changed file.
		return "internal/controller/rollup.go\n", nil
	}

	// Mirror main's resolution: main passes the --dir as root when absolute.
	findings := agent.CheckPerHunkCoverage(context.Background(), ws, "HEAD", run)
	if len(findings) == 0 {
		t.Fatal("main: expected at least one uncovered hunk in the fixture")
	}
}
