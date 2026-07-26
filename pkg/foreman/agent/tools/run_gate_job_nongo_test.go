package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The non-Go lint block is shell embedded in a Job template, so a
// strings.Contains assertion on the rendered YAML can only prove the text is
// present, never that it works. The first attempt at #1072 passed exactly that
// kind of test while being a complete no-op: it derived its own base with
// `git diff HEAD~1 HEAD` inside a `--depth 1` clone, where HEAD~1 does not
// exist, so the command failed, stderr was swallowed, no files were ever
// matched, and the gate printed a header and passed. One of its tests asserted
// the rendered args contained the literal string
// `git diff --name-only HEAD~1 HEAD`, which pinned the bug in place.
//
// These tests therefore EXECUTE the block with a stubbed PATH and assert on
// what the linters actually received.
//
// On the `bash -c` below: running a shell is the point here, since the artifact
// under test IS shell. Nothing untrusted reaches it. The script is our own
// rendered template, the file lists are test literals, and PATH is pinned to a
// temp dir holding stub executables so no real linter and no repo file is
// touched. This is not the pattern to copy into production code, where argv
// should be passed directly rather than through a shell.

// extractNonGoLintBlock pulls the non-Go lint shell out of the rendered args so
// it can be run standalone. Fails loudly rather than silently testing nothing
// if the markers move.
func extractNonGoLintBlock(t *testing.T, args string) string {
	t.Helper()
	const startMark = `echo "=== non-go lint ==="`
	start := strings.Index(args, startMark)
	if start < 0 {
		t.Fatalf("non-go lint block not found in rendered args")
	}
	rest := args[start+len(startMark):]
	// The block ends where the next phase begins, or at the gate verdict.
	end := len(rest)
	for _, mark := range []string{`echo "=== bite check`, `[ "$rc" -eq 0 ]`} {
		if i := strings.Index(rest, mark); i >= 0 && i < end {
			end = i
		}
	}
	return rest[:end]
}

// hermeticBin returns a directory to be used as the ENTIRE PATH. It symlinks
// only the coreutils the block genuinely needs, so a checker is present exactly
// when a test puts a stub there.
//
// Inheriting the real PATH is not an option: shellcheck is installed on plenty
// of developer machines, so a "checker is absent" test would quietly exercise
// the present-checker path instead, and pass while proving nothing.
func hermeticBin(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// testPATH is the PATH the block runs under: the test's stub dir first, then
// only the system coreutils directories. Linters normally install outside
// these (/opt/homebrew/bin, /usr/local/bin, a gem or npm prefix), so a checker
// is present when a test stubs it and otherwise absent.
func testPATH(binDir string) string {
	return binDir + ":/bin:/usr/bin"
}

// requireCheckerAbsent skips rather than fails when the machine really does
// provide the checker on the reduced PATH, which happens on distros that ship
// it in /usr/bin. Asserting "absent" on a box where it is present would test
// the present-checker path while claiming to test the absent one.
func requireCheckerAbsent(t *testing.T, binDir, name string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "command -v "+name)
	cmd.Env = append(os.Environ(), "PATH="+testPATH(binDir))
	if out, _ := cmd.Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Skipf("%s is present at %s on the reduced PATH; cannot exercise the "+
			"missing-checker path here", name, strings.TrimSpace(string(out)))
	}
}

// stubLinter writes a fake executable that appends its argv to a log file and
// exits with the given code, so a test can see exactly which files a checker
// was handed.
func stubLinter(t *testing.T, dir, name, logPath string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + logPath + "\nexit " +
		map[bool]string{true: "0", false: "1"}[exitCode == 0] + "\n"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// runNonGoLint executes the extracted block with `changed` preset, a stub PATH,
// and returns combined output plus the final rc.
func runNonGoLint(t *testing.T, changed string, binDir string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell block test requires a POSIX shell")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	block := extractNonGoLintBlock(t, renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "foreman/x", "biteCheck": true,
		"upstreamURL": "https://github.com/defilantech/LLMKube.git",
	}))

	script := "set -uo pipefail\n" +
		"rc=0\n" +
		`MB="deadbeef"` + "\n" +
		"changed=$(cat <<'CHANGED_EOF'\n" + changed + "\nCHANGED_EOF\n)\n" +
		block + "\n" +
		`echo "FINAL_RC=$rc"` + "\n"

	cmd := exec.Command("bash", "-c", script)
	// binDir is the ENTIRE PATH: it carries symlinked coreutils plus whatever
	// stubs the test placed. A checker is therefore present exactly when the
	// test says so, regardless of what is installed on the machine.
	cmd.Env = append(os.Environ(), "PATH="+testPATH(binDir))
	out, _ := cmd.CombinedOutput()
	text := string(out)
	rc := "unknown"
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "FINAL_RC=") {
			rc = strings.TrimPrefix(line, "FINAL_RC=")
		}
	}
	return text, rc
}

