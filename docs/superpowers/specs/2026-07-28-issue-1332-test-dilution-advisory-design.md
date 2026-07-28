# Test-dilution advisory (#1332) — design

**Goal:** Surface to the reviewer when a Foreman coder submission earns a green
gate by *weakening its own tests* — deleting assertions or relocating fixtures
that covered the changed code — rather than by being correct.

**Status:** approved design, ready for an implementation plan.

## Problem

In a harness-evaluation batch, two coder branches passed the deterministic gate
and self-reported GO, yet independent review found each shipped a critical
defect the gate could not catch, because the coder made the checks pass by
*weakening* them:

- **#1322 (PR #1325):** the classifier reclassified file URLs as repos (a
  regression) and the serve path returned a URL vLLM rejects. The coder MOVED
  existing test fixtures off `huggingface.co` to `example.com` so the
  reclassification would not break them — deleting the only coverage of the
  regressed path — and added assertions encoding the buggy behavior as expected.
- **#1309 (PR #1326):** a revalidation shell script's size probe was dead code
  (`curl -I -w '%{size_download}'`, always 0), but the tests only string-matched
  the generated command shape, so they were green on a skip branch that can
  never run.

Common failure mode: **the gate verifies that tests PASS, but cannot detect that
the tests were diluted.** A model optimizing for a green gate can satisfy it by
eroding the checks. `GATE-PASS + coder-GO` was not a sufficient signal; only
independent review with real execution caught the defects.

A key asymmetry drove the scope decision: a test rewritten to *assert the wrong
behavior* is still a real, biting test — it bites mutants fine, it just asserts
the wrong thing — so mutation/coverage execution cannot catch it. Only comparing
the test diff against the base can. The two batch failures therefore need
different detectors, and this design ships the one with the highest signal and
lowest false-positive risk.

## Scope

**In scope (this slice):** detect and surface **removed or relocated test
coverage that touched the changed code** — the #1322 dilution-by-relocation
mode.

**Explicit non-goals (deferred, tracked as follow-ups):**

1. **String-match-only tests for a logic/command-string change** (the #1309
   mode): flagging a change to command-string/shell generation whose only added
   tests are substring/shape matches with no executing test. Separate signal.
2. **Coverage / mutation-delta gate:** heavier, overlaps the existing
   mutation-survival check, and — per the asymmetry above — cannot catch #1322,
   so it is not a substitute for this slice.
3. **Assertion-value churn:** flagging existing assertions whose *expected*
   values were rewritten. Noisiest signal (legitimate correct-behavior changes
   also rewrite assertions); deferred to avoid advisory spam.
4. **Hard-blocking.** This slice never fails the gate (see Enforcement).

## Enforcement posture

**Advisory only.** The detector emits a `tierAdvisory` finding that flows to the
reviewer packet; it never fails the gate and is never fed back to the coder.
Rationale:

- It matches the issue author's leading suggestion ("feed a test-diff summary
  into the reviewer prompt so the human sees what test coverage changed") and the
  project's "review beats self-verify" philosophy.
- Distinguishing malicious fixture relocation from a legitimate one is a judgment
  call. A hard block would false-positive on healthy test refactors and burn coder
  turns; an advisory lets the reviewer confirm or dismiss with zero blast radius.
- **Adversary-blind by construction:** advisories are collected in the coder
  self-gate but are *not* part of the coder feedback string, so the coder never
  sees this signal and cannot iterate against it.

## Architecture

One new file, `pkg/foreman/agent/test_dilution_gate.go`, exposing a single
check function wired into `gateCheckRegistry` (in `coder_gate.go`) as a
`tierAdvisory` entry. It reuses the established gate machinery unchanged:

- **`gateCheck` contract** (`gate_registry.go`): the function has signature
  `func(ctx context.Context, workspace string, run commandRunner) (failed bool, output string)`.
  For a `tierAdvisory` check, `failed == true` causes `runGateChecks` to emit
  `advisory{Check: name, Detail: output}`. `failed == false` is silent.
- **Surfacing:** advisories reach the reviewer via the existing
  `attachGateAdvisories` → `Extra["gateAdvisories"]` → `renderGateAdvisories`
  path, rendered under "Gate advisories to verify (confirm or dismiss each)" as
  `[test-dilution] <detail>`. No new plumbing.
- **Kill-switch:** `gateCheckEnabled` gives every registered check a free
  `FOREMAN_TEST_DILUTION_GATE=0` env toggle. No new config.
- **Diff seam:** the same working-tree basis the other diff-based checks use —
  `git add -A` then `git diff --cached HEAD` (the gate runs before the coder
  commits, so `HEAD` is the base branch tip and the staged working tree is the
  full submission). `git diff --name-status --cached HEAD` supplies file-level
  add/delete/rename status for fixture detection.

The check function is a thin orchestrator; the detection logic below lives in
small, individually testable helpers in the same file.

## Detection logic

