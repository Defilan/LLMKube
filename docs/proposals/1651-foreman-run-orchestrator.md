# Proposal: `llmkube foreman run`, an unattended orchestration loop

**Status:** proposed. Design approved; no implementation yet.
**Umbrella issue:** [#1651](https://github.com/defilantech/LLMKube/issues/1651)
**Related:** [#1628](https://github.com/defilantech/LLMKube/issues/1628) (turn accounting, would sharpen stall detection), [#1636](https://github.com/defilantech/LLMKube/issues/1636) (demoted verdict spawns an empty revision cycle), [#1637](https://github.com/defilantech/LLMKube/issues/1637) (gate covers a subset of CI), [#1158](https://github.com/defilantech/LLMKube/issues/1158) (codehost/worktracker interfaces this composes)
**Evidence:** batch of 2026-08-22/23, issues #1602 / #1601 / #1438 / #1628, roughly nine hours on a two-slot local fleet; two runs stalled and were killed by hand, three defects originated in the orchestrator's own instructions.

**Goal:** Let a maintainer hand Foreman a prepared queue of issues and walk
away, returning to opened PRs plus a short list of decisions, instead of
sitting over the fleet dispatching, watching for stalls, killing runs that will
not converge, and deciding what to do with review findings.

---

## Problem

Running a Foreman batch today requires a human (or an agentic assistant) present
for its whole duration. This design is grounded in one real batch:
2026-08-22/23, four issues (#1602, #1601, #1438, #1628) over roughly nine hours
on a two-slot local fleet, where every step between "issue" and "PR" was done
by hand.

### What the orchestrator actually did

Separating that work by whether it needed judgment turns out to be the whole
design.

**Mechanism.** Cross-referenced 87 open issues against 6 open PRs and existing
`foreman/*` branches to find work that was not already done or in flight.
Dispatched Workloads. Polled to terminal. Noticed that two runs had burned
hours with no branch pushed, and killed them. Ran the gate, both lints, the
dead-code guard and the affected package tests against each branch. Enforced
"one feedback pass, then escalate." Squashed, signed off, and opened PRs with
the template and the AI-assistance disclosure.

**Judgment.** Confirmed issues were not already resolved, which required
reading the tree, not the issue text; #1621 and #1585 both had merged PRs whose
`Refs` convention left them open. Authored intents naming real file paths,
known traps, and verify commands. Decided whether each review finding was real.
Decided when to stop feeding findings back and hand-fix instead.

Most of the wall-clock was mechanism. None of it needed intelligence; it needed
a loop that does not get bored.

### The two failures worth designing against

**Runs that will not converge.** `wl-1628-turn-accounting` ran 4 hours and
burned 75 of 120 turns with nothing pushed, against a ~60-minute baseline for
comparable tasks. `wl-1601-revise` reached 110 of 120 turns, also with nothing
pushed. Both were killed by hand. The detection signal in both cases was
simply *elapsed far past baseline, with no branch pushed*.

**Confidently wrong orchestration.** Three defects in that batch originated in
the orchestrator's own instructions, not the coder's work: instructing a delete
that hid a dead file from the guard, asserting a false claim about a file's
symbols, and mis-attributing which test row pinned which conjunct. The coder
executed all three faithfully.

Every one was caught by an **independent adversarial review**. None was caught
by Foreman's gate, which passed all three branches. This is the single most
important constraint on the design: whatever occupies the orchestrator seat must
not also be the thing that grades its output.

## Non-goals for v1

Stated as scope boundaries, not as "later maybe":

- **Authoring intents.** The queue carries intents the human wrote. This is the
  judgment half, and the failure evidence above says it needs an independent
  grader before it can be automated.
- **Adjudicating findings.** Always parked.
- **Idea decomposition** (idea → deliverables → filed issues). A genuinely
  missing stage: nothing in the repo creates an issue, and the planner
  (`pkg/cli/slice_planner.go`) starts *from* issue text. Separate project.
- **A UI.** The Command Center is its own effort and can consume this loop's
  output directory.

## Architecture

A single long-running CLI process. Workstation while iterating; the same binary
in a pod for unattended overnight runs, since run-ahead requires surviving a
closed laptop.

It adds no new control plane. Existing pieces it composes:

| Existing | Role in the loop |
|---|---|
| `pkg/foreman/agent/tools/run_gate_job.go` | already an in-cluster clone-and-check Job harness; becomes the verify stage |
| `pkg/cli/dispatch.go` | exit-coded batch dispatch semantics |
| `pkg/foreman/agent/codehost`, `worktracker` (#1158) | GitHub reads for preflight, without hard-coding a forge |
| `scripts/foreman-finalize.sh` | squash to one human-signed commit and open the PR |
| Workload / AgenticTask CRDs | unchanged; the loop is a client |

### Stage machine

Each queue item advances through fixed stages. `PARK` means: record a decision
with its evidence and **move to the next item**, never block the queue.

```
preflight ──skip?──> done(skipped, reason)
    │
    ▼
 dispatch ──> watch ──stall?──> kill ──> PARK(escalate)
    │           │
    │           ▼
    │        verify ──clean?──> finalize ──> done(PR opened)
    │           │
    │           ▼
    │      PARK(adjudicate)
    │           │
    │      (your answer)
    │           ▼
    │       feedback ──> watch ──> verify ──clean?──> finalize
    │                                  │
    │                                  ▼
    └──────────────────────────> PARK(escalate)   # second failure, always
```

Two properties are structural rather than conventional:

**The one-pass rule is enforced by the machine.** After `feedback` the only
exits are `finalize` or `PARK(escalate)`. There is no path to a third automatic
attempt. Today this is a convention that depends on the operator remembering it.

**Parking never stalls the queue.** A run that needs a decision releases its
slot and the loop starts the next item. Blocking on judgment would relocate the
babysitting rather than remove it.

### Stage detail

**preflight** performs mechanical skips, each with a recorded reason: an open PR
references the issue; a `foreman/<workload>/issue-N` branch already exists on
the fork; a merged PR references it and the tree already contains the change.
The first two are pure API reads. The third is a heuristic and, when
inconclusive, parks rather than guessing. This is the check that caught #1621
and #1585 by hand.

**watch** polls task phases to terminal and evaluates the stall predicate
below.

**verify** runs an independent check on the pushed branch, deliberately not
Foreman's own gate. v1 runs the repo's full blocking check set (the
`DefaultGateChecks` superset from #1637) plus, for chart changes, a
default-render diff against the base. It does not attempt mutation testing;
choosing what to mutate is judgment.

**finalize** calls `foreman-finalize.sh`, which already squashes to one commit
and replaces the bot sign-off with a human one. PR body from the template, with the
AI-assistance disclosure CONTRIBUTING.md requires.

### Stall detection

A run is stalled when **no branch has been pushed** and **elapsed exceeds
`stallFactor × baseline`**, default factor 2.5.

`baseline` is the median `elapsedSec` of that agent's recent successful
`issue-fix` tasks, read from the `foreman.audit.v1` records already written per
task. With no history, fall back to a configured default.

Deliberately not used in v1: turn counts. #1628 would make turn accounting a
first-class metric and would sharpen this (a spin burns turns fast, a genuinely
hard task burns them slowly), but reconstructing it today means parsing
llama.cpp slot logs, which is forensics rather than a signal. Elapsed plus
no-branch-pushed caught both real stalls in the reference batch.

Killing is the one auto-action taken without asking: it deletes the Workload,
whose `watchTaskLiveness` watchdog (#1136) cancels the in-flight run and
releases the model slot within about 90 seconds. The kill is then parked as an
escalate decision, so nothing is silently abandoned.

### Decision queue

Files, one per parked decision:

```
.foreman/decisions/1602-adjudicate.yaml
  issue: 1602
  workload: wl-1602-deadcode-triage
  stage: adjudicate
  opened: 2026-08-23T04:12:00Z
  evidence:
    gate: PASS (make test-chart, 123 tests)
    verify: FAIL
    verifyDetail: ./evidence/1602-verify.txt
    findings: ./evidence/1602-findings.json
  options: [accept, revise, escalate, drop]
```

Reviewed and answered with `llmkube foreman decisions` (list / show / answer).

Files rather than a CRD, for three reasons: they are inspectable and diffable
with no schema to guess at before the shape is known; the Command Center can
read the same directory later without either side committing to an API; and
they keep the loop a pure client of the existing CRDs, so it cannot destabilise
a running fleet.

Posting decisions as GitHub issue comments, reviewable from a phone, evidence
living with the work, is an attractive later adapter, but it is a one-way door
on formatting and puts half-finished machine reasoning on a public repo. Add it
once the shape settles.

### Failure handling

- **API errors during watch** fail open: log and retry, never interpret a
  transient failure as a terminal state. This mirrors `watchTaskLiveness`,
  which only cancels on a definitive NotFound.
- **A crashed loop** resumes from queue-item state on disk; every stage
  transition is written before it is acted on, so a restart re-reads rather
  than re-runs.
- **A kill that does not free the slot** is surfaced, not retried: the watchdog
  path is known to take ~90s, and a longer silence means something the loop
  should not paper over.
- **Two harness defects the loop must tolerate today**, both filed: #1636, where
  a demoted verdict spawns a revision cycle whose feedback is an approval with
  no findings, the loop should treat a NO-GO carrying `extra.verdictDemoted`
  and zero findings as *not* actionable; and #1637, where the gate covers a
  strict subset of CI, which is why `verify` runs its own checks rather than
  trusting GATE-PASS.

## Testing

- **Stage machine** as a pure function of (current state, observed facts) →
  next state, table-driven. The one-pass invariant gets an explicit test: no
  input sequence reaches a third attempt.
- **Stall predicate** table-driven over (elapsed, baseline, branchPushed),
  including the two real cases from the reference batch and the contrast case
  of a slow-but-productive run that must *not* trip it.
- **Preflight skips** against a faked codehost, one case per skip reason.
- **Decision round-trip**: park, list, answer, resume, including answering a
  decision whose run was already killed.
- **No end-to-end fleet test in CI.** It needs a cluster and a model; the
  reference batch is the manual acceptance case.

## Open questions

1. **Queue source.** v1 reads a hand-written `queue.yaml`. Should it also accept
   a label selector (`--label ready-for-foreman`) so the backlog itself is the
   queue?
2. **Concurrency.** The loop currently infers slot availability by watching
   FleetNodes. Should it instead be told a concurrency limit, decoupling it from
   fleet topology?
3. **Where the judgment seam lands.** This design keeps judgment with the human.
   When a model is put in that seat, does it slot in behind the same
   park/answer interface, making the loop indifferent to who answers, or does
   it need a different shape?
