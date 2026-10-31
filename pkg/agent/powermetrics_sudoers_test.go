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

package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests cover the shipped powermetrics sudoers template and the
// Makefile rendering of it. They carry no build tag so CI (Linux) runs them;
// the darwin-only tests in power_darwin_test.go tie the rendered rule to the
// agent's argv.

const (
	// powermetricsSudoersTemplate is the shipped fragment, relative to this
	// package directory.
	powermetricsSudoersTemplate = "../../deployment/macos/sudoers.d/llmkube-powermetrics"
	// powermetricsUserPlaceholder and powermetricsIntervalPlaceholder are the
	// template tokens the Makefile replaces with `whoami` and
	// APPLE_POWER_INTERVAL_MS.
	powermetricsUserPlaceholder     = "__LLMKUBE_USER__"
	powermetricsIntervalPlaceholder = "__LLMKUBE_POWERMETRICS_INTERVAL_MS__"
	// wantPowermetricsTemplateRule is the whole rule line the template must
	// carry.
	wantPowermetricsTemplateRule = powermetricsUserPlaceholder + ` ALL = (root) NOPASSWD: ` +
		`/usr/bin/powermetrics --samplers cpu_power\,gpu_power -i ` + powermetricsIntervalPlaceholder
)

// sudoersRuleLines returns the lines of a sudoers file that are not blank
// and not comments, with backslash-newline continuations joined. Include
// directives look like comments but are not, so they are returned too.
func sudoersRuleLines(text string) []string {
	text = strings.ReplaceAll(text, "\\\n", " ")
	var rules []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#include") {
			continue
		}
		rules = append(rules, line)
	}
	return rules
}

// sudoersCommandSpec returns the command and arguments a rule grants: the
// text after its NOPASSWD: tag.
func sudoersCommandSpec(t *testing.T, rule string) string {
	t.Helper()
	_, spec, ok := strings.Cut(rule, "NOPASSWD:")
	if !ok {
		t.Fatalf("sudoers rule %q has no NOPASSWD: tag", rule)
	}
	return strings.TrimSpace(spec)
}

// singleSudoersRule returns the one rule line of a sudoers file.
func singleSudoersRule(t *testing.T, text string) string {
	t.Helper()
	rules := sudoersRuleLines(text)
	if len(rules) != 1 {
		t.Fatalf("want exactly one sudoers rule line, got %d: %q", len(rules), rules)
	}
	return rules[0]
}

func readPowermetricsSudoersTemplate(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(powermetricsSudoersTemplate)
	if err != nil {
		t.Fatalf("read shipped sudoers template: %v", err)
	}
	return string(data)
}

