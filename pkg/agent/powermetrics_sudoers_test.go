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
	// powermetricsIntervalPlaceholder is the template token the Makefile
	// replaces with APPLE_POWER_INTERVAL_MS.
	powermetricsIntervalPlaceholder = "__LLMKUBE_POWERMETRICS_INTERVAL_MS__"
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

// renderPowermetricsSudoers runs `make render-powermetrics-sudoers`, the
// rendering `make install-powermetrics-sudo` installs, and returns its
// stdout. makeVars go on the make command line, e.g.
// "APPLE_POWER_INTERVAL_MS=2500".
func renderPowermetricsSudoers(t *testing.T, makeVars ...string) (string, error) {
	t.Helper()
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skipf("make not on PATH: %v", err)
	}
	args := append([]string{"-s", "--no-print-directory", "-C", "../..", "render-powermetrics-sudoers"}, makeVars...)
	cmd := exec.Command(makeBin, args...)
	cmd.Env = renderEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
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

// TestPowermetricsSudoersTemplate_NoWildcards guards the pinned argument
// vector. sudo matches a rule's arguments as one string, and a wildcard there
// can match more than one argument vector (sudoers(5), "Wildcards"), so every
// argument must be literal. Arguments that start with ^ or end with $ are a
// sudoers regular expression and are refused for the same reason. The one
// variable part, the sampling interval, is a placeholder the Makefile renders
// as a plain integer.
func TestPowermetricsSudoersTemplate_NoWildcards(t *testing.T) {
	spec := sudoersCommandSpec(t, singleSudoersRule(t, readPowermetricsSudoersTemplate(t)))

	if strings.Contains(strings.ReplaceAll(spec, `\,`, ""), ",") {
		t.Errorf("sudoers rule grants more than one command: %q", spec)
	}
	fields := strings.Fields(spec)
	for _, f := range fields {
		if strings.ContainsAny(f, "*?[") {
			t.Errorf("sudoers argument %q contains a wildcard character; pin the literal value", f)
		}
		if strings.HasPrefix(f, "^") || strings.HasSuffix(f, "$") {
			t.Errorf("sudoers argument %q is a regular expression; pin the literal value", f)
		}
	}
	if fields[0] != DefaultPowermetricsBin {
		t.Errorf("sudoers command = %q, want %q", fields[0], DefaultPowermetricsBin)
	}
	if last := fields[len(fields)-1]; last != powermetricsIntervalPlaceholder {
		t.Errorf("sudoers interval argument = %q, want the %s placeholder", last, powermetricsIntervalPlaceholder)
	}
}

// TestRenderPowermetricsSudoers_PinsInterval renders the fragment through the
// Makefile and requires the installed rule to be the template's rule with the
// interval rendered as a plain integer and nothing else changed. The default
// case pins the Makefile default to the agent's default interval, so a bare
// `make install-powermetrics-sudo` matches an agent run without
// --apple-power-interval.
func TestRenderPowermetricsSudoers_PinsInterval(t *testing.T) {
	templateSpec := sudoersCommandSpec(t, singleSudoersRule(t, readPowermetricsSudoersTemplate(t)))

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
			got := sudoersCommandSpec(t, singleSudoersRule(t, out))
			want := strings.Replace(templateSpec, powermetricsIntervalPlaceholder, tc.wantMS, 1)
			if got != want {
				t.Errorf("rendered rule grants %q, want %q", got, want)
			}
		})
	}
}

// TestRenderPowermetricsSudoers_RejectsInvalidInterval requires the Makefile
// to render nothing for an interval that is not a plain integer of at least
// 100, the only values the agent can pass. Anything else would install a rule
// that never matches, or put characters into the rule that change what it
// grants.
func TestRenderPowermetricsSudoers_RejectsInvalidInterval(t *testing.T) {
	for _, v := range []string{"", "1s", "1000ms", "0100", "0", "99", "1000,ALL", "1*", "1000 -o /tmp/x"} {
		t.Run(strconv.Quote(v), func(t *testing.T) {
			t.Parallel()
			out, err := renderPowermetricsSudoers(t, "APPLE_POWER_INTERVAL_MS="+v)
			if err == nil {
				t.Fatalf("APPLE_POWER_INTERVAL_MS=%q rendered a fragment, want a refusal:\n%s", v, out)
			}
			if out != "" {
				t.Errorf("APPLE_POWER_INTERVAL_MS=%q: refusal still printed output:\n%s", v, out)
			}
		})
	}
}
