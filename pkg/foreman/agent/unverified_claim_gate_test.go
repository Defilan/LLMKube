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

package agent

import (
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestUnverifiedClaim_GoDemotedOnPhrase(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerUnverifiedClaim(logr.Discard(), extra,
		"Tests fail due to missing godot runtime in environment; cannot verify goal reward or progression logic.",
		foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("GO containing 'cannot verify' must demote to NO-GO, got %s", got)
	}
	if extra["verdictDemotedUnverifiedClaim"] != true {
		t.Fatal("expected verdictDemotedUnverifiedClaim=true")
	}
	if extra["unverifiedClaimReason"] != "cannot verify" {
		t.Fatalf("expected unverifiedClaimReason to name the matched phrase, got %v", extra["unverifiedClaimReason"])
	}
}

func TestUnverifiedClaim_CleanGoStaysGo(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerUnverifiedClaim(logr.Discard(), extra,
		"Fixed duration-0 one-time goal rewards expiring before consumption. Added guard and 5 tests.",
		foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("GO with a clean summary must stay GO, got %s", got)
	}
	if _, demoted := extra["verdictDemotedUnverifiedClaim"]; demoted {
		t.Fatal("clean GO must not set verdictDemotedUnverifiedClaim")
	}
	if _, reason := extra["unverifiedClaimReason"]; reason {
		t.Fatal("clean GO must not set unverifiedClaimReason")
	}
}

func TestUnverifiedClaim_NoGoUntouched(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerUnverifiedClaim(logr.Discard(), extra,
		"cannot verify goal reward logic",
		foremanv1alpha1.AgenticTaskVerdictNoGo)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("rail must only act on GO; NO-GO must pass through, got %s", got)
	}
	if _, demoted := extra["verdictDemotedUnverifiedClaim"]; demoted {
		t.Fatal("NO-GO path must not set demotion keys")
	}
}

func TestUnverifiedClaim_ToggleOff(t *testing.T) {
	t.Setenv("FOREMAN_UNVERIFIED_CLAIM", "0")
	extra := map[string]any{}
	got := enforceReviewerUnverifiedClaim(logr.Discard(), extra,
		"cannot verify goal reward logic",
		foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("toggle off must leave GO untouched, got %s", got)
	}
	if _, demoted := extra["verdictDemotedUnverifiedClaim"]; demoted {
		t.Fatal("toggle off must not set demotion keys")
	}
}

func TestUnverifiedClaim_NilExtraDoesNotPanic(t *testing.T) {
	// nil extra must not panic; the verdict still demotes.
	got := enforceReviewerUnverifiedClaim(logr.Discard(), nil,
		"cannot verify goal reward logic",
		foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("nil extra with a demotable phrase must still demote, got %s", got)
	}
}

func TestUnverifiedClaim_EmptySummaryNoOp(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerUnverifiedClaim(logr.Discard(), extra, "",
		foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("empty summary must be a no-op, got %s", got)
	}
	if _, demoted := extra["verdictDemotedUnverifiedClaim"]; demoted {
		t.Fatal("empty summary must not set demotion keys")
	}
}

func TestUnverifiedClaim_AllPhrasesDemote(t *testing.T) {
	phrases := []string{
		"cannot verify",
		"could not verify",
		"couldn't verify",
		"can't verify",
		"unable to verify",
		"did not verify",
		"was not able to verify",
		"no way to verify",
	}
	for _, phrase := range phrases {
		t.Run(phrase, func(t *testing.T) {
			extra := map[string]any{}
			got := enforceReviewerUnverifiedClaim(logr.Discard(), extra, "we "+phrase+" this change.",
				foremanv1alpha1.AgenticTaskVerdictGo)
			if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
				t.Fatalf("GO with %q must demote to NO-GO, got %s", phrase, got)
			}
		})
	}
}

