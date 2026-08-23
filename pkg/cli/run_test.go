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
	"regexp"
	"slices"
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

// decisionRow returns the rendered table row that starts with lead, or "".
func decisionRow(out, lead string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, lead) {
			return l
		}
	}
	return ""
}

var tableGap = regexp.MustCompile(` {2,}`)

// tableCells splits a rendered row into its columns. tabwriter pads every
// aligned cell with at least two spaces, so a run of two or more spaces is a
// column break while a single space inside a cell survives. Comparing whole
// rows this way pins column order and content without hand-computing padding.
func tableCells(row string) []string {
	return tableGap.Split(strings.TrimRight(row, " "), -1)
}

func TestRenderDecisions(t *testing.T) {
	var buf bytes.Buffer
	renderDecisions(&buf, []Decision{
		{Issue: 1602, Kind: "adjudicate", Reason: "verify found issues",
			Opened:  time.Date(2026, 8, 23, 4, 12, 0, 0, time.UTC),
			Options: []string{"accept", "revise"}},
		{Issue: 1601, Kind: "escalate", Reason: "stalled",
			Opened: time.Date(2026, 8, 23, 5, 0, 0, 0, time.UTC), Answer: "drop"},
		{Issue: 1600, Kind: "unblock", Reason: "needs a human",
			Opened: time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)},
		{Issue: 1603, Kind: "revise", Reason: "coder pushed a fix",
			Opened:  time.Date(2026, 8, 23, 6, 0, 0, 0, time.UTC),
			Options: []string{"accept", "revise"}, Answer: "accept"},
	})
	out := buf.String()
	want := []struct {
		lead  string
		cells []string
	}{
		{lead: "ISSUE", cells: []string{"ISSUE", "KIND", "OPENED", "OPTIONS", "ANSWER", "REASON"}},
		// Unanswered with options: the human can read the valid answers off
		// the table instead of guessing and reading the rejection.
		{lead: "1602", cells: []string{
			"1602", "adjudicate", "2026-08-23 04:12", "accept|revise", "-", "verify found issues"}},
		// Answered with no options: nothing to suppress.
		{lead: "1601", cells: []string{
			"1601", "escalate", "2026-08-23 05:00", "-", "drop", "stalled"}},
		// Answered WITH options, the normal end state of an adjudicate: the
		// answer is the news and the options are spent. Without this row the
		// answered arm is only ever exercised on the option-less path, and
		// suppressing spent options is untested.
		{lead: "1603", cells: []string{
			"1603", "revise", "2026-08-23 06:00", "-", "accept", "coder pushed a fix"}},
		// Unanswered with no options: AnswerDecision accepts anything here.
		{lead: "1600", cells: []string{
			"1600", "unblock", "2026-08-23 03:00", "any", "-", "needs a human"}},
	}
	for _, tc := range want {
		row := decisionRow(out, tc.lead)
		if row == "" {
			t.Errorf("no rendered row leading with %q:\n%s", tc.lead, out)
			continue
		}
		if got := tableCells(row); !slices.Equal(got, tc.cells) {
			t.Errorf("row %q cells = %q, want %q", tc.lead, got, tc.cells)
		}
	}
}

func TestRenderDecisions_EmptySaysSo(t *testing.T) {
	var buf bytes.Buffer
	renderDecisions(&buf, nil)
	if !strings.Contains(strings.ToLower(buf.String()), "no parked decisions") {
		t.Errorf("want an explicit empty message, got %q", buf.String())
	}
}

