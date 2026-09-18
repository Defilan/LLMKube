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
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// drainExecutor blocks in Execute until either release is closed (it then
// returns a GO result) or ctx is cancelled (it records the cancellation and
// returns ctx.Err()). The block is what holds a turn in flight across the
// drain, and the cancelled flag is what distinguishes "drained to completion"
// from "aborted by liveness".
type drainExecutor struct {
	release   chan struct{}
	cancelled *atomic.Bool
}

func (e *drainExecutor) Kind() string { return "drain-test" }

func (e *drainExecutor) Execute(
	ctx context.Context,
	task *foremanv1alpha1.AgenticTask,
	_ *foremanv1alpha1.Agent,
) (*Result, error) {
	select {
	case <-e.release:
		return NewResult(
			e.Kind(),
			foremanv1alpha1.AgenticTaskVerdictGo,
			"finished during drain",
			time.Millisecond,
		), nil
	case <-ctx.Done():
		e.cancelled.Store(true)
		return nil, ctx.Err()
	}
}

// waitForTask polls the task's status until want(status) is true, or the
// deadline elapses. The drain tests need it to observe the poll loop's claim
// (Scheduled -> Running) before they trigger the drain.
func waitForTask(
	t *testing.T, c client.Client, name string,
	want func(foremanv1alpha1.AgenticTaskStatus) bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got foremanv1alpha1.AgenticTask
		if err := c.Get(
			context.Background(),
			types.NamespacedName{Namespace: "default", Name: name}, &got,
		); err == nil && want(got.Status) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for task %s to reach the wanted phase", name)
}

// A SIGTERM arriving while an in-process task is already claimed must NOT
// kill the run mid-patch. The watcher stops claiming, blocks on the in-flight
// execution, lets it run to a terminal result and patch it inside the
// termination grace window, then returns. Before the fix the run's context
// inherited the drain cancellation, so the kubelet's force-kill (or the
// process exiting) would drop the work and re-queue it on claim expiry.
//
// Regression for defilantech/LLMKube#1438.
func TestRun_DrainsInFlightTaskOnSigterm(t *testing.T) {
	c := newRecoveryClient(t, scheduledTask(
		"drain-go", "m5max-coder", foremanv1alpha1.AgenticTaskKindIssueFix, time.Minute,
	))
	exe := &drainExecutor{release: make(chan struct{}), cancelled: &atomic.Bool{}}
	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "m5max-coder",
		Namespace:            "default",
		Executor:             exe,
		Interval:             10 * time.Millisecond,
		TaskLivenessInterval: time.Hour, // never fires before the run ends; the task is not deleted here
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait until the poll loop has claimed the task and launched the run.
	waitForTask(t, c, "drain-go", func(s foremanv1alpha1.AgenticTaskStatus) bool {
		return s.Phase == foremanv1alpha1.AgenticTaskPhaseRunning
	})

	// SIGTERM: the watcher stops claiming and begins draining.
	cancel()

	// Let the in-flight turn finish. Its context is detached from the drained
	// ctx, so completing it now patches the terminal status DURING the drain.
	close(exe.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the in-flight turn drained")
	}

	if exe.cancelled.Load() {
		t.Fatal("executor was cancelled during the drain; want it to run to completion")
	}
	got := getTask(t, c, "drain-go")
	if got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (terminal patched during the drain)", got.Status.Phase)
	}
}

// Deleting the task out from under an in-flight run during a drain must still
// abort the run: the liveness watchdog runs on the detached run context, so it
// survives the drain and cancels the executor, and the terminal patch stands
// down (the task is already gone). The drain still completes and Run returns.
//
// Regression for defilantech/LLMKube#1438 (liveness must be preserved by the
// drain); the abort itself is #1136.
func TestRun_DrainStandsDownWhenTaskDeleted(t *testing.T) {
	const taskName = "drain-deleted"
	c := newRecoveryClient(t, scheduledTask(taskName, "m5max-coder", foremanv1alpha1.AgenticTaskKindIssueFix, time.Minute))
	exe := &drainExecutor{release: make(chan struct{}), cancelled: &atomic.Bool{}}
	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "m5max-coder",
		Namespace:            "default",
		Executor:             exe,
		Interval:             10 * time.Millisecond,
		TaskLivenessInterval: 10 * time.Millisecond, // detect the deletion fast
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait until the poll loop has claimed the task and launched the run.
	waitForTask(t, c, taskName, func(s foremanv1alpha1.AgenticTaskStatus) bool {
		return s.Phase == foremanv1alpha1.AgenticTaskPhaseRunning
	})

	// SIGTERM: the watcher stops claiming and begins draining.
	cancel()

	// Delete the task out from under the in-flight run (kubectl delete
	// workload GC-ing the child task).
	var task foremanv1alpha1.AgenticTask
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Namespace: "default", Name: taskName}, &task,
	); err != nil {
		t.Fatalf("get task before delete: %v", err)
	}
	if err := c.Delete(context.Background(), &task); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	// The run is cancelled by liveness, the patch stands down, and the drain
	// completes. Run must return.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the in-flight run was cancelled by liveness")
	}

	if !exe.cancelled.Load() {
		t.Fatal("executor was NOT cancelled on task deletion; liveness must still abort the run during a drain")
	}
	var after foremanv1alpha1.AgenticTask
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Namespace: "default", Name: taskName}, &after,
	); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted task Get = %v, want NotFound (no terminal patch on a deleted task)", err)
	}
}

