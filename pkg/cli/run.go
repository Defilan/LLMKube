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
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

const defaultDecisionsDir = ".foreman/decisions"

type runOptions struct {
	queueFile    string
	decisionsDir string
	namespace    string
	coderAgent   string
	stallFactor  float64
	dryRun       bool
}

// newRunCommand drives a prepared queue through the Foreman pipeline,
// parking judgment calls instead of blocking on them.
func newRunCommand() *cobra.Command {
	opts := &runOptions{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Drive a queue of issues through Foreman unattended",
		// The default directory is interpolated rather than spelled out, so
		// the help cannot drift from the flag.
		Long: fmt.Sprintf(`Drive a prepared queue of issues through the Foreman pipeline.

The loop does the mechanical work (preflight skips, dispatch, watching for
stalls, independent verification, finalizing a PR) and parks judgment calls
as files under %s for you to review with
'llmkube foreman decisions'. It never blocks: a parked decision releases the
slot and the loop moves to the next item.

Intents are yours to write; the queue points at them.

Example:

  llmkube foreman run --queue queue.yaml --coder-agent qwen38-coder`, defaultDecisionsDir),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printRunPlan(cmd.OutOrStdout(), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.queueFile, "queue", "", "Path to the work queue YAML (required)")
	f.StringVar(&opts.decisionsDir, "decisions-dir", defaultDecisionsDir, "Where parked decisions are written")
	f.StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace for Workloads")
	f.StringVar(&opts.coderAgent, "coder-agent", "", "Coder Agent name (required)")
	f.Float64Var(&opts.stallFactor, "stall-factor", DefaultStallFactor,
		"Kill a run with no branch after this multiple of baseline")
	f.BoolVar(&opts.dryRun, "dry-run", false, "Print what would happen without applying anything")
	// Both are documented as required and neither has a defensible default:
	// a queue-less run has nothing to do, and an agent-less run would have to
	// guess which box does the work.
	_ = cmd.MarkFlagRequired("queue")
	_ = cmd.MarkFlagRequired("coder-agent")
	return cmd
}

// printRunPlan loads the queue the human prepared, checks every intent is
// readable, and prints what a run would do.
//
// A live run is refused rather than half-attempted: the cluster-backed Effects
// (dispatch, watch, verify, kill) are a follow-up to this plan, so there is
// nothing to drive the loop with yet. Validating the queue is real work that
// needs no cluster and it catches the mistakes a human actually makes, so it
// happens BEFORE the refusal: answering a queue with a missing intent by
// talking about unwired effects helps nobody.
func printRunPlan(w io.Writer, opts *runOptions) error {
	// Before the queue: a bad --stall-factor is a typo in the command that was
	// just typed, and reporting a queue problem first would have the human
	// editing a file when the fix is on the command line.
	if err := checkStallFactor(opts.stallFactor); err != nil {
		return err
	}
	data, err := os.ReadFile(opts.queueFile)
	if err != nil {
		return fmt.Errorf("read queue: %w", err)
	}
	q, err := ParseQueue(data)
	if err != nil {
		return err
	}
	intents := make([]string, len(q.Items))
	for i, item := range q.Items {
		s, err := q.IntentFor(item)
		if err != nil {
			return err
		}
		intents[i] = s
	}
	if !opts.dryRun {
		return fmt.Errorf("the queue is valid, but a live run cannot start yet: dispatch, "+
			"watch and verify against the cluster are not wired up. Re-run with --dry-run "+
			"to see what %d item(s) would do", len(q.Items))
	}
	// Labelled with the flag names that produced them, so the plan reads back
	// as the command that made it.
	fprintln(w, "dry run, nothing is applied.")
	fprintf(w, "queue: %s\n", opts.queueFile)
	fprintf(w, "coder-agent: %s\n", opts.coderAgent)
	fprintf(w, "namespace: %s\n", opts.namespace)
	fprintf(w, "decisions-dir: %s\n", opts.decisionsDir)
	fprintf(w, "stall-factor: %g\n", opts.stallFactor)
	fprintf(w, "items: %d\n", len(q.Items))
	for i, item := range q.Items {
		// The reserved name: nothing has dispatched, so there is no returned
		// Workload name to build the branch from.
		fprintf(w, "  issue %d (%s): branch %s, intent %s (%d bytes)\n",
			item.Issue, item.Repo, taskBranch(plannedWorkloadName(item.Issue), item.Issue),
			item.IntentPath, len(intents[i]))
	}
	return nil
}

// checkStallFactor refuses a stall budget that would kill every run.
//
// pflag parses the value with strconv.ParseFloat, which happily accepts "NaN",
// "Inf" and "-1", and IsStalled compares elapsed against factor x baseline: at
// zero or below the threshold is zero or negative and the first watch tick
// declares a run that has been going for a second stalled, and NaN converts to
// a Duration that every elapsed time is greater than, which does the same. The
// flag is the place to say so, because the human can see which flag is wrong
// before anything is dispatched. IsStalled defends itself as well, for callers
// that never went through this command.
func checkStallFactor(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return fmt.Errorf("--stall-factor must be a finite number greater than 0, got %g: "+
			"anything else kills every run on its first watch tick", f)
	}
	return nil
}

