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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// TestTaskOutcomeLabels verifies the bounded label extraction the operator uses
// to emit terminal-task outcome metrics (#1491): agent name from spec.agentRef,
// kind, verdict, and the machine outcome / elapsedSec / turns carried in the
// result envelope. A missing or unparseable result yields empty outcome and
// zero numeric fields, never a fabricated value. The task NAME is deliberately
// never returned — it is unbounded and would break cardinality.
func TestTaskOutcomeLabels(t *testing.T) {
	t.Run("full envelope with agentRef", func(t *testing.T) {
		task := &foremanv1alpha1.AgenticTask{}
		task.Spec.Kind = foremanv1alpha1.AgenticTaskKindIssueFix
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: "coder"}
		task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		task.Status.Result = &runtime.RawExtension{Raw: []byte(
			`{"elapsedSec":42.5,"extra":{"outcome":"ALREADY-RESOLVED","turnCount":17}}`)}

		agent, kind, verdict, outcome, elapsed, turns := taskOutcomeLabels(task)
		if agent != "coder" {
			t.Errorf("agent = %q, want %q", agent, "coder")
		}
		if kind != "issue-fix" {
			t.Errorf("kind = %q, want %q", kind, "issue-fix")
		}
		if verdict != "NO-GO" {
			t.Errorf("verdict = %q, want %q", verdict, "NO-GO")
		}
		if outcome != "ALREADY-RESOLVED" {
			t.Errorf("outcome = %q, want %q", outcome, "ALREADY-RESOLVED")
		}
		if elapsed != 42.5 {
			t.Errorf("elapsed = %f, want 42.5", elapsed)
		}
		if turns != 17 {
			t.Errorf("turns = %d, want 17", turns)
		}
	})

	t.Run("no agentRef and no result envelope", func(t *testing.T) {
		task := &foremanv1alpha1.AgenticTask{}
		task.Spec.Kind = foremanv1alpha1.AgenticTaskKindVerify
		task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGatePass

		agent, kind, verdict, outcome, elapsed, turns := taskOutcomeLabels(task)
		if agent != "" {
			t.Errorf("agent = %q, want empty for unset agentRef", agent)
		}
		if kind != "verify" {
			t.Errorf("kind = %q, want %q", kind, "verify")
		}
		if verdict != "GATE-PASS" {
			t.Errorf("verdict = %q, want %q", verdict, "GATE-PASS")
		}
		if outcome != "" || elapsed != 0 || turns != 0 {
			t.Errorf("missing envelope must yield empty/zero, got outcome=%q elapsed=%f turns=%d",
				outcome, elapsed, turns)
		}
	})

	t.Run("unparseable result yields no fabricated values", func(t *testing.T) {
		task := &foremanv1alpha1.AgenticTask{}
		task.Spec.Kind = foremanv1alpha1.AgenticTaskKindFreeform
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: "freeform-agent"}
		task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictIncomplete
		task.Status.Result = &runtime.RawExtension{Raw: []byte(`{not-json`)}

		_, _, _, outcome, elapsed, turns := taskOutcomeLabels(task)
		if outcome != "" || elapsed != 0 || turns != 0 {
			t.Errorf("unparseable envelope must yield empty/zero, got outcome=%q elapsed=%f turns=%d",
				outcome, elapsed, turns)
		}
	})
}
