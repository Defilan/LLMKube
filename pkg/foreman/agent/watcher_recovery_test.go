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
	"context"
	"strings"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func newRecoveryClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		Build()
}

func runningTask(name, node string) *foremanv1alpha1.AgenticTask {
	now := metav1.Now()
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase:        foremanv1alpha1.AgenticTaskPhaseRunning,
			AssignedNode: node,
			ClaimedAt:    &now,
			StartedAt:    &now,
		},
	}
}

func getTask(t *testing.T, c client.Client, name string) foremanv1alpha1.AgenticTask {
	t.Helper()
	var got foremanv1alpha1.AgenticTask
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return got
}

// A task left Running by a dead PID on this node must be reset to Pending
// with assignedNode/claimedAt/startedAt cleared and a recovery condition,
// so the scheduler re-dispatches it. Regression for defilantech/LLMKube#542.
func TestRecoverOrphanedTasks_ResetsRunningTaskOnThisNode(t *testing.T) {
	c := newRecoveryClient(t, runningTask("code-531", "m5max-coder"))
	w := &AgenticTaskWatcher{Client: c, NodeName: "m5max-coder", Namespace: "default"}

	if err := w.recoverOrphanedTasks(context.Background(), "default"); err != nil {
		t.Fatalf("recoverOrphanedTasks: %v", err)
	}

	got := getTask(t, c, "code-531")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhasePending {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}
	if got.Status.AssignedNode != "" {
		t.Fatalf("assignedNode = %q, want cleared", got.Status.AssignedNode)
	}
	if got.Status.ClaimedAt != nil || got.Status.StartedAt != nil {
		t.Fatalf("claimedAt=%v startedAt=%v, want both cleared", got.Status.ClaimedAt, got.Status.StartedAt)
	}
	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Reason == "AgentRestartRecovery" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an AgentRestartRecovery condition, got %+v", got.Status.Conditions)
	}
}

// A Running task assigned to a different node is not this agent's to
// recover; leave it untouched.
func TestRecoverOrphanedTasks_IgnoresOtherNode(t *testing.T) {
	c := newRecoveryClient(t, runningTask("other-node-task", "some-other-node"))
	w := &AgenticTaskWatcher{Client: c, NodeName: "m5max-coder", Namespace: "default"}

	if err := w.recoverOrphanedTasks(context.Background(), "default"); err != nil {
		t.Fatalf("recoverOrphanedTasks: %v", err)
	}

	got := getTask(t, c, "other-node-task")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseRunning {
		t.Fatalf("phase = %q, want Running (untouched)", got.Status.Phase)
	}
}

// Tasks on this node that are not Running (e.g. Scheduled) must not be
// disturbed: only orphaned in-flight work is recovered.
func TestRecoverOrphanedTasks_IgnoresNonRunningPhase(t *testing.T) {
	scheduled := runningTask("scheduled-task", "m5max-coder")
	scheduled.Status.Phase = foremanv1alpha1.AgenticTaskPhaseScheduled
	c := newRecoveryClient(t, scheduled)
	w := &AgenticTaskWatcher{Client: c, NodeName: "m5max-coder", Namespace: "default"}

	if err := w.recoverOrphanedTasks(context.Background(), "default"); err != nil {
		t.Fatalf("recoverOrphanedTasks: %v", err)
	}

	got := getTask(t, c, "scheduled-task")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseScheduled {
		t.Fatalf("phase = %q, want Scheduled (untouched)", got.Status.Phase)
	}
}

// If the apiserver rejects the primary terminal-status patch (e.g. because a
// future contract drift produces an enum value not yet in the CRD), the watcher
// must NOT leave the task wedged in Running forever. It must fall back to a
// minimal valid status: Phase=Failed, Verdict=INCOMPLETE,
// FailureReason=InfrastructureError, and a "TerminalPatchRejected" condition
// whose message includes the original bogus verdict string.
//
// Regression for defilantech/LLMKube#649.
func TestPatchTerminal_FallsBackOnRejectedVerdict(t *testing.T) {
	task := pendingTask("code-649")

	// Build a real fake client that holds the task, then wrap it in an
	// interceptor that rejects the FIRST SubResourcePatch with an Invalid
	// error (simulating apiserver enum validation rejection), then lets the
	// second patch (the fallback) pass through to the underlying client.
	base := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(task).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		Build()

	var patchCalls atomic.Int32
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, c client.Client, subResourceName string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			n := patchCalls.Add(1)
			if n == 1 {
				// Simulate apiserver rejecting the patch because "BOGUS" is not in
				// the verdict enum.
				return apierrors.NewInvalid(
					schema.GroupKind{Group: "foreman.llmkube.dev", Kind: "AgenticTask"},
					obj.GetName(),
					nil,
				)
			}
			// Let the fallback patch go through.
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	})

	w := &AgenticTaskWatcher{Client: c, NodeName: "coder", Namespace: "default"}

	bogusResult := &Result{
		SchemaVersion: ResultSchemaVersion,
		Kind:          "issue-fix",
		Verdict:       foremanv1alpha1.AgenticTaskVerdict("BOGUS"),
		Summary:       "contract drift test",
	}

	// patchTerminal must not return an error: the fallback patch should succeed.
	if err := w.patchTerminal(context.Background(), task, bogusResult, nil); err != nil {
		t.Fatalf("patchTerminal returned error: %v", err)
	}

	// The underlying fake client holds the real state; read from it directly.
	got := getTask(t, base, "code-649")

	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", got.Status.Verdict)
	}
	if got.Status.FailureReason != foremanv1alpha1.FailureInfrastructureError {
		t.Fatalf("failureReason = %q, want InfrastructureError", got.Status.FailureReason)
	}
	if got.Status.FinishedAt == nil {
		t.Fatal("finishedAt is nil, want non-nil on fallback outcome")
	}
	if got.Status.Result == nil {
		t.Fatal("result is nil, want non-nil on fallback outcome")
	}

	var rejCond *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Reason == "TerminalPatchRejected" {
			rejCond = &got.Status.Conditions[i]
			break
		}
	}
	if rejCond == nil {
		t.Fatalf("expected a TerminalPatchRejected condition, got %+v", got.Status.Conditions)
	}
	if rejCond.Status != metav1.ConditionFalse {
		t.Fatalf("TerminalPatchRejected condition status = %q, want False", rejCond.Status)
	}

	// The message must contain the original bogus verdict so an operator can
	// correlate logs with the task object without needing to grep the agent log.
	const bogusVerdict = "BOGUS"
	if msg := rejCond.Message; len(msg) == 0 {
		t.Fatal("TerminalPatchRejected condition message is empty")
	} else if !strings.Contains(msg, bogusVerdict) {
		t.Fatalf("TerminalPatchRejected message %q does not contain original verdict %q", msg, bogusVerdict)
	}
}
