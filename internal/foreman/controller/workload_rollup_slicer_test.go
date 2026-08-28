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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// TestClassifyChildren_SlicerVerdicts locks in that a sliced Workload's
// integrate and reconcile steps roll up through the same verdict-based
// classification as every other kind (#1033). The rollup is deliberately
// kind-agnostic: a Succeeded + GATE-FAIL reconcile (pinned interface drift) or
// integrate (overlap / stale-base apply) lands in the incomplete bucket, which
// keeps the Workload out of Completed; a clean GATE-PASS counts as succeeded.
// If a future change special-cases kinds in classifyChildren, this fails.
func TestClassifyChildren_SlicerVerdicts(t *testing.T) {
	mk := func(kind foremanv1alpha1.AgenticTaskKind, verdict foremanv1alpha1.AgenticTaskVerdict) foremanv1alpha1.AgenticTask {
		return foremanv1alpha1.AgenticTask{
			Spec:   foremanv1alpha1.AgenticTaskSpec{Kind: kind},
			Status: foremanv1alpha1.AgenticTaskStatus{Phase: foremanv1alpha1.AgenticTaskPhaseSucceeded, Verdict: verdict},
		}
	}

	tests := []struct {
		name           string
		task           foremanv1alpha1.AgenticTask
		wantSucceeded  int32
		wantIncomplete int32
	}{
		{"integrate clean", mk(foremanv1alpha1.AgenticTaskKindIntegrate, foremanv1alpha1.AgenticTaskVerdictGatePass), 1, 0},
		{"integrate overlap", mk(foremanv1alpha1.AgenticTaskKindIntegrate, foremanv1alpha1.AgenticTaskVerdictGateFail), 0, 1},
		{"reconcile clean", mk(foremanv1alpha1.AgenticTaskKindReconcile, foremanv1alpha1.AgenticTaskVerdictGatePass), 1, 0},
		{"reconcile drift", mk(foremanv1alpha1.AgenticTaskKindReconcile, foremanv1alpha1.AgenticTaskVerdictGateFail), 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := classifyChildren([]foremanv1alpha1.AgenticTask{tc.task})
			if c.succeeded != tc.wantSucceeded || c.incomplete != tc.wantIncomplete {
				t.Fatalf("classifyChildren = {succeeded:%d incomplete:%d}, want {succeeded:%d incomplete:%d}",
					c.succeeded, c.incomplete, tc.wantSucceeded, tc.wantIncomplete)
			}
		})
	}
}

// withContradictionsEnvelope builds a complete result envelope that
// carries the given cross-stage contradiction strings under
// extra.crossStageContradictions (the shape the detector writes).
func withContradictionsEnvelope(contradictions ...string) *runtime.RawExtension {
	parts := make([]string, len(contradictions))
	for i, c := range contradictions {
		parts[i] = `"` + c + `"`
	}
	j := `{"summary":"","extra":{"crossStageContradictions":[` + strings.Join(parts, ",") + `]}}`
	return &runtime.RawExtension{Raw: []byte(j)}
}

