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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestDeadlineReconcilerCancelsHungReconcile pins the reason the deadline
// exists (#2069): a reconcile whose call never returns must observe
// cancellation and must surface an error, so the workqueue requeues instead
// of holding the worker forever.
func TestDeadlineReconcilerCancelsHungReconcile(t *testing.T) {
	inner := &deadlineTestReconciler{wait: 5 * time.Second}
	d := &DeadlineReconciler{Inner: inner, Timeout: 50 * time.Millisecond, Log: logr.Discard()}

	_, err := d.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "isvc"},
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline-exceeded error, got %v", err)
	}
	if !errors.Is(inner.sawErr, context.DeadlineExceeded) {
		t.Fatalf("inner reconciler did not observe the deadline cancellation, saw %v", inner.sawErr)
	}
}

// TestDeadlineReconcilerSurfacesDeadlineWhenInnerSwallowsIt covers an inner
// that returns cleanly (nil error) after the deadline fired: the wrapper must
// still turn the aborted reconcile into an error, or the object silently
// stays unreconciled with no retry.
func TestDeadlineReconcilerSurfacesDeadlineWhenInnerSwallowsIt(t *testing.T) {
	inner := &deadlineTestReconciler{wait: 5 * time.Second, swallow: true}
	d := &DeadlineReconciler{Inner: inner, Timeout: 50 * time.Millisecond, Log: logr.Discard()}

	_, err := d.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "isvc"},
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the wrapper to surface the deadline, got %v", err)
	}
}

// TestDeadlineReconcilerPassesThroughOnSuccess covers the happy path: a
// reconcile that finishes within the deadline keeps its result and error.
func TestDeadlineReconcilerPassesThroughOnSuccess(t *testing.T) {
	inner := &deadlineTestReconciler{wait: 10 * time.Millisecond, result: reconcile.Result{RequeueAfter: time.Minute}}
	d := &DeadlineReconciler{Inner: inner, Timeout: time.Minute, Log: logr.Discard()}

	res, err := d.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "isvc"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != time.Minute {
		t.Fatalf("expected the inner result to pass through, got %+v", res)
	}
}

// TestDeadlineReconcilerDisabledWithZeroTimeout pins the off switch: a zero
// timeout must not cancel a slow-but-finite reconcile.
func TestDeadlineReconcilerDisabledWithZeroTimeout(t *testing.T) {
	inner := &deadlineTestReconciler{wait: 200 * time.Millisecond, result: reconcile.Result{RequeueAfter: time.Minute}}
	d := &DeadlineReconciler{Inner: inner, Timeout: 0, Log: logr.Discard()}

	res, err := d.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "isvc"},
	})
	if err != nil {
		t.Fatalf("unexpected error with the deadline disabled: %v", err)
	}
	if res.RequeueAfter != time.Minute {
		t.Fatalf("expected the inner result to pass through, got %+v", res)
	}
	if inner.sawErr != nil {
		t.Fatalf("inner saw cancellation %v with the deadline disabled", inner.sawErr)
	}
}

// TestDeadlineReconcilerKeepsParentCancellationUntouched pins the polarity of
// the error relabeling: a manager-shutdown cancellation must not be relabeled
// as a deadline miss.
func TestDeadlineReconcilerKeepsParentCancellationUntouched(t *testing.T) {
	inner := &deadlineTestReconciler{swallow: true}
	d := &DeadlineReconciler{Inner: inner, Timeout: time.Minute, Log: logr.Discard()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := d.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "isvc"},
	})
	if err != nil {
		t.Fatalf("expected a parent cancellation to pass through untouched, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatal("expected no requeue for a cancelled reconcile")
	}
}

// deadlineTestReconciler blocks for wait (or until its context is done) and
// records which error, if any, its context ended with.
type deadlineTestReconciler struct {
	wait    time.Duration
	swallow bool
	result  reconcile.Result
	sawErr  error
}

func (r *deadlineTestReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	select {
	case <-ctx.Done():
		r.sawErr = ctx.Err()
		if r.swallow {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, ctx.Err()
	case <-time.After(r.wait):
		return r.result, nil
	}
}
