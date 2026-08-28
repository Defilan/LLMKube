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

// Command hunkcheck is the thin CLI wrapper the Foreman gate Job runs with
// `go run ./cmd/hunkcheck` to perform the per-hunk mutation-coverage pass for
// envtest packages. It exists so the bash gate template does not reimplement
// the diff parsing the in-agent gate already owns: the CLI resolves the
// fork-point merge-base the bash side already computed, then delegates to the
// pure pkg/foreman/agent core, which reverts each added hunk in an envtest
// package and runs that package's tests.
//
// A hunk whose revert leaves the package's tests green is uncovered; a clean
// pass or a compile failure (which proves the hunk is load-bearing for
// exercised code) is covered. The core reports findings; it never fails the
// gate itself. The bash Job surfaces each finding as a GATE-WARN advisory
// line and exits 0, so this per-hunk check is reporting-only until a deliberate
// follow-up promotes it to a hard failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
)

// hunkAdvisory is the GATE-WARN line the bash gate Job prints for each
// uncovered hunk. Kept as a package constant so the literal fits the linter's
// line-width limit while still naming the file and the reverted line range.
const hunkAdvisory = "GATE-WARN: hunk check: package %s has an uncovered hunk in %s:%s\n"

// run is the command runner the core drives: it shells out with the given
// directory and extra env, capturing combined stdout+stderr. It is a package
// variable so tests can substitute a fake runner.
var run = func(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func main() {
	dir := flag.String("dir", ".", "workspace root to diff")
	base := flag.String("base", "", "ref to diff against (the fork-point merge-base; defaults to HEAD)")
	flag.Parse()

	if strings.TrimSpace(*base) == "" {
		*base = "HEAD"
	}

	// The gate Job runs from the repo root; the workspace root is the --dir
	// (the cloned checkout). Resolve it relative to the caller's working dir
	// only when it is not already absolute, so the bash Job can pass a path
	// that is correct for its cwd.
	root := *dir
	if !filepath.IsAbs(root) {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "hunkcheck: cannot determine working dir:", err)
			os.Exit(2)
		}
		root = filepath.Join(wd, root)
	}

	for _, f := range agent.CheckPerHunkCoverage(context.Background(), root, *base, run) {
		fmt.Printf(hunkAdvisory, f.Dir, f.File, f.LineRange)
	}
}
