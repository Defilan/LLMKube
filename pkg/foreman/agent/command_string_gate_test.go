package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// commandStringRunner fakes the three git calls checkCommandStringTestDilution
// makes: `git add -A` (no-op), `git diff --name-status --cached HEAD`, and
// the `git diff --cached --unified=0 ...` full diff.
func commandStringRunner(nameStatus, fullDiff string, nsErr, diffErr error) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil
		}
		switch {
		case len(args) >= 2 && args[0] == "add" && args[1] == "-A":
			return "", nil
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--name-status":
			return nameStatus, nsErr
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--cached":
			return fullDiff, diffErr
		default:
			return "", nil
		}
	}
}

// TestCheckCommandStringTestDilution_MustFire verifies the must-fire case:
// a production change to a generated shell command whose only added test
// assertions are ContainSubstring on the command string.
func TestCheckCommandStringTestDilution_MustFire(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
+	Expect(cmdStr).To(ContainSubstring("-I"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	failed, out := checkCommandStringTestDilution(context.Background(), "/w", run)
	if !failed {
		t.Fatal("expected advisory when command-string change has only ContainSubstring assertions")
	}
	if !strings.Contains(out, "command-string") || !strings.Contains(out, "string-shape") {
		t.Errorf("detail = %q", out)
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_ParsedResult verifies
// that a command-string change whose added tests assert a parsed result
// (not string-shape) stays silent.
func TestCheckCommandStringTestDilution_MustStaySilent_ParsedResult(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(size).To(Equal(int64(42)))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("command-string change with parsed-result assertion must stay silent")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_ExitCode verifies
// that a command-string change whose added tests assert an exit code
// stays silent.
func TestCheckCommandStringTestDilution_MustStaySilent_ExitCode(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(exitCode).To(Equal(0))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("command-string change with exit-code assertion must stay silent")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_NoTestChange verifies
// that a non-command production change with string assertions stays silent
// because there's no command-string change.
func TestCheckCommandStringTestDilution_MustStaySilent_NoCommandChange(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	return "hello"
+	return "world"
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(got).To(ContainSubstring("world"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("non-command production change with string assertions must stay silent")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_NoTestFileChange verifies
// that any change where no _test.go file changed stays silent.
func TestCheckCommandStringTestDilution_MustStaySilent_NoTestFileChange(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("command-string change with no test file change must stay silent")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_NoProdChange verifies
// that a test-only submission stays silent.
func TestCheckCommandStringTestDilution_MustStaySilent_NoProdChange(t *testing.T) {
	ns := "M\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("test-only submission must stay silent")
	}
}

// TestCheckCommandStringTestDilution_FailOpenOnGitError verifies that a
// git error fails the check open (silent).
func TestCheckCommandStringTestDilution_FailOpenOnGitError(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
`
	if failed, out := checkCommandStringTestDilution(context.Background(), "/w",
		commandStringRunner(ns, diff, nil, errors.New("boom"))); failed || out != "" {
		t.Fatalf("git error must fail open (silent); got failed=%v out=%q", failed, out)
	}
}

// TestCheckCommandStringTestDilution_FailOpenOnNameStatusError verifies that
// a name-status git error fails the check open (silent).
func TestCheckCommandStringTestDilution_FailOpenOnNameStatusError(t *testing.T) {
	if failed, out := checkCommandStringTestDilution(context.Background(), "/w",
		commandStringRunner("", "", errors.New("boom"), nil)); failed || out != "" {
		t.Fatalf("name-status error must fail open (silent); got failed=%v out=%q", failed, out)
	}
}

// TestCheckCommandStringTestDilution_ShellCommandChange fires on "sh", "-c"
// pattern with only string-shape assertions.
func TestCheckCommandStringTestDilution_ShellCommandChange(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("sh", "-c", "curl -I -w '%{size_download}'")
+	cmd := exec.Command("sh", "-c", "curl -I -w '%{size_total}'")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	failed, out := checkCommandStringTestDilution(context.Background(), "/w", run)
	if !failed {
		t.Fatal("expected advisory for shell command change with only string-shape assertions")
	}
	if !strings.Contains(out, "command-string") {
		t.Errorf("detail = %q", out)
	}
}

// TestCheckCommandStringTestDilution_MixedAssertionsStaysSilent verifies
// that when a command-string change has both string-shape and behavioral
// assertions, the check stays silent (not exclusively string-shape).
func TestCheckCommandStringTestDilution_MixedAssertionsStaysSilent(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +22 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
+	Expect(exitCode).To(Equal(0))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("mixed string-shape and behavioral assertions must stay silent")
	}
}

// TestIsCommandStringLine_ExecCommand verifies the exec.Command( detection.
func TestIsCommandStringLine_ExecCommand(t *testing.T) {
	if !isCommandStringLine("\tcmd := exec.Command(\"curl\", \"-I\")") {
		t.Fatal("exec.Command( should be detected")
	}
}

// TestIsCommandStringLine_ShC verifies the "sh", "-c" detection.
func TestIsCommandStringLine_ShC(t *testing.T) {
	if !isCommandStringLine("\tcmd := exec.Command(\"sh\", \"-c\", \"curl -I\")") {
		t.Fatal("\"sh\", \"-c\" should be detected")
	}
}

// TestIsCommandStringLine_NoMatch verifies non-command lines are not detected.
func TestIsCommandStringLine_NoMatch(t *testing.T) {
	if isCommandStringLine("\treturn \"hello world\"") {
		t.Fatal("plain string return should not be detected as command string")
	}
}

// TestIsStringShapeAssertion_ContainSubstring verifies ContainSubstring is
// classified as string-shape.
func TestIsStringShapeAssertion_ContainSubstring(t *testing.T) {
	if !isStringShapeAssertion("\tExpect(cmdStr).To(ContainSubstring(\"curl\"))") {
		t.Fatal("ContainSubstring should be string-shape")
	}
}

// TestIsStringShapeAssertion_EqualStringLiteral verifies Equal on a string
// literal is classified as string-shape.
func TestIsStringShapeAssertion_EqualStringLiteral(t *testing.T) {
	if !isStringShapeAssertion("\tExpect(got).To(Equal(\"hello\"))") {
		t.Fatal("Equal on string literal should be string-shape")
	}
}

// TestIsStringShapeAssertion_EqualIntNotStringShape verifies Equal on an
// integer is NOT classified as string-shape.
func TestIsStringShapeAssertion_EqualIntNotStringShape(t *testing.T) {
	if isStringShapeAssertion("\tExpect(exitCode).To(Equal(0))") {
		t.Fatal("Equal on integer should not be string-shape")
	}
}

// TestIsStringShapeAssertion_StringsContains verifies strings.Contains is
// classified as string-shape.
func TestIsStringShapeAssertion_StringsContains(t *testing.T) {
	if !isStringShapeAssertion("\tassert.True(t, strings.Contains(cmdStr, \"curl\"))") {
		t.Fatal("strings.Contains should be string-shape")
	}
}

// TestCommandStringTestDilution_RegisteredAsAdvisory verifies the check is
// registered in the gate registry with tierAdvisory.
func TestCommandStringTestDilution_RegisteredAsAdvisory(t *testing.T) {
	var found bool
	var tier gateTier
	for _, c := range gateCheckRegistry("", "", nil) {
		if c.name == "command-string-test-dilution" {
			found = true
			tier = c.tier
		}
	}
	if !found {
		t.Fatal(`gateCheckRegistry is missing the "command-string-test-dilution" check`)
	}
	if tier != tierAdvisory {
		t.Errorf("command-string-test-dilution tier = %v, want tierAdvisory", tier)
	}
}

// TestCommandStringTestDilution_SurfacesAsAdvisoryNotBlocking verifies the
// check never blocks the gate.
func TestCommandStringTestDilution_SurfacesAsAdvisoryNotBlocking(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	blocking, advisories := runGateChecks(context.Background(), "/w", run,
		[]gateCheck{{name: "command-string-test-dilution", tier: tierAdvisory, fn: checkCommandStringTestDilution}})
	if len(blocking) != 0 {
		t.Errorf("command-string-test-dilution must never block; got %d blocking", len(blocking))
	}
	if len(advisories) != 1 || advisories[0].Check != "command-string-test-dilution" {
		t.Fatalf("expected one command-string-test-dilution advisory; got %+v", advisories)
	}
}
