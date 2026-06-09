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

package controller

import (
	"fmt"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// escalationSteps decides which escalate-<N>-<j> review steps to emit
// NOW, given the Workload spec and its current children (#546).
//
// For each issue N the trigger is: every base reviewer child
// (step label "review-<N>-<i>") is terminal (Phase Succeeded or
// Failed) AND at least one carries verdict=NO-GO. A cascade
// INCOMPLETE after GATE-FAIL/GATE-ERROR (#548) is terminal but is
// not a NO-GO, so a branch the gate already rejected never burns
// escalation compute. Any existing escalate-<N>-* child suppresses
// re-emission, which makes the rollup-driven call sites idempotent;
// it also means an escalation reviewer's own NO-GO can never trigger
// another round (the tier is exactly two levels deep).
//
// Pure function: no API calls, no status writes. The caller owns
// MaxTasks accounting, sovereignty filtering, and creation.
// Callers must also restrict invocation to issue-batch mode: an
// explicit spec.Pipeline takes precedence over Issues at planning
// time, and user-authored pipeline step names could otherwise
// false-match the review-<N>-* prefixes scanned here.
func escalationSteps(
	w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) (steps []foremanv1alpha1.PipelineStep, escalated []int32) {
	if len(w.Spec.EscalationReviewerAgentRefs) == 0 ||
		len(w.Spec.ReviewerAgentRefs) == 0 ||
		len(w.Spec.Issues) == 0 {
		return nil, nil
	}

	for _, n := range w.Spec.Issues {
		// The trailing dash keeps issue 64 from matching review-641-0.
		basePrefix := fmt.Sprintf("review-%d-", n)
		escPrefix := fmt.Sprintf("escalate-%d-", n)

		var baseTotal, baseTerminal, baseNoGo, escExisting int
		for i := range children {
			step := children[i].Labels[labelStep]
			switch {
			case strings.HasPrefix(step, escPrefix):
				escExisting++
			case strings.HasPrefix(step, basePrefix):
				baseTotal++
				phase := children[i].Status.Phase
				if phase == foremanv1alpha1.AgenticTaskPhaseSucceeded ||
					phase == foremanv1alpha1.AgenticTaskPhaseFailed {
					baseTerminal++
				}
				if children[i].Status.Verdict == foremanv1alpha1.AgenticTaskVerdictNoGo {
					baseNoGo++
				}
			}
		}

		if escExisting > 0 || baseTotal == 0 || baseTerminal < baseTotal || baseNoGo == 0 {
			continue
		}

		branch := fmt.Sprintf("foreman/%s/issue-%d", w.Name, n)
		for j, ref := range w.Spec.EscalationReviewerAgentRefs {
			steps = append(steps, foremanv1alpha1.PipelineStep{
				Name:     fmt.Sprintf("escalate-%d-%d", n, j),
				Kind:     foremanv1alpha1.AgenticTaskKindReview,
				AgentRef: ref,
				Payload: foremanv1alpha1.AgenticTaskPayload{
					Repo:   w.Spec.Repo,
					Issue:  n,
					Branch: branch,
				},
			})
		}
		escalated = append(escalated, n)
	}
	return steps, escalated
}
