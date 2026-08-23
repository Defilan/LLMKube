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

package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflowDir is the CI definition this gate is meant to mirror.
const workflowDir = "../../../../.github/workflows"

// gateExemptCIChecks are make targets CI runs that DefaultGateChecks
// deliberately omits. Each entry is a DECISION with a stated reason, not
// a formality: an unexplained omission is how a gate quietly stops
// meaning what its name says (#1637).
var gateExemptCIChecks = map[string]string{
	"validate-samples": "requires python3 + the jsonschema package; the gate image is a plain golang " +
		"image and the target hard-exits when they are absent, so adding it would fail every gate run",
	"setup-test-e2e":                "provisions an e2e cluster; lifecycle, not a branch check",
	"cleanup-test-e2e":              "tears down an e2e cluster; lifecycle, not a branch check",
	"test-e2e":                      "needs a live cluster the gate Job does not own",
	"docker-build":                  "image build, not a branch check",
	"docker-build-foreman-agent":    "image build, not a branch check",
	"docker-build-foreman-operator": "image build, not a branch check",
}

// golangciConfigTargets maps a golangci-lint --config= value used by a
// workflow to the make target that runs the same check, so a
// GitHub-Action-invoked lint is compared against the gate like any other.
var golangciConfigTargets = map[string]string{
	".golangci-deadcode.yml": "lint-deadcode",
	".golangci.yml":          "lint",
}

var (
	// A make invocation is either `run: make ...` on one line, or a bare
	// `make ...` line inside a `run: |` block. Both forms are live in
	// this repo (test.yml runs `make test` inside a block), and matching
	// only the first would under-report the gap this test exists to
	// close.
	makeLineRe  = regexp.MustCompile(`^(?:-\s*)?(?:run:\s*)?make\s+(.*)$`)
	golangciRe  = regexp.MustCompile(`--config=(\S+)`)
	makeTargetR = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// makeTargetsIn returns the target names from a make invocation's
// argument tail, stopping at the first token that is a variable
// assignment or otherwise not a target (e.g. `IMG="${IMG}"`).
func makeTargetsIn(tail string) []string {
	var out []string
	for _, tok := range strings.Fields(tail) {
		if !makeTargetR.MatchString(tok) {
			break
		}
		out = append(out, tok)
	}
	return out
}

// ciBlockingChecks returns every make target CI invokes across all
// workflow files, plus the make-target equivalent of any golangci-lint
// action run with an explicit config.
func ciBlockingChecks(t *testing.T) map[string]string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(workflowDir, "*.yml"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no workflow files under %s (err=%v)", workflowDir, err)
	}
	found := map[string]string{}
	for _, f := range entries {
		buf, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		base := filepath.Base(f)
		for _, line := range strings.Split(string(buf), "\n") {
			trimmed := strings.TrimSpace(line)
			// Comments mention targets in prose ("the tests make the code
			// look used", "Run 'make generate ...'"); they invoke nothing.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if m := makeLineRe.FindStringSubmatch(trimmed); m != nil {
				for _, target := range makeTargetsIn(m[1]) {
					found[target] = base
				}
			}
			for _, m := range golangciRe.FindAllStringSubmatch(trimmed, -1) {
				if target, ok := golangciConfigTargets[m[1]]; ok {
					found[target] = base
				}
			}
		}
	}
	return found
}

// TestDefaultGateChecksCoverCI pins the Foreman gate against CI. A
// GATE-PASS is read by operators, and by the verdict machinery, as "this
// branch is expected to pass CI"; that is only true while the gate's
// check list is a superset of what CI blocks on.
func TestDefaultGateChecksCoverCI(t *testing.T) {
	gate := map[string]bool{}
	for _, c := range DefaultGateChecks {
		gate[c] = true
	}

	for target, workflow := range ciBlockingChecks(t) {
		if gate[target] {
			continue
		}
		if reason, ok := gateExemptCIChecks[target]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%q is exempt from DefaultGateChecks with an empty reason; state why", target)
			}
			continue
		}
		t.Errorf("CI (%s) blocks on `make %s` but DefaultGateChecks does not run it: "+
			"a branch can earn GATE-PASS and still fail CI. Add it to DefaultGateChecks, "+
			"or add it to gateExemptCIChecks with a reason.", workflow, target)
	}
}

// TestGateExemptionsAreLive stops the exemption list from outliving the
// CI steps it excuses. A stale entry silently widens the next real gap.
func TestGateExemptionsAreLive(t *testing.T) {
	ci := ciBlockingChecks(t)
	for target := range gateExemptCIChecks {
		if _, ok := ci[target]; !ok {
			t.Errorf("gateExemptCIChecks has %q but no workflow runs it; drop the stale exemption", target)
		}
	}
}