// firstOrEmpty returns the first element of a slice or "" when empty.
func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// TestClassifyChildren_CrossStageContradiction locks in the rollup bucket
// for cross-stage contradictions (#1685). A contradiction is orthogonal to
// success: a Succeeded child that also carries contradictions counts in
// BOTH the succeeded and the contradicted buckets, never in the failed or
// incomplete buckets. Asserts the contradicted count independently of the
// other buckets, and asserts that a contradicting-but-Succeeded task still
// counts as succeeded.
func TestClassifyChildren_CrossStageContradiction(t *testing.T) {
	withContradiction := func(verdict foremanv1alpha1.AgenticTaskVerdict, contradictions ...string) foremanv1alpha1.AgenticTask {
		return foremanv1alpha1.AgenticTask{
			Status: foremanv1alpha1.AgenticTaskStatus{
				Phase:   foremanv1alpha1.AgenticTaskPhaseSucceeded,
				Verdict: verdict,
				Result:  withContradictionsEnvelope(contradictions...),
			},
		}
	}
	failedWithContradiction := func(contradictions ...string) foremanv1alpha1.AgenticTask {
		return foremanv1alpha1.AgenticTask{
			Status: foremanv1alpha1.AgenticTaskStatus{
				Phase:  foremanv1alpha1.AgenticTaskPhaseFailed,
				Result: withContradictionsEnvelope(contradictions...),
			},
		}
	}

	tests := []struct {
		name             string
		tasks            []foremanv1alpha1.AgenticTask
		wantSucceeded    int32
		wantIncomplete   int32
		wantFailed       int32
		wantContradicted int32
		wantFirstContra  string
	}{
		{
			name:             "no contradiction",
			tasks:            []foremanv1alpha1.AgenticTask{{Status: foremanv1alpha1.AgenticTaskStatus{Phase: foremanv1alpha1.AgenticTaskPhaseSucceeded, Verdict: foremanv1alpha1.AgenticTaskVerdictGatePass}}},
			wantSucceeded:    1,
			wantContradicted: 0,
		},
		{
			name:             "one contradiction on a Succeeded task",
			tasks:            []foremanv1alpha1.AgenticTask{withContradiction(foremanv1alpha1.AgenticTaskVerdictGatePass, "coder claims edits on empty branch")},
			wantSucceeded:    1,
			wantContradicted: 1,
			wantFirstContra:  "coder claims edits on empty branch",
		},
		{
			name:             "one contradiction on a Failed task",
			tasks:            []foremanv1alpha1.AgenticTask{failedWithContradiction("gate passed on empty branch")},
			wantFailed:       1,
			wantContradicted: 1,
			wantFirstContra:  "gate passed on empty branch",
		},
		{
			name:             "unparseable result",
			tasks:            []foremanv1alpha1.AgenticTask{{Status: foremanv1alpha1.AgenticTaskStatus{Phase: foremanv1alpha1.AgenticTaskPhaseSucceeded, Verdict: foremanv1alpha1.AgenticTaskVerdictGatePass, Result: &runtime.RawExtension{Raw: []byte("{not json")}}}},
			wantSucceeded:    1,
			wantContradicted: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := classifyChildren(tc.tasks)
			if c.succeeded != tc.wantSucceeded {
				t.Errorf("succeeded = %d, want %d", c.succeeded, tc.wantSucceeded)
			}
			if c.incomplete != tc.wantIncomplete {
				t.Errorf("incomplete = %d, want %d", c.incomplete, tc.wantIncomplete)
			}
			if c.failed != tc.wantFailed {
				t.Errorf("failed = %d, want %d", c.failed, tc.wantFailed)
			}
			// The contradicted count stands on its own, independent of
			// the terminal buckets above.
			if c.contradicted != tc.wantContradicted {
				t.Errorf("contradicted = %d, want %d", c.contradicted, tc.wantContradicted)
			}
			if tc.wantFirstContra != "" {
				if len(c.contradictions) == 0 || c.contradictions[0] != tc.wantFirstContra {
					t.Errorf("contradictions[0] = %q, want %q", firstOrEmpty(c.contradictions), tc.wantFirstContra)
				}
			}
		})
	}
}

// TestEmitCrossStageContradictionCondition verifies both arms of
// emitCrossStageContradictionCondition (#1685). Zero contradictions leaves
// the condition present with Status=False and Reason=NoContradictions. One
// or more contradictions sets Status=True, Reason=CrossStageContradiction,
// and the message names the count and the first contradiction string.
func TestEmitCrossStageContradictionCondition(t *testing.T) {
	r := &WorkloadReconciler{}
	now := metav1.Now()

	cond := func(w *foremanv1alpha1.Workload) *metav1.Condition {
		for i := range w.Status.Conditions {
			if w.Status.Conditions[i].Type == conditionTypeCrossStageContradiction {
				return &w.Status.Conditions[i]
			}
		}
		return nil
	}

	// Zero contradictions: condition present, Status=False,
	// Reason=NoContradictions.
	w := &foremanv1alpha1.Workload{}
	r.emitCrossStageContradictionCondition(w, childCounts{}, now)
	c := cond(w)
	if c == nil {
		t.Fatalf("CrossStageContradiction condition not present")
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Status = %q, want %q", c.Status, metav1.ConditionFalse)
	}
	if c.Reason != "NoContradictions" {
		t.Errorf("Reason = %q, want %q", c.Reason, "NoContradictions")
	}

	// One contradiction: Status=True, Reason=CrossStageContradiction,
	// message names the count and first contradiction string.
	w = &foremanv1alpha1.Workload{}
	r.emitCrossStageContradictionCondition(w, childCounts{
		contradicted:   1,
		contradictions: []string{"coder claims edits on empty branch"},
	}, now)
	c = cond(w)
	if c == nil {
		t.Fatalf("CrossStageContradiction condition not present")
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Status = %q, want %q", c.Status, metav1.ConditionTrue)
	}
	if c.Reason != "CrossStageContradiction" {
		t.Errorf("Reason = %q, want %q", c.Reason, "CrossStageContradiction")
	}
	if !strings.Contains(c.Message, "1 child task(s) recorded a cross-stage contradiction") {
		t.Errorf("message does not name the count: %q", c.Message)
	}
	if !strings.Contains(c.Message, "coder claims edits on empty branch") {
		t.Errorf("message does not contain the first contradiction: %q", c.Message)
	}

	// Two contradictions: count reflects both, message carries the first.
	w = &foremanv1alpha1.Workload{}
	r.emitCrossStageContradictionCondition(w, childCounts{
		contradicted: 2,
		contradictions: []string{
			"gate passed on empty branch",
			"reviewer claims a changed line",
		},
	}, now)
	c = cond(w)
	if c == nil {
		t.Fatalf("CrossStageContradiction condition not present")
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Status = %q, want %q", c.Status, metav1.ConditionTrue)
	}
	if !strings.Contains(c.Message, "2 child task(s) recorded a cross-stage contradiction") {
		t.Errorf("message does not name the count: %q", c.Message)
	}
	if !strings.Contains(c.Message, "gate passed on empty branch") {
		t.Errorf("message does not contain the first contradiction: %q", c.Message)
	}
	if strings.Contains(c.Message, "reviewer claims a changed line") {
		t.Errorf("message should name only the first contradiction: %q", c.Message)
	}
}