// runRepoMake runs make in the repository root with env and returns its
// stdout; a failure carries stderr.
func runRepoMake(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skipf("make not on PATH: %v", err)
	}
	cmd := exec.Command(makeBin, append([]string{"-s", "--no-print-directory", "-C", "../.."}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// renderPowermetricsSudoers runs `make render-powermetrics-sudoers`, the
// rendering `make install-powermetrics-sudo` installs, and returns its
// stdout. makeVars go on the make command line, e.g.
// "APPLE_POWER_INTERVAL_MS=2500".
func renderPowermetricsSudoers(t *testing.T, makeVars ...string) (string, error) {
	t.Helper()
	return runRepoMake(t, renderEnv(), append([]string{"render-powermetrics-sudoers"}, makeVars...)...)
}

// renderEnv is this process's environment minus what an outer make exports
// (so `make test` does not leak its flags into the nested make) and minus
// APPLE_POWER_INTERVAL_MS (so the Makefile default is what renders when a
// test passes no override).
func renderEnv() []string {
	environ := os.Environ()
	env := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS", "MAKELEVEL", "MAKEOVERRIDES", "MAKEFILES",
			"APPLE_POWER_INTERVAL_MS":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// currentUser is the name `$(shell whoami)` renders into the rule.
func currentUser(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("whoami").Output()
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestPowermetricsSudoersTemplate_PinsExactRule pins the whole rule line, so
// a change to the user, host, run-as user, tags or command shows up here and
// not only a change to the arguments.
func TestPowermetricsSudoersTemplate_PinsExactRule(t *testing.T) {
	if got := singleSudoersRule(t, readPowermetricsSudoersTemplate(t)); got != wantPowermetricsTemplateRule {
		t.Errorf("template rule = %q\nwant            %q", got, wantPowermetricsTemplateRule)
	}
}

// TestPowermetricsSudoersTemplate_NoWildcards guards the pinned command line.
// sudo matches a rule's arguments as one space-joined string, and a wildcard
// there can match more than one command line (sudoers(5), "Wildcards"), so
// every field must be literal. sudo treats the arguments as a regular
// expression only when the whole argument string starts with ^ and ends with
// $; this test is deliberately stricter and refuses any field that starts with
// ^ or ends with $. The one variable part, the sampling interval, is a
// placeholder the Makefile renders as a plain integer.
func TestPowermetricsSudoersTemplate_NoWildcards(t *testing.T) {
	rule := singleSudoersRule(t, readPowermetricsSudoersTemplate(t))
	spec := sudoersCommandSpec(t, rule)

	if strings.Contains(strings.ReplaceAll(spec, `\,`, ""), ",") {
		t.Errorf("sudoers rule grants more than one command: %q", spec)
	}
	for _, f := range strings.Fields(rule) {
		if strings.ContainsAny(f, "*?[") {
			t.Errorf("sudoers field %q contains a wildcard character; pin the literal value", f)
		}
		if strings.HasPrefix(f, "^") || strings.HasSuffix(f, "$") {
			t.Errorf("sudoers field %q looks like a regular expression; pin the literal value", f)
		}
	}
	fields := strings.Fields(spec)
	if fields[0] != DefaultPowermetricsBin {
		t.Errorf("sudoers command = %q, want %q", fields[0], DefaultPowermetricsBin)
	}
	if last := fields[len(fields)-1]; last != powermetricsIntervalPlaceholder {
		t.Errorf("sudoers interval argument = %q, want the %s placeholder", last, powermetricsIntervalPlaceholder)
	}
}

// TestRenderPowermetricsSudoers_PinsInterval renders the fragment through the
// Makefile and requires the whole installed rule to be the template's rule
// with the user and the interval substituted and nothing else changed. The
// default case pins the Makefile default to the agent's default interval, so
// a bare `make install-powermetrics-sudo` matches an agent run without
// --apple-power-interval.
func TestRenderPowermetricsSudoers_PinsInterval(t *testing.T) {
	templateRule := singleSudoersRule(t, readPowermetricsSudoersTemplate(t))
	user := currentUser(t)

	cases := []struct {
		name     string
		makeVars []string
		wantMS   string
	}{
		{name: "Makefile default is the agent default",
			wantMS: strconv.FormatInt(DefaultApplePowerInterval.Milliseconds(), 10)},
		{name: "explicit interval", makeVars: []string{"APPLE_POWER_INTERVAL_MS=2500"}, wantMS: "2500"},
		{name: "minimum interval", makeVars: []string{"APPLE_POWER_INTERVAL_MS=100"}, wantMS: "100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := renderPowermetricsSudoers(t, tc.makeVars...)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(out, "__LLMKUBE_") {
				t.Errorf("rendered fragment still contains a placeholder:\n%s", out)
			}
			want := strings.NewReplacer(
				powermetricsUserPlaceholder, user,
				powermetricsIntervalPlaceholder, tc.wantMS,
			).Replace(templateRule)
			if got := singleSudoersRule(t, out); got != want {
				t.Errorf("rendered rule = %q\nwant            %q", got, want)
			}
		})
	}
}

// TestRenderPowermetricsSudoers_RejectsInvalidInterval requires the Makefile
// to render nothing for an interval that is not a plain integer from 100 to
// 999999999. Anything else would install a rule that never matches, or put
// characters into the rule that change what it grants. Each value is passed
// the way a user would, on the make command line and through the environment,
// exactly as their shell delivers it after its own quoting: make turns $$
// into $, so for example "$$(echo 1000)" reaches the recipe as $(echo 1000).
func TestRenderPowermetricsSudoers_RejectsInvalidInterval(t *testing.T) {
	values := []string{
		"", "1s", "1000ms", "0100", "0", "99", "1000,ALL", "1*", "1000 -o /tmp/x",
		"$$(echo 1000)", "`echo 1000`", "$${C-1000}$${C+[0-9]*}$${C=}", "1000'",
		"99999999999999999999", "1000000000",
	}
	for _, v := range values {
		for _, channel := range []string{"command line", "environment"} {
			t.Run(channel+" "+strconv.Quote(v), func(t *testing.T) {
				t.Parallel()
				env := renderEnv()
				args := []string{"render-powermetrics-sudoers"}
				if channel == "environment" {
					env = append(env, "APPLE_POWER_INTERVAL_MS="+v)
				} else {
					args = append(args, "APPLE_POWER_INTERVAL_MS="+v)
				}
				out, err := runRepoMake(t, env, args...)
				if err == nil {
					t.Fatalf("APPLE_POWER_INTERVAL_MS=%q rendered a fragment, want a refusal:\n%s", v, out)
				}
				if out != "" {
					t.Errorf("APPLE_POWER_INTERVAL_MS=%q: refusal still printed output:\n%s", v, out)
				}
			})
		}
	}
}

// TestInstallPowermetricsSudo_DryRunUsesRenderRecipe keeps install from
// drifting from render. `make -n` runs nothing; its output must show install
// writing exactly the render recipe (for the requested interval and the
// shipped template) into the file it syntax-checks and installs, and both
// sudo steps aborting the recipe on failure. A stub sudo first on PATH would
// record any command that did run.
func TestInstallPowermetricsSudo_DryRunUsesRenderRecipe(t *testing.T) {
	stubDir := t.TempDir()
	sudoLog := filepath.Join(stubDir, "sudo.log")
	stub := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> '%s'\nexit 1\n", sudoLog)
	if err := os.WriteFile(filepath.Join(stubDir, "sudo"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	env := renderEnv()
	for i, kv := range env {
		if path, ok := strings.CutPrefix(kv, "PATH="); ok {
			env[i] = "PATH=" + stubDir + string(os.PathListSeparator) + path
		}
	}

	renderDry, err := runRepoMake(t, env, "-n", "render-powermetrics-sudoers", "APPLE_POWER_INTERVAL_MS=2500")
	if err != nil {
		t.Fatalf("make -n render-powermetrics-sudoers: %v", err)
	}
	installDry, err := runRepoMake(t, env, "-n", "install-powermetrics-sudo", "APPLE_POWER_INTERVAL_MS=2500")
	if err != nil {
		t.Fatalf("make -n install-powermetrics-sudo: %v", err)
	}

	render := strings.TrimSpace(renderDry)
	for _, want := range []string{"ms='2500';", `"s/__LLMKUBE_POWERMETRICS_INTERVAL_MS__/$ms/"`,
		strings.TrimPrefix(powermetricsSudoersTemplate, "../../")} {
		if !strings.Contains(render, want) {
			t.Errorf("render recipe does not contain %q:\n%s", want, render)
		}
	}
	for _, want := range []string{
		"( " + render + " ) > \"$TMP\" || {",
		`sudo visudo -cf "$TMP" || {`,
		`sudo install -m 0440 -o root -g wheel "$TMP" /etc/sudoers.d/llmkube-powermetrics || {`,
	} {
		if !strings.Contains(installDry, want) {
			t.Errorf("install recipe does not contain %q:\n%s", want, installDry)
		}
	}
	if ran, err := os.ReadFile(sudoLog); err == nil {
		t.Errorf("make -n ran sudo: %s", ran)
	}
}
