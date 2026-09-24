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

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// errReader is a client.Reader whose Get always fails with a non-NotFound
// error, standing in for a transient API failure during the confirmation.
type errReader struct{ err error }

func (e errReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return e.err
}

func (e errReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}

// laggedWorkload wires the informer-lag window: the cache-backed List has
// observed only one of the Workload's two planned children, while both exist
// in the API. uncached is the reader the reconciler uses to confirm absence.
// It returns the reconciler and the cache-backed client, so a test can read
// the status the reconciler patched back.
func laggedWorkload(t *testing.T, uncached client.Reader) (*WorkloadReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := foremanv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add foreman scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	child := func(name string) *foremanv1alpha1.AgenticTask {
		return &foremanv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{labelWorkload: "lag-wl"},
			},
			Status: foremanv1alpha1.AgenticTaskStatus{
				Phase: foremanv1alpha1.AgenticTaskPhasePending,
			},
		}
	}

	wl := &foremanv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "lag-wl", Namespace: "default"},
		Status: foremanv1alpha1.WorkloadStatus{
			Tasks: []corev1.ObjectReference{{Name: "code-1"}, {Name: "verify-1"}},
		},
	}

	// The cache has caught up to verify-1 only; code-1's create has not
	// reached the informer yet.
	cached := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(wl, child("verify-1")).WithStatusSubresource(wl).Build()
	r := &WorkloadReconciler{Client: cached, Scheme: scheme, APIReader: uncached}
	return r, cached
}

// TestReconcile_PlannedChildConfirmedViaUncachedReaderNotReportedMissing is
// the #1744 fix at the seam: a planned child the cache has not observed yet
// must be confirmed against the uncached reader before the Dispatched
// condition reports it, so the report does not fire during informer lag.
func TestReconcile_PlannedChildConfirmedViaUncachedReaderNotReportedMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := foremanv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add foreman scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	// The API knows about both children.
	uncached := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			&foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "code-1", Namespace: "default"},
			},
		).Build()

	r, cached := laggedWorkload(t, uncached)
	key := client.ObjectKey{Namespace: "default", Name: "lag-wl"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var fresh foremanv1alpha1.Workload
	if err := cached.Get(context.Background(), key, &fresh); err != nil {
		t.Fatalf("get workload after reconcile: %v", err)
	}
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, conditionTypeDispatched)
	if cond == nil {
		t.Fatal("Dispatched condition not emitted")
	}
	if cond.Reason == "ChildrenMissing" {
		t.Errorf("condition reports ChildrenMissing for a child the API confirms exists: %q", cond.Message)
	}
	if fresh.Status.Phase != foremanv1alpha1.WorkloadPhaseDispatched {
		t.Errorf("phase = %q, want %q", fresh.Status.Phase, foremanv1alpha1.WorkloadPhaseDispatched)
	}
}

// TestConfirmMissing_ReaderErrorIsNotReported: a reader that cannot prove
// absence must not turn a shortfall into a report. Any error other than
// NotFound means "unconfirmed", never "missing".
func TestConfirmMissing_ReaderErrorIsNotReported(t *testing.T) {
	wl := &foremanv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: "err-wl", Namespace: "default"}}
	r := &WorkloadReconciler{APIReader: errReader{err: errors.New("connection refused")}}

	got := r.confirmMissing(context.Background(), wl, []string{"code-1"})
	if len(got) != 0 {
		t.Errorf("a non-NotFound reader error must not report a missing child; got %v", got)
	}
}

// TestConfirmMissing_NilReaderPreservesBehaviour: the reconciler is built
// directly in many tests without an API reader, so a nil reader must skip
// the confirmation and pass the suspects through rather than panic.
func TestConfirmMissing_NilReaderPreservesBehaviour(t *testing.T) {
	wl := &foremanv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: "nil-wl", Namespace: "default"}}
	r := &WorkloadReconciler{}

	got := r.confirmMissing(context.Background(), wl, []string{"code-1", "verify-1"})
	if len(got) != 2 || got[0] != "code-1" || got[1] != "verify-1" {
		t.Errorf("nil reader must return the suspects unchanged; got %v", got)
	}
}
