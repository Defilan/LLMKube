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

// Per-clause coverage rail.
//
// An issue commonly enumerates more than one required behaviour, and a coder
// reliably implements the one its first test happens to cover and stops. The
// coder prompt already pastes the enumerated clauses as an unchecked
// checklist (see buildUserPrompt); this rail closes the loop on the review
// side: it checks whether the reviewer's findings actually covered each clause,
// and for any clause the reviewer never addressed it appends a major finding
// so the gap is surfaced rather than silently omitted.
//
// This extends the existing issueAsk machinery, which checked that the
// reviewer restated the issue; that compares against the whole body and is
// brittle to light reformatting. Per-clause coverage instead asks, for each
// named behaviour, whether the reviewer pointed at something that satisfies it.
//
// Non-blocking by design: unsatisfied clauses become findings, not a verdict
// demotion. The reviewer's GO/NO-GO stays authoritative; the findings enrich
// it and are recorded on the task so a human or the controller can route them.
package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
)

// clauseStopWords are the most common English stopwords stripped before the
// token-overlap check. Dropping them lets a reviewer paraphrase a clause
// ("the disabled path keeps its no-side-effect behaviour") match the clause
// ("the disabled path preserves the original no-side-effect behaviour") even
// though no 60-character prefix of the clause survives as a substring, which
// is the defect this rail exists to replace.
var clauseStopWords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "at": true, "for": true, "with": true,
	"out": true, "is": true, "are": true, "was": true, "were": true, "be": true,
	"been": true, "being": true, "it": true, "its": true, "this": true, "that": true,
	"these": true, "those": true, "as": true, "by": true, "from": true, "into": true,
	"than": true, "then": true, "so": true, "if": true, "must": true, "shall": true,
	"should": true, "will": true, "would": true, "can": true, "could": true,
	"have": true, "has": true, "had": true, "do": true, "does": true, "did": true,
}

// clauseMatchThreshold is the minimum number of non-stopword clause tokens
// the reviewer's cited text must share for the clause to count as covered.
// Two is high enough to reject an unrelated one-word mention while still
// forgiving a paraphrase that keeps the clause's key terms.
const clauseMatchThreshold = 2

// applyIssueClauseCoverage checks the reviewer's findings + summary against the
// issue's enumerated behaviour clauses and, for every clause the reviewer never
// addressed, appends a major finding so the clause is surfaced rather than
// omitted. It is a no-op when there are no clauses (an issue with none must
// degrade to a no-op rather than an error) or when extra is nil.
//
// A clause counts as cited when the reviewer's summary or any of its findings
// shares enough of the clause's key tokens; this is the mechanical check that
// complements the coder prompt's checklist. The reviewer's own verdict is
// left untouched. summary is the reviewer's free-form submit_result summary;
// it is a real citation site (it may describe coverage without emitting a
// structured finding per clause).
func applyIssueClauseCoverage(
	log logr.Logger,
	extra map[string]any,
	clauses []string,
	summary string,
) {
	if extra == nil || len(clauses) == 0 {
		return
	}
	findings, _ := reviewer.ParseFindings(extra)
	cited := citedClauses(clauses, findings, summary)
	gaps := unsatisfiedClauses(clauses, cited)
	if len(gaps) == 0 {
		return
	}
	appendClauseFindings(extra, clauses, gaps)
	log.Info("reviewer clause coverage: appended findings for uncited clauses",
		"total", len(clauses), "unsatisfied", len(gaps))
}

// applyIssueClauseCoverageForTask gates applyIssueClauseCoverage to issue-fix
// review runs and resolves the issue body a future call site uses. Extracted
// out of the reviewer block in runLLMPath so the call site there is a single
// statement, keeping that function's cyclomatic complexity budget untouched.
func applyIssueClauseCoverageForTask(
	log logr.Logger,
	task *foremanv1alpha1.AgenticTask,
	loopRes *LoopResult,
) {
	if task.Spec.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
		return
	}
	if loopRes == nil || loopRes.Terminal == nil {
		return
	}
	// The reviewer's fetch_issue body is the source of truth; fall back to the
	// payload prompt when the reviewer never fetched one.
	body := extractFetchIssueBody(loopRes.Transcript)
	if body == "" {
		body = task.Spec.Payload.Prompt
	}
	clauses := extractClauses(body)
	applyIssueClauseCoverage(log, loopRes.Terminal.Extra, clauses, loopRes.Terminal.Summary)
}

