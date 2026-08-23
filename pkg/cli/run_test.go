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

package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// foremanExec runs args through the real `foreman` command group and returns
// what the command wrote to stdout. Going through NewForemanCommand rather
// than calling the constructors directly is deliberate: it exercises the
// registration and the flag wiring instead of asserting they exist. Cobra
// writes errors to stderr, which is discarded here so that a stdout
// assertion cannot be satisfied by an error report.
func foremanExec(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewForemanCommand()
	// Cobra dumps usage to stdout when a command errors, unless the command
	// Execute was called on silences it. In production that is
	// NewRootCommand, which sets SilenceUsage; setting it here models the
	// same run rather than testing a mode the CLI never has.
	cmd.SilenceUsage = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// decisionRow returns the rendered table row for an issue, or "".
func decisionRow(out, issue string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, issue) {
			return l
		}
	}
	return ""
}

func TestRenderDecisions(t *testing.T) {
	var buf bytes.Buffer
	renderDecisions(&buf, []Decision{
		{Issue: 1602, Kind: "adjudicate", Reason: "verify found issues",
			Opened:  time.Date(2026, 8, 23, 4, 12, 0, 0, time.UTC),
			Options: []string{"accept", "revise"}},
		{Issue: 1601, Kind: "escalate", Reason: "stalled",
			Opened: time.Date(2026, 8, 23, 5, 0, 0, 0, time.UTC), Answer: "drop"},
	})
	out := buf.String()
	wants := []string{
		"ISSUE", "KIND", "OPENED", "REASON", "ANSWER",
		"1602", "adjudicate", "verify found issues", "2026-08-23 04:12",
		"1601", "escalate", "stalled", "2026-08-23 05:00", "drop",
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	row := decisionRow(out, "1602")
	if row == "" {
		t.Fatalf("no rendered row for issue 1602:\n%s", out)
	}
	if f := strings.Fields(row); f[len(f)-1] != "-" {
		t.Errorf("unanswered row = %q, want a placeholder in the ANSWER column", row)
	}
}

func TestRenderDecisions_EmptySaysSo(t *testing.T) {
	var buf bytes.Buffer
	renderDecisions(&buf, nil)
	if !strings.Contains(strings.ToLower(buf.String()), "no parked decisions") {
		t.Errorf("want an explicit empty message, got %q", buf.String())
	}
}

func TestForemanCommand_RegistersRunAndDecisions(t *testing.T) {
	cases := []struct {
		sub, wantInHelp string
	}{
		// A flag only the run command declares.
		{sub: "run", wantInHelp: "--stall-factor"},
		// The answer subcommand's own Short, which the parent's Short does
		// not repeat: "answer" alone would match either.
		{sub: "decisions", wantInHelp: "Answer one parked decision"},
	}
	for _, tc := range cases {
		t.Run(tc.sub, func(t *testing.T) {
			out, err := foremanExec(t, tc.sub, "--help")
			if err != nil {
				t.Fatalf("foreman %s --help: %v", tc.sub, err)
			}
			if !strings.Contains(out, tc.wantInHelp) {
				t.Errorf("help for %q missing %q:\n%s", tc.sub, tc.wantInHelp, out)
			}
		})
	}
}

// TestDecisionsCommand_RendersTheGoodOnesAndReportsTheBad pins the ordering
// inside the command: ListDecisions returns partial results alongside an
// error, so reporting the broken file before rendering would hide every
// decision that did parse from the human who ran this to see them.
func TestDecisionsCommand_RendersTheGoodOnesAndReportsTheBad(t *testing.T) {
	dir, _ := decisionFixtureDir(t)
	bad := filepath.Join(dir, "1500-adjudicate.yaml")
	if err := os.WriteFile(bad, []byte("answer: revise\noptions: [accept, revise\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := foremanExec(t, "decisions", "--decisions-dir", dir)
	if err == nil {
		t.Fatal("Execute() = nil, want an error naming the file that could not be read")
	}
	if !strings.Contains(err.Error(), "1500-adjudicate.yaml") {
		t.Errorf("err = %v, want it to name 1500-adjudicate.yaml", err)
	}
	for _, want := range []string{"1602", "adjudicate"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; the decision that parsed must still be shown:\n%s", want, out)
		}
	}
}

// A directory that could not be listed at all must not be reported as an
// empty queue: "No parked decisions." next to an error reads as reassurance.
func TestDecisionsCommand_DoesNotClaimEmptyWhenNothingWasListed(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "decisions")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := foremanExec(t, "decisions", "--decisions-dir", notADir)
	if err == nil {
		t.Fatal("Execute() = nil, want an error for an unlistable decisions dir")
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing printed when nothing could be listed", out)
	}
}

func TestDecisionsCommand_RejectsStrayArguments(t *testing.T) {
	_, err := foremanExec(t, "decisions", "1602", "--decisions-dir", t.TempDir())
	if err == nil {
		t.Fatal("Execute() = nil, want a stray argument to be refused, not ignored")
	}
	if !strings.Contains(err.Error(), `unknown command "1602"`) {
		t.Errorf("err = %v, want it to name the stray argument", err)
	}
}

func TestDecisionsAnswerCommand(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantErr    string
		wantOut    string
		wantAnswer string
	}{
		{
			name:       "records the answer",
			args:       []string{"1602", "adjudicate", "accept"},
			wantOut:    "answered 1602/adjudicate: accept",
			wantAnswer: "accept",
		},
		{
			name:    "refuses an option that was not offered",
			args:    []string{"1602", "adjudicate", "maybe"},
			wantErr: "is not one of",
		},
		{
			name:    "refuses a non-numeric issue",
			args:    []string{"abc", "adjudicate", "accept"},
			wantErr: "ISSUE must be a number",
		},
		{
			name:    "refuses an issue with trailing garbage",
			args:    []string{"1602x", "adjudicate", "accept"},
			wantErr: "ISSUE must be a number",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := decisionFixtureDir(t)
			args := append([]string{"decisions", "answer"}, tc.args...)
			args = append(args, "--decisions-dir", dir)
			out, err := foremanExec(t, args...)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Execute() = %v, want no error", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Execute() = nil, want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if tc.wantOut == "" {
				if out != "" {
					t.Errorf("stdout = %q, want nothing printed for a refused answer", out)
				}
			} else if !strings.Contains(out, tc.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", out, tc.wantOut)
			}
			ds, err := ListDecisions(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(ds) != 1 {
				t.Fatalf("len(decisions) = %d, want the one fixture", len(ds))
			}
			if ds[0].Answer != tc.wantAnswer {
				t.Errorf("recorded answer = %q, want %q", ds[0].Answer, tc.wantAnswer)
			}
		})
	}
}

