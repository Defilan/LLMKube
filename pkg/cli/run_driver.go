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
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// WatchResult is what the watch stage observed.
type WatchResult struct {
	// Stalled is the stall predicate's verdict, IsStalled applied to what the
	// watcher saw. The driver does not re-derive it.
	Stalled bool
	// BranchPushed reports whether the coder pushed anything. It is an input
	// to Stalled rather than a second signal the driver reads.
	BranchPushed bool
}

// Effects are the side-effecting operations the driver performs. Kept behind
// an interface so DriveItem's control flow is testable without a cluster;
// the production implementation wraps the controller-runtime client and is a
// follow-up to this plan.
//
// Effects.Preflight is a METHOD and is not the package-level Preflight
// function: that one takes a PreflightProbe and does the forge reads, and the
// production implementation of this method is what will call it.
type Effects interface {
	Preflight(ctx context.Context, item QueueItem, branch string) (string, error)
	Dispatch(ctx context.Context, item QueueItem, intent string) (workload string, err error)
	Watch(ctx context.Context, workload string, baseline time.Duration, factor float64) (WatchResult, error)
	Kill(ctx context.Context, workload string) error
	Verify(ctx context.Context, workload, branch string) (clean bool, evidence string, err error)
}

// taskBranch is the branch Foreman's coder pushes for a Workload. It mirrors
// the controller's own convention, which builds the name from the Workload and
// the issue: workload_controller.go:379 is "foreman/<workload>/issue-<n>".
//
// The workload name is a parameter and not derived from the issue on purpose.
// Effects.Dispatch RETURNS the name it created precisely so the caller does not
// have to guess it, and a production Dispatch has every reason to return
// something other than the reserved name: a retry suffix, a collision-avoiding
// name, a slug carrying the repo. A driver that rebuilt the branch from the
// issue would then watch and verify a branch nobody pushed and call the verify
// clean, which is a silent wrong answer in the direction that matters.
func taskBranch(workload string, issue int32) string {
	return fmt.Sprintf("foreman/%s/issue-%d", workload, issue)
}

// plannedWorkloadName is the Workload name the driver reserves for an item.
//
// Preflight probes the task branch before any Workload exists, so that one call
// has no returned name to work from and has to assume this. It is the only
// place the assumption is unavoidable, and it is a probe rather than a
// judgment: an open PR on the issue is the stronger of preflight's two signals
// and does not depend on the name at all.
func plannedWorkloadName(issue int32) string {
	return fmt.Sprintf("wl-%d", issue)
}

// optionsFor is the answer set a parked decision offers. Every park sets one:
// AnswerDecision accepts arbitrary text when a decision offers no options and
// `foreman decisions` renders that as "any", so an option-less park hands the
// human a free-text prompt instead of the answers the loop understands.
func optionsFor(kind string) []string {
	if kind == "escalate" {
		return []string{"requeue", "hand-fix", "drop"}
	}
	return []string{"accept", "revise", "escalate", "drop"}
}

// DriveItem advances one queue item until it reaches a terminal stage: done,
// or parked. Parking is terminal for this pass by design, so the caller
// proceeds to the next item rather than blocking on a human.
//
// StageFinalize runs no effect. The Effects interface has no Finalize, because
// opening the PR is scripts/foreman-finalize.sh and belongs with the rest of
// the cluster-backed implementation; until that lands a clean verify falls
// straight through finalize to done.
func DriveItem(
	ctx context.Context,
	e Effects,
	item QueueItem,
	intent, decisionsDir string,
	baseline time.Duration,
	factor float64,
) (Stage, error) {
	// Preflight has no Workload to name yet; every stage after dispatch uses
	// the name Dispatch reported.
	branch := taskBranch(plannedWorkloadName(item.Issue), item.Issue)
	var workload, evidence string
	// attempts is inert in v1: nothing routes to StageFeedback, so verify is
	// only ever reached on the first attempt and NextStage's one-pass rule is
	// never the deciding fact. It is counted anyway because the moment the
	// feedback effect lands, that rule depends on it, and a driver that always
	// reported zero would re-adjudicate the same item forever.
	attempts := 0
	stage := StagePreflight

	for {
		facts := Facts{Attempts: attempts}
		switch stage {
		case StagePreflight:
			skip, err := e.Preflight(ctx, item, branch)
			if err != nil {
				return stage, err
			}
			facts.SkipReason = skip
		case StageDispatch:
			w, err := e.Dispatch(ctx, item, intent)
			if err != nil {
				return stage, err
			}
			// An empty name is the one answer the "trust what Dispatch
			// returns" contract cannot absorb: it makes the branch
			// "foreman//issue-<n>", points watch, kill and verify at nothing,
			// and parks a decision whose Workload field is blank, which is
			// the only thing telling a human which run to go and look at.
			// A Workload that cannot be named was not created.
			if w == "" {
				return stage, fmt.Errorf("dispatch for issue %d reported no workload name", item.Issue)
			}
			workload = w
			// The branch follows the Workload that actually exists, not the
			// name reserved for the probe above.
			branch = taskBranch(workload, item.Issue)
			attempts++
		case StageWatch:
			res, err := e.Watch(ctx, workload, baseline, factor)
			if err != nil {
				return stage, err
			}
			if res.Stalled {
				if err := e.Kill(ctx, workload); err != nil {
					return stage, err
				}
			}
			facts.Stalled = res.Stalled
		case StageVerify:
			clean, ev, err := e.Verify(ctx, workload, branch)
			if err != nil {
				return stage, err
			}
			facts.VerifyClean, evidence = clean, ev
		}

		t := NextStage(stage, facts)
		switch t.Next {
		case StageParked:
			d := Decision{
				Issue:    item.Issue,
				Workload: workload,
				Stage:    string(stage),
				Kind:     t.Park,
				Reason:   t.Reason,
				Options:  optionsFor(t.Park),
			}
			// Only when verify actually produced some: a stall is killed
			// before verify runs, and an empty "verify" key would claim a
			// judgment that nothing made.
			if evidence != "" {
				d.Evidence = map[string]string{"verify": evidence}
			}
			// A park that could not be written is not a park. Returning
			// StageParked here would tell the caller a human has been asked
			// when there is nothing on disk to answer.
			if _, err := ParkDecision(decisionsDir, d); err != nil {
				return stage, err
			}
			return StageParked, nil
		case StageDone:
			return StageDone, nil
		}
		stage = t.Next
	}
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
