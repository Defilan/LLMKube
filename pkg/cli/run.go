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
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("not implemented: the driver lands in task 8")
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
// out of alignment.
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

// renderDecisions prints the parked-decision queue as a table.
//
// OPTIONS is the column that makes the listing actionable: without it the
// only way to learn what an answer may be is to read the YAML or guess and
// wait for the rejection. REASON comes last because it is the one free-form
// column, and leading with it would push the actionable ones off the right
// of a narrow terminal.
func renderDecisions(w io.Writer, ds []Decision) {
	if len(ds) == 0 {
		fprintln(w, "No parked decisions.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fprintln(tw, "ISSUE\tKIND\tOPENED\tOPTIONS\tANSWER\tREASON")
	for _, d := range ds {
		answer := d.Answer
		if answer == "" {
			answer = "-"
		}
		fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			d.Issue, flattenCell(d.Kind), d.Opened.Format("2006-01-02 15:04"),
			flattenCell(decisionOptions(d)), flattenCell(answer), flattenCell(d.Reason))
	}
	_ = tw.Flush()
}
