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
// side: it checks whether the reviewer's findings actually cited each clause,
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

// clauseCiteLen bounds how much of a clause is matched against the reviewer's
// text. A clause is long enough that a verbatim echo of the whole thing is not
// required; matching a distinctive prefix catches the reviewer quoting the
// clause (or a paraphrase that keeps its key phrase) without tripping on
// light rewording.
const clauseCiteLen = 60

// applyIssueClauseCoverage checks the reviewer's findings + summary against the
// issue's enumerated behaviour clauses and, for every clause the reviewer never
// addressed, appends a major finding so the clause is surfaced rather than
// omitted. It is a no-op when there are no clauses (an issue with none must
// degrade to a no-op rather than an error) or when extra is nil.
//
// A clause counts as cited when the reviewer's summary or any of its findings
// echoes a distinctive phrase of the clause text; this is the mechanical check
// that complements the coder prompt's checklist. The reviewer's own verdict
// is left untouched.
func applyIssueClauseCoverage(
	log logr.Logger,
	extra map[string]any,
	clauses []string,
) {
	if extra == nil || len(clauses) == 0 {
		return
	}
	findings, _ := reviewer.ParseFindings(extra)
	cited := citedClauses(clauses, findings, extra)
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
	applyIssueClauseCoverage(log, loopRes.Terminal.Extra, clauses)
}

// citedClauses builds the clause-index -> citation map that
// unsatisfiedClauses consumes. A clause is cited when the reviewer's summary or
// any of its findings echoes a distinctive phrase of the clause text.
func citedClauses(clauses []string, findings []reviewer.Finding, extra map[string]any) map[int]string {
	cited := make(map[int]string, len(clauses))

	haystacks := make([]string, 0, len(findings)+1)
	for _, f := range findings {
		if f.Message != "" {
			haystacks = append(haystacks, strings.ToLower(f.Message))
		}
	}
	// The reviewer's free-form summary is a real citation site too: it may
	// describe coverage without emitting a structured finding per clause.
	if summary, ok := extra["reviewOutcome"].(string); ok && strings.TrimSpace(summary) != "" {
		haystacks = append(haystacks, strings.ToLower(summary))
	}

	for i, clause := range clauses {
		needle := strings.TrimSpace(clause)
		if needle == "" {
			continue
		}
		if len(needle) > clauseCiteLen {
			needle = needle[:clauseCiteLen]
		}
		needle = strings.TrimSpace(strings.ToLower(needle))
		if needle == "" {
			continue
		}
		for _, h := range haystacks {
			if strings.Contains(h, needle) {
				cited[i] = "clause echoed in reviewer findings or summary"
				break
			}
		}
	}
	return cited
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