// TestRollup_ContradictedTasksAndCondition drives rollup end-to-end over
// children where one carries a cross-stage contradiction, and asserts the
// two things a survivor of mutation testing would miss: that the counted
// bucket actually reaches Workload.status.ContradictedTasks (mutation B),
// and that emitCrossStageContradictionCondition is actually wired into
// rollup (mutation C). Deleting either production line makes an assertion
// here fail.
func TestRollup_ContradictedTasksAndCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := foremanv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add foreman scheme: %v", err)
	}
	// corev1 must be registered too: rollup patches a Workload and the
	// fake client needs the Workload's status subresource registered.
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	wl := &foremanv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "contradiction-wl", Namespace: "default"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(wl).WithStatusSubresource(wl).Build()

	r := &WorkloadReconciler{Client: cl, Scheme: scheme}

	// One child Succeeded on target (counts as succeeded) AND carries a
	// cross-stage contradiction (counts as contradicted). The contradiction
	// is orthogonal to success, so it lands in BOTH buckets.
	contradicting := foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "contradiction-wl-code-1",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: foremanv1alpha1.GroupVersion.String(),
				Kind:       "Workload",
				Name:       "contradiction-wl",
			}},
		},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase:   foremanv1alpha1.AgenticTaskPhaseSucceeded,
			Verdict: foremanv1alpha1.AgenticTaskVerdictGatePass,
			Result:  withContradictionsEnvelope("coder claims edits on empty branch"),
		},
	}
	if err := cl.Create(context.Background(), &contradicting); err != nil {
		t.Fatalf("create contradicting task: %v", err)
	}

	ctx := context.Background()
	children := []foremanv1alpha1.AgenticTask{contradicting}
	if _, err := r.rollup(ctx, wl, children); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	// Mutation B: the counted bucket must reach the status field.
	if wl.Status.ContradictedTasks != 1 {
		t.Errorf("w.Status.ContradictedTasks = %d, want 1", wl.Status.ContradictedTasks)
	}
	// And success is unaffected: the task still counts as succeeded.
	if wl.Status.SucceededTasks != 1 {
		t.Errorf("w.Status.SucceededTasks = %d, want 1", wl.Status.SucceededTasks)
	}

	// Mutation C: the condition must actually be emitted by rollup.
	cond := apimeta.FindStatusCondition(wl.Status.Conditions, conditionTypeCrossStageContradiction)
	if cond == nil {
		t.Fatal("CrossStageContradiction condition not emitted by rollup")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition Status = %q, want %q", cond.Status, metav1.ConditionTrue)
	}
	if cond.Reason != "CrossStageContradiction" {
		t.Errorf("condition Reason = %q, want %q", cond.Reason, "CrossStageContradiction")
	}
	if !strings.Contains(cond.Message, "coder claims edits on empty branch") {
		t.Errorf("condition message does not name the first contradiction: %q", cond.Message)
	}
}