// The table-level flatten test catches a tab only because tabwriter pads with
// two spaces, so a single space between words cannot be padding. That couples
// the invariant to a rendering constant: drop the tab arm and set padding to 1
// and the table test goes quiet. Hold the invariant here, where nothing about
// the table can reach it.
func TestFlattenCell(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{name: "a tab", in: "verify\tfailed", want: "verify failed"},
		{name: "a newline", in: "verify\nfailed", want: "verify failed"},
		{name: "a carriage return", in: "verify\rfailed", want: "verify failed"},
		// Documenting current behaviour, not requiring it: each control
		// character maps to its own space, so CRLF widens to two. Harmless in
		// a table, and the same reason a tab followed by a space widens.
		{name: "a CRLF becomes two spaces", in: "verify\r\nfailed", want: "verify  failed"},
		{name: "text with nothing to flatten", in: "verify failed", want: "verify failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattenCell(tc.in); got != tc.want {
				t.Errorf("flattenCell(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A cell carrying a newline or a tab does not just mangle its own row, it
// shifts every row after it. Task 8 fills Reason from verify output and stall
// evidence, which is exactly where multi-line strings come from, and Answer
// is a shell argument.
func TestRenderDecisions_FlattensCellsThatWouldBreakTheTable(t *testing.T) {
	opened := time.Date(2026, 8, 23, 4, 12, 0, 0, time.UTC)
	clean := Decision{Issue: 1601, Kind: "escalate", Reason: "stalled", Opened: opened, Answer: "drop"}
	cases := []struct {
		name     string
		mangled  Decision
		wantFlat string
	}{
		{
			name:     "a newline in REASON",
			mangled:  Decision{Issue: 1602, Kind: "adjudicate", Opened: opened, Reason: "verify failed\nrerun it"},
			wantFlat: "verify failed rerun it",
		},
		{
			name:     "a tab in REASON",
			mangled:  Decision{Issue: 1602, Kind: "adjudicate", Opened: opened, Reason: "verify\tfailed"},
			wantFlat: "verify failed",
		},
		{
			name:     "a carriage return in REASON",
			mangled:  Decision{Issue: 1602, Kind: "adjudicate", Opened: opened, Reason: "verify\rfailed"},
			wantFlat: "verify failed",
		},
		{
			name: "a newline in ANSWER",
			mangled: Decision{Issue: 1602, Kind: "adjudicate", Opened: opened, Reason: "stalled",
				Answer: "no\nmaybe"},
			wantFlat: "no maybe",
		},
		{
			name:     "a newline in KIND",
			mangled:  Decision{Issue: 1602, Kind: "adjud\nicate", Opened: opened, Reason: "stalled"},
			wantFlat: "adjud icate",
		},
		{
			name: "a newline in OPTIONS",
			mangled: Decision{Issue: 1602, Kind: "adjudicate", Opened: opened, Reason: "stalled",
				Options: []string{"acc\nept", "revise"}},
			wantFlat: "acc ept|revise",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderDecisions(&buf, []Decision{tc.mangled, clean})
			out := buf.String()
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if len(lines) != 3 {
				t.Fatalf("got %d lines, want a header and one line per decision:\n%s", len(lines), out)
			}
			if !strings.Contains(out, tc.wantFlat) {
				t.Errorf("flattened cell %q missing from:\n%s", tc.wantFlat, out)
			}
			// The row after the mangled one must still sit under the header.
			kind := strings.Index(lines[0], "KIND")
			row := decisionRow(out, "1601")
			if len(row) <= kind || !strings.HasPrefix(row[kind:], "escalate") {
				t.Errorf("the row after the mangled cell is out of alignment:\n%s", out)
			}
		})
	}
}

// Asserts on registered names rather than help text, so rewording a Short
// does not fail a test about wiring. The two pre-existing registrations are
// covered here too; nothing else pins them.
func TestForemanCommand_RegistersItsSubcommands(t *testing.T) {
	paths := [][]string{
		{"dispatch"},
		{"slice"},
		{"run"},
		{"decisions"},
		{"decisions", "answer"},
	}
	for _, path := range paths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			c, _, err := NewForemanCommand().Find(path)
			if err != nil {
				t.Fatalf("foreman %s: %v", strings.Join(path, " "), err)
			}
			if want := path[len(path)-1]; c.Name() != want {
				t.Errorf("foreman %s resolved to %q, want %q", strings.Join(path, " "), c.Name(), want)
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

// An empty queue is a real answer and has to be said out loud.
func TestDecisionsCommand_EmptyDirSaysSo(t *testing.T) {
	out, err := foremanExec(t, "decisions", "--decisions-dir", t.TempDir())
	if err != nil {
		t.Fatalf("Execute() = %v, want no error for an empty decisions dir", err)
	}
	if !strings.Contains(out, "No parked decisions.") {
		t.Errorf("stdout = %q, want the empty queue said out loud", out)
	}
}

func TestDecisionsCommand_MissingDir(t *testing.T) {
	// A directory the human typed and got wrong. Answering a typo with "No
	// parked decisions." turns it into a confident "nothing to do".
	t.Run("typed and absent is an error", func(t *testing.T) {
		typo := filepath.Join(t.TempDir(), "no-such-dir")
		out, err := foremanExec(t, "decisions", "--decisions-dir", typo)
		if err == nil {
			t.Fatal("Execute() = nil, want a typo'd --decisions-dir to be refused")
		}
		if !strings.Contains(err.Error(), typo) {
			t.Errorf("err = %v, want it to name %q", err, typo)
		}
		if out != "" {
			t.Errorf("stdout = %q, want nothing printed for a directory that is not there", out)
		}
	})
	// The default is different: nothing has parked yet is the normal state
	// on a fresh checkout, and erroring on it would make the command unusable
	// before the first run.
	t.Run("the default being absent is not", func(t *testing.T) {
		t.Chdir(t.TempDir())
		out, err := foremanExec(t, "decisions")
		if err != nil {
			t.Fatalf("Execute() = %v, want an absent default dir to be the normal empty state", err)
		}
		if !strings.Contains(out, "No parked decisions.") {
			t.Errorf("stdout = %q, want the empty queue said out loud", out)
		}
	})
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
			// The confirmation names the decision that was written, not the
			// string that was typed.
			name:       "confirms with the parsed issue",
			args:       []string{"+1602", "adjudicate", "accept"},
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
		{
			name:    "refuses a fourth argument",
			args:    []string{"1602", "adjudicate", "accept", "revise"},
			wantErr: "accepts 3 arg(s), received 4",
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

// The loop parks where --decisions-dir says and the human answers where
// --decisions-dir says, but those are two separate registrations. If their
// defaults ever drift, the loop parks somewhere the human is not looking.
func TestDecisionsDirIsNamedInOnePlace(t *testing.T) {
	run := newRunCommand()
	runFlag := run.Flags().Lookup("decisions-dir")
	decFlag := newDecisionsCommand().PersistentFlags().Lookup("decisions-dir")
	if runFlag == nil || decFlag == nil {
		t.Fatalf("--decisions-dir registered on run = %v, on decisions = %v", runFlag, decFlag)
	}
	if runFlag.DefValue != decFlag.DefValue {
		t.Errorf("run defaults to %q but decisions defaults to %q; the loop would park "+
			"where the human is not looking", runFlag.DefValue, decFlag.DefValue)
	}
	if runFlag.DefValue != defaultDecisionsDir {
		t.Errorf("--decisions-dir default = %q, want the package constant %q", runFlag.DefValue, defaultDecisionsDir)
	}
	if !strings.Contains(run.Long, defaultDecisionsDir) {
		t.Errorf("run --help does not name %q, so the help can drift from the flag:\n%s",
			defaultDecisionsDir, run.Long)
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

// The old TestRunCommand_FlagDefaults read DefValue and ShorthandLookup and
// never touched the bound variable, so swapping which struct field --queue and
// --coder-agent bind to survived the whole package. These run the command and
// read every option back out of what it did.

// runQueueFixture writes a one-item queue and its intent, and returns both
// paths.
func runQueueFixture(t *testing.T) (queuePath, intentPath string) {
	t.Helper()
	dir := t.TempDir()
	intentPath = filepath.Join(dir, "1602.md")
	if err := os.WriteFile(intentPath, []byte(runFixtureIntent), 0o600); err != nil {
		t.Fatal(err)
	}
	queuePath = filepath.Join(dir, "queue.yaml")
	body := "repo: defilantech/LLMKube\nitems:\n  - issue: 1602\n    intent: " + intentPath + "\n"
	if err := os.WriteFile(queuePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return queuePath, intentPath
}

const runFixtureIntent = "fix the thing"

// Every value is distinct and none is a flag default, so a flag bound to the
// wrong field lands somewhere the assertions can see. The whole labelled line
// is asserted rather than the bare value, so two options swapping places is a
// failure and not a coincidence.
func TestRunCommand_UsesTheOptionsItWasGiven(t *testing.T) {
	queue, intent := runQueueFixture(t)
	parked := filepath.Join(t.TempDir(), "parked-here")
	out, err := foremanExec(t, "run", "--dry-run",
		"--queue", queue,
		"--coder-agent", "coder-metal",
		"--namespace", "foreman-ns",
		"--decisions-dir", parked,
		"--stall-factor", "3.5")
	if err != nil {
		t.Fatalf("Execute() = %v, want a dry run to succeed", err)
	}
	want := []string{
		"queue: " + queue,
		"coder-agent: coder-metal",
		"namespace: foreman-ns",
		"decisions-dir: " + parked,
		"stall-factor: 3.5",
		"items: 1",
		"issue 1602 (defilantech/LLMKube): branch foreman/wl-1602/issue-1602, intent " +
			intent + " (13 bytes)",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("plan missing %q:\n%s", w, out)
		}
	}
}

// A live run has no cluster-backed effects behind it yet, so it must say so
// rather than print a plan and exit 0 as though something ran.
func TestRunCommand_RefusesALiveRunRatherThanPretending(t *testing.T) {
	queue, _ := runQueueFixture(t)
	out, err := foremanExec(t, "run", "--queue", queue, "--coder-agent", "coder-metal")
	if err == nil {
		t.Fatal("Execute() = nil, want a live run refused while the effects are unwired")
	}
	if !strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("err = %v, want it to name the flag that does work today", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want no plan printed by a run that refused", out)
	}
}

// The queue is the human's input and it is where the mistakes are. Both cases
// run WITHOUT --dry-run: validation has to happen before the run is refused,
// or `run` answers a broken queue with a message about unwired effects.
func TestRunCommand_ReportsWhatIsWrongWithTheQueue(t *testing.T) {
	t.Run("a queue that is not there", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-queue.yaml")
		_, err := foremanExec(t, "run", "--queue", missing, "--coder-agent", "coder-metal")
		if err == nil {
			t.Fatal("Execute() = nil, want a missing queue refused")
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("err = %v, want it to name %q", err, missing)
		}
	})
	t.Run("a queue that is not YAML", func(t *testing.T) {
		queue, _ := runQueueFixture(t)
		// An unbalanced bracket: a parse failure, not a schema complaint, so
		// this exercises ParseQueue's own error path rather than its
		// validation. Nothing else in the CLI tests reaches it.
		body := "repo: defilantech/LLMKube\nitems:\n  - issue: 1602\n    intent: [oops\n"
		if err := os.WriteFile(queue, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := foremanExec(t, "run", "--queue", queue, "--coder-agent", "coder-metal")
		if err == nil {
			t.Fatal("Execute() = nil, want a malformed queue refused")
		}
		if !strings.Contains(err.Error(), "parse queue") {
			t.Errorf("err = %v, want the parse failure surfaced, not swallowed", err)
		}
		if out != "" {
			t.Errorf("stdout = %q, want no plan printed for a queue that would not parse", out)
		}
	})
	t.Run("an intent that is not there", func(t *testing.T) {
		queue, intent := runQueueFixture(t)
		if err := os.Remove(intent); err != nil {
			t.Fatal(err)
		}
		_, err := foremanExec(t, "run", "--queue", queue, "--coder-agent", "coder-metal")
		if err == nil {
			t.Fatal("Execute() = nil, want an unreadable intent refused")
		}
		if !strings.Contains(err.Error(), intent) {
			t.Errorf("err = %v, want it to name %q", err, intent)
		}
	})
}