func TestRunCommand_RequiresQueueAndCoderAgent(t *testing.T) {
	_, err := foremanExec(t, "run")
	if err == nil {
		t.Fatal("Execute() = nil, want the required flags to be enforced")
	}
	for _, want := range []string{"required flag(s)", `"queue"`, `"coder-agent"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}

// The driver lands in task 8. Until then run must say so rather than do half
// a dispatch: this test is expected to be replaced, not kept.
func TestRunCommand_ReportsNotImplemented(t *testing.T) {
	out, err := foremanExec(t, "run", "--queue", "queue.yaml", "--coder-agent", "coder-metal")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("Execute() = %v, want a not-implemented error", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing printed by a command that does nothing", out)
	}
}

// The remaining run flags have no consumer until task 8, so this pins the
// surface a human types and nothing more. It is a registration assertion, not
// a behavioural one, and it should grow teeth when the driver reads them.
func TestRunCommand_FlagDefaults(t *testing.T) {
	cmd := newRunCommand()
	cases := []struct {
		flag, want string
	}{
		{flag: "queue", want: ""},
		{flag: "decisions-dir", want: defaultDecisionsDir},
		{flag: "namespace", want: "default"},
		{flag: "coder-agent", want: ""},
		{flag: "stall-factor", want: strconv.FormatFloat(DefaultStallFactor, 'g', -1, 64)},
		{flag: "dry-run", want: "false"},
	}
	for _, tc := range cases {
		f := cmd.Flags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("--%s is not registered", tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s default = %q, want %q", tc.flag, f.DefValue, tc.want)
		}
	}
	if sh := cmd.Flags().ShorthandLookup("n"); sh == nil || sh.Name != "namespace" {
		t.Errorf("-n shorthand = %v, want it bound to --namespace", sh)
	}
}