// newDecisionsCommand lists and answers parked decisions.
func newDecisionsCommand() *cobra.Command {
	dir := defaultDecisionsDir
	cmd := &cobra.Command{
		Use:   "decisions",
		Short: "List and answer decisions the run loop parked",
		// Without this a stray argument is silently ignored and the full
		// queue is listed, which reads as "nothing matched".
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A missing default directory means nothing has parked yet,
			// which is the normal state and not worth an error. A missing
			// directory the human typed is a typo, and answering a typo with
			// "No parked decisions." turns it into a confident "nothing to
			// do" while the real queue sits unread somewhere else.
			if cmd.Flags().Changed("decisions-dir") {
				if _, err := os.Stat(dir); err != nil {
					return fmt.Errorf("--decisions-dir %s: %w", dir, err)
				}
			}
			ds, err := ListDecisions(dir)
			// Render first. ListDecisions returns what it could parse
			// alongside an error naming what it could not, and this command
			// exists to show a human what is parked: one truncated file must
			// not hide the rest. Returning the error afterwards still exits
			// non-zero and names the file. Nothing is rendered when nothing
			// was listed, so "No parked decisions." never appears next to an
			// error and reads as reassurance.
			if len(ds) > 0 || err == nil {
				renderDecisions(cmd.OutOrStdout(), ds)
			}
			return err
		},
	}
	cmd.PersistentFlags().StringVar(&dir, "decisions-dir", defaultDecisionsDir, "Where parked decisions live")

	answer := &cobra.Command{
		Use:   "answer ISSUE KIND ANSWER",
		Short: "Answer one parked decision",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			// ParseInt, not Sscanf: Sscanf("1602x", "%d") succeeds with 1602
			// and no error, so a typo would silently answer a decision the
			// human did not name.
			issue, err := strconv.ParseInt(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("ISSUE must be a number: %w", err)
			}
			if err := AnswerDecision(dir, int32(issue), args[1], args[2]); err != nil {
				return err
			}
			// The parsed issue, not the argument: the confirmation should
			// name the decision that was actually written.
			fprintf(cmd.OutOrStdout(), "answered %d/%s: %s\n", issue, args[1], args[2])
			return nil
		},
	}
	cmd.AddCommand(answer)
	return cmd
}

// flattenCell keeps one table cell on one line. Reason arrives from verify
// output and stall evidence and Answer from a shell argument, so either can
// carry a newline or a tab, and either one shifts every row that follows it
// out of alignment. Every cell goes through it rather than only those two: a
// decision file is human-editable YAML, so any field can come back mangled.
func flattenCell(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
}

// decisionOptions renders the answers a decision will accept. An unanswered
// decision that offers no options accepts anything, since AnswerDecision only
// enforces the list when there is one, and saying so beats a bare placeholder
// that reads as "nothing you can do here".
func decisionOptions(d Decision) string {
	if d.Answer != "" {
		return "-"
	}
	if len(d.Options) == 0 {
		return "any"
	}
	return strings.Join(d.Options, "|")
}

// orDash renders an empty cell as a dash. tabwriter pads an empty cell out to
// the column width, so an omitted value is indistinguishable from a row that
// has slipped a column; a dash says "nothing here" out loud.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// renderDecisions prints the parked-decision queue as a table.
//
// WORKLOAD is the only column that says which run to go and look at: ISSUE
// names the work, but a human wanting the logs, the branch or the transcript
// needs the Workload, and without the column the only way to get it is to open
// the YAML. OPTIONS is the column that makes the listing actionable: without it
// the only way to learn what an answer may be is to read the YAML or guess and
// wait for the rejection. REASON comes last because it is the one free-form
// column, and leading with it would push the actionable ones off the right
// of a narrow terminal.
//
// Decision.Stage is deliberately NOT a column. Every park the machine produces
// today already names its stage in REASON ("stalled" only comes from watch,
// both verify reasons say "verify"), so a STAGE column would spend width
// repeating what the row already says and push the actionable columns further
// right. The field stays in the YAML for anyone who needs it.
func renderDecisions(w io.Writer, ds []Decision) {
	if len(ds) == 0 {
		fprintln(w, "No parked decisions.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fprintln(tw, "ISSUE\tWORKLOAD\tKIND\tOPENED\tOPTIONS\tANSWER\tREASON")
	for _, d := range ds {
		fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			d.Issue, flattenCell(orDash(d.Workload)), flattenCell(d.Kind),
			d.Opened.Format("2006-01-02 15:04"),
			flattenCell(decisionOptions(d)), flattenCell(orDash(d.Answer)), flattenCell(d.Reason))
	}
	_ = tw.Flush()
}