// citedClauses builds the clause-index -> citation map that
// unsatisfiedClauses consumes. A clause is cited when the reviewer's summary or
// any of its findings shares enough of the clause's key tokens to show the
// reviewer addressed that behaviour rather than merely echoing a prefix of it.
func citedClauses(clauses []string, findings []reviewer.Finding, summary string) map[int]string {
	cited := make(map[int]string, len(clauses))

	haystacks := make([]string, 0, len(findings)+1)
	for _, f := range findings {
		if f.Message != "" {
			haystacks = append(haystacks, f.Message)
		}
	}
	// The reviewer's free-form summary is a real citation site too: it may
	// describe coverage without emitting a structured finding per clause.
	if strings.TrimSpace(summary) != "" {
		haystacks = append(haystacks, summary)
	}

	for i, clause := range clauses {
		needle := clauseTokens(clause)
		if len(needle) < clauseMatchThreshold {
			// A clause with fewer than two key tokens is too short to
			// match reliably; fall back to a substring check so a very
			// short clause still counts as covered when echoed verbatim.
			trimmed := strings.TrimSpace(clause)
			if trimmed == "" {
				continue
			}
			for _, h := range haystacks {
				if strings.Contains(h, trimmed) {
					cited[i] = "clause echoed in reviewer findings or summary"
					break
				}
			}
			continue
		}
		for _, h := range haystacks {
			if tokenOverlap(h, needle) >= clauseMatchThreshold {
				cited[i] = "clause covered by reviewer findings or summary"
				break
			}
		}
	}
	return cited
}

// clauseTokens splits clause into lower-cased tokens with stopwords and
// non-alphanumeric noise stripped. The result is the set of key terms the
// overlap check compares against the reviewer's text.
func clauseTokens(clause string) []string {
	raw := strings.FieldsFunc(strings.ToLower(clause), nonAlnumRune)
	tokens := make([]string, 0, len(raw))
	for _, t := range raw {
		if clauseStopWords[t] {
			continue
		}
		tokens = append(tokens, t)
	}
	return tokens
}

// tokenOverlap returns how many of clause's key tokens appear (at least once)
// in haystack. It is the mechanical coverage check: a reviewer that paraphrases
// a clause but keeps its key terms still scores above the threshold. A trailing
// 's' is ignored so a clause and its paraphrase agree across singular/plural
// ("clause" matches "clause" and "clauses") without a full stemmer.
func tokenOverlap(haystack string, clauseTokens []string) int {
	words := strings.FieldsFunc(strings.ToLower(haystack), nonAlnumRune)
	hits := 0
	for _, c := range clauseTokens {
		for _, w := range words {
			if formsMatch(c, w) {
				hits++
				break
			}
		}
	}
	return hits
}

// formsMatch reports whether clause token c and haystack word w are the same
// word, ignoring a single trailing 's' that marks the plural. "clause" matches
// "clause" and "clauses"; "clauses" matches "clause" and "clauses".
func formsMatch(c, w string) bool {
	if c == w {
		return true
	}
	if strings.HasSuffix(c, "s") && c[:len(c)-1] == w {
		return true
	}
	if strings.HasSuffix(w, "s") && w[:len(w)-1] == c {
		return true
	}
	return false
}

// nonAlnumRune reports whether r is not an ASCII letter or digit.
func nonAlnumRune(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
}

// appendClauseFindings adds a major finding per unsatisfied clause to
// extra["findings"], preserving the reviewer's existing findings. It rebuilds
// the field from the parsed findings so the stored shape stays uniform
// ([]any) regardless of the shape it arrived in.
func appendClauseFindings(extra map[string]any, clauses []string, gaps []int) {
	combined, _ := reviewer.ParseFindings(extra)
	for _, idx := range gaps {
		combined = append(combined, reviewer.Finding{
			Severity: reviewer.SeverityMajor,
			Area:     reviewer.AreaScope,
			Message:  fmt.Sprintf("issue clause not cited by reviewer: %q", clauses[idx]),
		})
	}
	marshaled, err := json.Marshal(combined)
	if err != nil {
		return
	}
	var asAny []any
	if err := json.Unmarshal(marshaled, &asAny); err != nil {
		return
	}
	extra["findings"] = asAny
}