// TestUnverifiedClaim_CodeCannotVerifyStaysGo is the regression for the defect
// the issue calls out: a summary whose phrase describes the REVIEWED CODE
// failing to verify something (a noun subject, not the reviewer) must NOT be
// demoted. These are ordinary, correct reviews of verification code (cosign
// signing, TEE attestation, JWT validation, checksum checks) and must stay GO.
// Each of these FAILS against the naive strings.Contains matcher, so they
// reproduce the defect and prove the fix.
func TestUnverifiedClaim_CodeCannotVerifyStaysGo(t *testing.T) {
	summaries := []string{
		// Sentence B from the issue: the handler (the code) cannot verify.
		"The handler cannot verify the signature when the key is missing, which this change fixes.",
		// The verifier (the code) cannot verify.
		"The verifier cannot verify a manifest without the public key.",
		// Callers (the code's users) could not verify; past tense.
		"Callers could not verify the checksum before this change.",
	}
	for _, summary := range summaries {
		t.Run(summary, func(t *testing.T) {
			extra := map[string]any{}
			got := enforceReviewerUnverifiedClaim(logr.Discard(), extra, summary,
				foremanv1alpha1.AgenticTaskVerdictGo)
			if got != foremanv1alpha1.AgenticTaskVerdictGo {
				t.Fatalf("GO describing code that cannot verify must stay GO, got %s; summary: %s", got, summary)
			}
			if _, demoted := extra["verdictDemotedUnverifiedClaim"]; demoted {
				t.Fatalf("code-cannot-verify summary must not be marked demoted; summary: %s", summary)
			}
		})
	}
}

// TestUnverifiedClaim_ReviewerCannotVerifyStillDemotes is the issue's own
// sentence (regression): the reviewer could not verify (elided subject after
// a semicolon). This must keep demoting after the boundary-aware fix.
func TestUnverifiedClaim_ReviewerCannotVerifyStillDemotes(t *testing.T) {
	extra := map[string]any{}
	got := enforceReviewerUnverifiedClaim(logr.Discard(), extra,
		"Tests fail due to missing godot runtime in environment; cannot verify goal reward or progression logic.",
		foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("reviewer's own 'cannot verify' (issue's sentence) must still demote to NO-GO, got %s", got)
	}
}

// TestUnverifiedClaim_FirstPersonDemotes covers the first-person distinguisher:
// the reviewer says it (or we) could not verify.
func TestUnverifiedClaim_FirstPersonDemotes(t *testing.T) {
	summaries := []string{
		"I cannot verify this without a running cluster.",
		"We could not verify the behavior end to end.",
		"I still could not verify the fix on this host.",
		"We still can't verify the checksum.",
	}
	for _, summary := range summaries {
		t.Run(summary, func(t *testing.T) {
			extra := map[string]any{}
			got := enforceReviewerUnverifiedClaim(logr.Discard(), extra, summary,
				foremanv1alpha1.AgenticTaskVerdictGo)
			if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
				t.Fatalf("first-person 'cannot verify' must demote to NO-GO, got %s; summary: %s", got, summary)
			}
		})
	}
}

// TestUnverifiedClaim_ClauseInitialAfterPeriod covers a clause-initial match
// after a PERIOD (not just a semicolon), including a leading connective and
// the start of the summary.
func TestUnverifiedClaim_ClauseInitialAfterPeriod(t *testing.T) {
	summaries := []string{
		// After a period, no connective.
		"Build and tests pass. cannot verify the rollout metrics without a live cluster.",
		// After a period with a leading connective.
		"Build and tests pass. however, cannot verify the metrics without a live cluster.",
		// At the very start of the summary (no preceding clause).
		"Cannot verify the rollout metrics without a live cluster.",
		// After a comma.
		"Runtime is missing in the coder image, cannot verify the test command output.",
	}
	for _, summary := range summaries {
		t.Run(summary, func(t *testing.T) {
			extra := map[string]any{}
			got := enforceReviewerUnverifiedClaim(logr.Discard(), extra, summary,
				foremanv1alpha1.AgenticTaskVerdictGo)
			if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
				t.Fatalf("clause-initial 'cannot verify' must demote to NO-GO, got %s; summary: %s", got, summary)
			}
		})
	}
}