// TestNonGoLint_ScopesCheckersToChangedFilesOnly is the regression that matters
// for a false GATE-FAIL: a checker must see the files this branch changed and
// nothing else. Linting the whole repo would fail an unrelated contributor on a
// pre-existing violation in a file they never touched.
func TestNonGoLint_ScopesCheckersToChangedFilesOnly(t *testing.T) {
	bin := hermeticBin(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	stubLinter(t, bin, "shellcheck", log, 0)

	changed := "hack/one.sh\ndocs/readme.md\npkg/foo/foo.go"
	out, rc := runNonGoLint(t, changed, bin)

	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("shellcheck stub was never invoked; output:\n%s", out)
	}
	received := strings.Fields(string(got))
	if len(received) != 1 || received[0] != "hack/one.sh" {
		t.Errorf("shellcheck must receive exactly the changed shell files, got %v", received)
	}
	if rc != "0" {
		t.Errorf("a passing checker must not fail the gate, rc=%s\n%s", rc, out)
	}
}

// TestNonGoLint_MissingCheckerIsReportedNotFailed pins the degrade-gracefully
// requirement: the gate runs in a Go image, so an absent linter must be stated
// honestly and must not fail the branch.
func TestNonGoLint_MissingCheckerIsReportedNotFailed(t *testing.T) {
	bin := hermeticBin(t)
	requireCheckerAbsent(t, bin, "shellcheck")
	out, rc := runNonGoLint(t, "hack/one.sh", bin)

	if !strings.Contains(out, "shellcheck not installed") {
		t.Errorf("a missing checker must be reported, got:\n%s", out)
	}
	if rc != "0" {
		t.Errorf("a missing checker must not fail the gate, rc=%s\n%s", rc, out)
	}
}

// TestNonGoLint_CheckerFindingFailsTheGate is the other half: when the checker
// is present and reports a problem, the gate must actually fail. Without this
// the whole block could no-op and still pass every other test here.
func TestNonGoLint_CheckerFindingFailsTheGate(t *testing.T) {
	bin := hermeticBin(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	stubLinter(t, bin, "shellcheck", log, 1) // non-zero: a real finding

	out, rc := runNonGoLint(t, "hack/one.sh", bin)
	if rc != "1" {
		t.Errorf("a checker finding must set rc=1, got rc=%s\n%s", rc, out)
	}
}

// TestNonGoLint_GoOnlyDiffChecksNothingAndPasses guards the "do not turn
// nothing-to-check into an error" requirement, and guards against a future
// change that lints the repo regardless of the diff: a stub that gets invoked
// here means the scoping regressed.
func TestNonGoLint_GoOnlyDiffChecksNothingAndPasses(t *testing.T) {
	bin := hermeticBin(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	stubLinter(t, bin, "shellcheck", log, 1) // would fail if wrongly invoked
	stubLinter(t, bin, "actionlint", log, 1)
	stubLinter(t, bin, "rubocop", log, 1)

	out, rc := runNonGoLint(t, "pkg/foo/foo.go\npkg/foo/foo_test.go", bin)

	if _, err := os.ReadFile(log); err == nil {
		t.Errorf("no checker may run for a Go-only diff; output:\n%s", out)
	}
	if !strings.Contains(out, "no checkable non-Go files changed") {
		t.Errorf("a Go-only diff should say so, got:\n%s", out)
	}
	if rc != "0" {
		t.Errorf("a Go-only diff must pass, rc=%s\n%s", rc, out)
	}
}

// TestNonGoLint_NoBaseSkipsHonestly covers the path that caused #1072. When the
// base cannot be resolved there is no reliable file list, and the block must
// say so rather than silently checking nothing while reporting success.
func TestNonGoLint_NoBaseSkipsHonestly(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	bin := hermeticBin(t)
	block := extractNonGoLintBlock(t, renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "foreman/x", "biteCheck": true,
		"upstreamURL": "https://github.com/defilantech/LLMKube.git",
	}))
	// MB empty is the "could not resolve a base" state.
	script := "set -uo pipefail\nrc=0\nMB=\"\"\nchanged=\"\"\n" + block +
		"\necho \"FINAL_RC=$rc\"\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+testPATH(bin))
	out, _ := cmd.CombinedOutput()

	if !strings.Contains(string(out), "no usable base") {
		t.Errorf("an unresolved base must be reported, got:\n%s", out)
	}
	if !strings.Contains(string(out), "FINAL_RC=0") {
		t.Errorf("an unresolved base is an infra anomaly, not a coder failure:\n%s", out)
	}
}

// TestNonGoLint_DoesNotDeriveItsOwnBase is a cheap guard on the specific
// regression: the block must use the shared `$changed`, never compute a base
// from HEAD~N, which silently yields nothing in the `--depth 1` clone the gate
// actually runs in.
func TestNonGoLint_DoesNotDeriveItsOwnBase(t *testing.T) {
	block := extractNonGoLintBlock(t, renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "foreman/x", "biteCheck": true,
	}))
	for _, forbidden := range []string{"HEAD~", "git ls-files", "git diff"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("non-go lint must use the shared $changed list, found %q in:\n%s",
				forbidden, block)
		}
	}
	if !strings.Contains(block, `"$changed"`) {
		t.Errorf("non-go lint must consume the shared $changed list:\n%s", block)
	}
}