Let `changedProdPkgs` = the set of package directories that contain a changed
**non-test** Go source file (a changed `*.go` that is not `*_test.go`).

For each changed **test artifact** (a `*_test.go` file, or a file under a
`testdata/` directory) that belongs to a package in `changedProdPkgs`, evaluate
two erosion sub-signals:

### (A) Removed assertions (net erosion)

Parse the unified test diff. Classify each hunk line that is added (`+`) or
removed (`-`) as *assertion-shaped* if, after trimming, it contains any of a
fixed set of assertion tokens:

```
Expect(   Ω(   assert.   require.   ContainSubstring(   t.Error   t.Fatal
!= want   want !=   got !=   != got
```

Per test file, compute `removedAssertions` and `addedAssertions` counts. The
file is flagged for (A) only when `removedAssertions > addedAssertions` — **net
erosion**. Requiring a net decrease keeps a rename, reorder, or one-for-one
rewrite from tripping the signal; it fires only when coverage shrank.

### (B) Relocated or deleted fixtures

Fires on either:

- **File-level:** a file under a `testdata/` directory within a changed-prod
  package's directory tree whose `git diff --name-status` code is `D` (deleted)
  or `R` (renamed). Linkage is by path containment (deterministic); we do not
  attempt to resolve which fixture a given test loads.
- **Literal-level:** within a changed test file, a *fixture-input literal* whose
  value changed — a removed (`-`) line and an added (`+`) line that each contain
  a URL, host, or `testdata/` path literal where the host or path differs. This
  is the exact #1322 signature (`-…huggingface.co…` / `+…example.com…`).

Literal-level detection is scoped to fixture *inputs* (URLs, hosts, `testdata/`
paths), NOT assertion right-hand-side values, to stay inside this slice and out
of the deferred assertion-value-churn signal.

### Fire condition and output

If any package in `changedProdPkgs` shows (A) or (B), the check returns
`failed = true` with a `Detail` that:

- names each affected package,
- states which sub-signal fired and the concrete removed items (removed
  assertion snippets; deleted/renamed fixture paths; changed host/path literals),
- ends with a fixed reviewer instruction, e.g. *"Confirm coverage of the changed
  behavior was not weakened or dodged."*

`Detail` is truncated to the gate's existing `maxCheckOutputBytes` cap so a large
diff cannot produce an unbounded advisory.

## Data flow

```
RunCoderGate
  └─ runGateChecks(..., gateCheckRegistry)
       └─ checkTestDilution(ctx, workspace, run)          [tierAdvisory]
            ├─ git add -A                                  (stage submission)
            ├─ git diff --name-status --cached HEAD        (file add/del/rename)
            ├─ git diff --cached HEAD -- <test paths>      (unified test diff)
            ├─ changedProdPkgs  ← non-test changed files
            ├─ (A) net-removed assertions in changed-prod pkg?
            ├─ (B) deleted/renamed/relocated fixture in changed-prod pkg?
            └─ failed?, detail
  advisories ─ attachGateAdvisories ─ Extra["gateAdvisories"]
             └─ renderGateAdvisories ─ reviewer prompt
```

## Error handling

Fail-open throughout. Any git error, unparseable diff, or empty diff returns
`(false, "")` — the check stays silent rather than blocking or crashing. Because
the tier is advisory, a false negative costs only the pre-existing state (the
reviewer relies on independent review, as today); a crash or block is never
acceptable for a heuristic. Fail-open is the same posture the other diff-based
advisory checks take.

## Testing

Table-driven unit tests with an injected fake `commandRunner` returning canned
`git diff` / `git diff --name-status` output — the established gate-test pattern
(no shelling out). Required cases:

| case | expectation |
|---|---|
| net-removed assertions in a changed-prod package | fires; detail lists removed assertions + names the package |
| fixture host churn (`huggingface.co`→`example.com`) in a changed-prod package | fires; detail names the changed host |
| deleted `testdata/` fixture under a changed-prod package | fires |
| tests only ADDED (coverage grew) | silent |
| assertions removed but that package's production code did NOT change | silent (package-linkage) |
| removed == added assertions (rename/reorder) | silent (net-erosion rule) |
| `git` command error | silent (fail-open) |
| no production change at all (test-only submission) | silent |

Plus a **bite-check** reconstructing a minimal #1322-shaped diff (a fixture URL
moved off `huggingface.co` in a package whose production classifier changed) and
asserting the advisory fires with the host named. Each new helper gets a
bite-check: revert the helper's core condition and confirm its test fails.

## Non-goal follow-ups to file

After this ships, open follow-up issues for the deferred signals so they are not
silently lost: (1) string-match-only detection for command-string/shell changes
(#1309 mode), and (2) a coverage/mutation-delta investigation, noting it does not
subsume this slice.

## Conventions

DCO `git commit -s`; fork workflow (PR into `defilantech/LLMKube`); no em dashes
in prose; `gofmt` + `GOOS=linux ./bin/golangci-lint run ./...` clean before push;
band-3 AI-assisted disclosure on the PR.