// A SIGTERM that lands while a poll pass is ALREADY looping over its listed
// candidates must stop the pass from claiming any candidate beyond the one it
// has started. The pass listed both tasks before the cancel, so without the
// per-candidate drain check it would claim the second one too and launch a run
// on a node that is about to be torn down; with it, the pass falls out at the
// first sign of the drain, the first run goes in-flight (and drains under its
// detached context), and the second stays Scheduled for the next node.
//
// Regression for defilantech/LLMKube#1438.
func TestPollOnce_StopsClaimingCandidatesAfterCancel(t *testing.T) {
	first := taskForAgent("first", "coder", time.Hour) // older, so claimed first
	second := taskForAgent("second", "coder", time.Minute)
	// Both job-mode, so after the first is launched the supervision budget
	// (default 4) still has room: the ONLY thing holding the second back is
	// the drain check, not a capacity skip. A capacity-based skip would pass
	// this test even without the fix.
	exec := newModeExecutor(t, map[string]bool{"coder": true})

	base := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(first, second, agentCR("coder", true)).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		Build()

	// Pin the pass at the first candidate's claim: signal when that patch is
	// reached, then block until the test cancels ctx and releases. This lands
	// the cancel deterministically mid-pass -- after the first is claimed and
	// launched, before the second is considered -- with no timing to race.
	reached := make(chan struct{})
	release := make(chan struct{})
	var claims atomic.Int32
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, cl client.Client, subResourceName string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			if claims.Add(1) == 1 {
				close(reached)
				<-release
			}
			return cl.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	})

	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "node-1",
		Namespace:            "default",
		Executor:             exec,
		TaskLivenessInterval: time.Hour, // no watchdog interference
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.pollOnce(ctx, "default") }()

	// The pass has reached the first candidate's claim.
	<-reached
	// SIGTERM mid-pass: the node is draining.
	cancel()
	// Let the first candidate's claim land and its run go in-flight.
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pollOnce returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pollOnce did not return after the drain")
	}

	// The first candidate was claimed and its run is in-flight (draining).
	if got := getTask(t, w.Client, "first"); got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseRunning {
		t.Fatalf("first phase = %q, want Running (claimed and in-flight)", got.Status.Phase)
	}
	exec.waitStarted(t, 1)
	// The second, already-scheduled candidate must NOT have been claimed.
	if got := getTask(t, w.Client, "second"); got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseScheduled {
		t.Fatalf("second phase = %q, want Scheduled (not claimed after the cancel)", got.Status.Phase)
	}
}

// The per-candidate drain check at the top of the loop is not enough on its
// own: between it and the status patch sit the Agent resolve (an uncached
// apiserver GET) and the capacity check, so a SIGTERM can land after the
// check passed and the candidate still gets claimed onto a node that is about
// to go away. The re-check immediately before claim closes that gap.
//
// The cancel is pinned deterministically at the boundary: the two candidates
// carry different agentRefs, so the second candidate's Agent resolve is not
// memoized and its Get is the last client call before its claim. Cancelling
// inside that Get — after the second candidate's top-of-loop check ran —
// leaves the cancellation landed with only the re-check standing between here
// and the claim. Without the re-check the second candidate is claimed (the
// fake client, like a real apiserver mid-flight, does not veto a patch whose
// client context has just been cancelled) and the claim-patch interceptor
// observes it arriving on a cancelled context.
//
// Regression for defilantech/LLMKube#1438.
func TestPollOnce_RechecksCancellationBeforeClaim(t *testing.T) {
	first := taskForAgent("first", "coder-a", time.Hour) // older, so claimed first
	second := taskForAgent("second", "coder-b", time.Minute)
	// Both job-mode, so the supervision budget (default 4) still has room for
	// the second after the first launches: only the drain check can stop it.
	exec := newModeExecutor(t, map[string]bool{"coder-a": true, "coder-b": true})

	base := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(first, second, agentCR("coder-a", true), agentCR("coder-b", true)).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		Build()

	ctx, cancel := context.WithCancel(context.Background())
	var cancelledClaims atomic.Int32
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			err := cl.Get(ctx, key, obj, opts...)
			// The pass has moved to the second candidate: its Agent Get is
			// the last thing before its claim, so SIGTERM lands exactly at
			// the check-then-claim boundary.
			if a, ok := obj.(*foremanv1alpha1.Agent); ok && a.Name == "coder-b" {
				cancel()
			}
			return err
		},
		SubResourcePatch: func(
			ctx context.Context, cl client.Client, subResourceName string,
			obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			// A claim attempted on the cancelled drain: the very thing the
			// re-check must prevent.
			if ctx.Err() != nil {
				cancelledClaims.Add(1)
			}
			return cl.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	})

	w := &AgenticTaskWatcher{
		Client:               c,
		NodeName:             "node-1",
		Namespace:            "default",
		Executor:             exec,
		TaskLivenessInterval: time.Hour, // no watchdog interference
	}

	if err := w.pollOnce(ctx, "default"); err != nil {
		t.Fatalf("pollOnce returned error: %v", err)
	}

	// The first candidate was claimed before the drain and is in-flight.
	if got := getTask(t, w.Client, "first"); got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseRunning {
		t.Fatalf("first phase = %q, want Running (claimed before the drain)", got.Status.Phase)
	}
	exec.waitStarted(t, 1)
	// The cancellation landed after the second candidate's top-of-loop check:
	// it must still stay Scheduled for the next node.
	if got := getTask(t, w.Client, "second"); got.Status.Phase != foremanv1alpha1.AgenticTaskPhaseScheduled {
		t.Fatalf(
			"second phase = %q, want Scheduled (claim must not start after a cancel at the claim boundary)",
			got.Status.Phase,
		)
	}
	if n := cancelledClaims.Load(); n != 0 {
		t.Fatalf("%d claim patch attempt(s) on a cancelled context, want 0 (the pre-claim re-check must stop the pass)", n)
	}
}
