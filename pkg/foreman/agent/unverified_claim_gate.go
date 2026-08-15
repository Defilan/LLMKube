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

// Unverified-claim rail: a reviewer that states in its own summary that it
// could not verify the change still returns GO, which opens a PR whose body is
// that very "cannot verify" sentence. A GO is a verification claim; a reviewer
// that says it could not verify is making the opposite claim in the very field
// that becomes the PR description. Left undemoted, the sentence advertises that
// nothing validated the change while the verdict asserts the opposite (#1454).
//
// This is the cheaper and more general guard from the issue: it needs no
// knowledge of which backstops are configured, unlike a self-gate-deferred
// check. It is deliberately placed LAST in the reviewer rail chain: an earlier
// rail may legitimately demote or promote the verdict, and this one only ever
// acts on a surviving GO.
//
// Disabled by FOREMAN_UNVERIFIED_CLAIM=0.
package agent

import (
	"os"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/go-logr/logr"
)

// unverifiedPhrases are the plain-language phrases a reviewer uses to say it
// could not verify the change. Substring match on the lowercased summary.
var unverifiedPhrases = []string{
	"cannot verify",
	"could not verify",
	"couldn't verify",
	"can't verify",
	"unable to verify",
	"did not verify",
	"was not able to verify",
	"no way to verify",
}

// unverifiedClaimDisabled reports whether the unverified-claim rail is turned
// off. Default (unset) is ENABLED; setting FOREMAN_UNVERIFIED_CLAIM=0
// disables it.
func unverifiedClaimDisabled() bool {
	return os.Getenv("FOREMAN_UNVERIFIED_CLAIM") == "0"
}

// enforceReviewerUnverifiedClaim demotes a surviving reviewer GO to NO-GO when
// the summary states the reviewer could not verify the change. It is a no-op
// when the rail is disabled, when the verdict is not GO, or when the summary
// is empty. On a match it records the demotion in extra so downstream audit
// and analytics can see it.
func enforceReviewerUnverifiedClaim(
	log logr.Logger,
	extra map[string]any,
	summary string,
	verdict foremanv1alpha1.AgenticTaskVerdict,
) foremanv1alpha1.AgenticTaskVerdict {
	if unverifiedClaimDisabled() {
		return verdict
	}
	if verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		return verdict
	}
	if summary == "" {
		return verdict
	}

	lower := strings.ToLower(summary)
	for _, phrase := range unverifiedPhrases {
		if phraseCountsAsUnverified(lower, phrase) {
			if extra != nil {
				extra["verdictDemotedUnverifiedClaim"] = true
				extra["unverifiedClaimReason"] = phrase
			}
			log.Info("demoting GO to NO-GO: reviewer stated it could not verify the change",
				"phrase", phrase, "summary", summary)
			return foremanv1alpha1.AgenticTaskVerdictNoGo
		}
	}
	return verdict
}

// clauseConnectives are the leading connectives that may sit between a clause
// boundary and an elided-subject "cannot verify" phrase. They are ignored
// when deciding whether the phrase is clause-initial (see
// unverifiedClaimSubjectElidedOrFirstPerson).
var clauseConnectives = map[string]bool{
	"but": true, "and": true, "however": true, "so": true, "yet": true,
}

// firstPersonSubjects are the subjects that, immediately before an "cannot
// verify" phrase, identify the speaker (the reviewer) as the one who could
// not verify — the case that must demote.
var firstPersonSubjects = map[string]bool{"i": true, "we": true}

// phraseCountsAsUnverified reports whether the lowercased summary contains
// the phrase in a position that means the REVIEWER could not verify the
// change — as opposed to describing the reviewed CODE's inability to verify
// something. A plain strings.Contains cannot tell these apart:
//
//   - A. "Tests fail ... in environment; cannot verify goal reward or
//     progression logic." — the subject of "cannot verify" is elided; it is
//     the reviewer. This must demote.
//   - B. "The handler cannot verify the signature when the key is missing,
//     which this change fixes." — the subject is the noun "handler"; the
//     reviewed code cannot verify. This must NOT demote.
//
// The mechanical distinguisher (see unverifiedClaimSubjectElidedOrFirstPerson)
// counts a match only when the phrase is clause-initial (subject elided at a
// clause boundary) or first-person (immediately preceded by "i"/"we"); a noun
// subject like "handler" or "verifier" describes the code and is left alone.
func phraseCountsAsUnverified(lower, phrase string) bool {
	for from := 0; ; {
		idx := strings.Index(lower[from:], phrase)
		if idx < 0 {
			return false
		}
		if unverifiedClaimSubjectElidedOrFirstPerson(lower[:from+idx]) {
			return true
		}
		from += idx + len(phrase)
	}
}

// unverifiedClaimSubjectElidedOrFirstPerson reports whether the lowercased
// summary text before a matched phrase (prefix) leaves the phrase's subject
// elided (clause-initial) or first-person, i.e. the reviewer is the one who
// could not verify the change. It is the A-vs-B distinguisher from
// phraseCountsAsUnverified: a bare strings.Contains demotes BOTH the
// reviewer's own "cannot verify" (A) and a sentence about the reviewed code
// failing to verify (B); this helper demotes only A.
//
// The phrase counts when EITHER:
//
//   - it is clause-initial — at the start of the summary, or preceded
//     (ignoring whitespace and an optional leading connective) by one of
//     . ; : , or a newline, where the subject is elided; OR
//   - it is first-person — immediately preceded by a first-person subject
//     ("i", "we", "i still", "we still").
//
// Otherwise (a noun subject such as "handler", "verifier", "callers") the
// phrase describes the reviewed code and the verdict must be left alone.
func unverifiedClaimSubjectElidedOrFirstPerson(prefix string) bool {
	trimmed := strings.TrimRight(prefix, " \t\r\n")
	if trimmed == "" {
		// The phrase sits at the very start of the summary: there is no
		// subject, so the reviewer's is elided.
		return true
	}

	// First-person: the last one or two whole words are a first-person
	// subject ("i"/"we", optionally followed by "still"). Whole-word
	// matching keeps "software" from matching "we" and "handler" from
	// matching anything.
	words := strings.Fields(trimmed)
	last := words[len(words)-1]
	if firstPersonSubjects[last] {
		return true
	}
	if last == "still" && len(words) >= 2 && firstPersonSubjects[words[len(words)-2]] {
		return true
	}

	// Clause-initial: ignore whitespace and an optional leading connective,
	// then require a clause boundary (or the start of the summary) before the
	// phrase.
	core := trimmed
	for {
		words := strings.Fields(core)
		if len(words) >= 1 && clauseConnectives[words[len(words)-1]] {
			c := words[len(words)-1]
			core = strings.TrimRight(core[:strings.LastIndex(core, c)], " \t\r\n")
			if core == "" {
				// Only a connective preceded the phrase (start of summary).
				return true
			}
			continue
		}
		break
	}
	boundary := core[len(core)-1]
	return strings.ContainsAny(string(boundary), ".;:,\n")
}
