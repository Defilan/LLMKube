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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// newStatusBuilderReconciler builds a minimal InferenceServiceReconciler backed
// by a fake client for unit-testing updateStatusWithSchedulingInfo without
// standing up envtest.
func newStatusBuilderReconciler(t *testing.T, isvc *inferencev1alpha1.InferenceService) *InferenceServiceReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithObjects(isvc).
		Build()
	return &InferenceServiceReconciler{
		Client: c,
		Scheme: scheme,
	}
}

// seedReadyAvailable writes a stale Available: True condition onto the
// InferenceService, simulating a service that was previously Ready. The
// status update is persisted so updateStatusWithSchedulingInfo reads it back.
func seedReadyAvailable(t *testing.T, ctx context.Context, c client.Client, isvc *inferencev1alpha1.InferenceService) {
	t.Helper()
	isvc.Status.Phase = PhaseReady
	isvc.Status.ReadyReplicas = 1
	isvc.Status.DesiredReplicas = 1
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             "InferenceReady",
		Message:            "Inference service is ready and serving requests",
	})
	if err := c.Status().Update(ctx, isvc); err != nil {
		t.Fatalf("seed status update: %v", err)
	}
}

// TestUpdateStatusStoppedClearsStaleAvailable verifies that a previously-Ready
// InferenceService scaled to zero (Phase=Stopped) reports Available: False
// with reason ManuallyScaledToZero, rather than keeping the stale Available:
// True from the Ready era. This mirrors the metal-agent's markStopped
// convention so the two status writers agree.
func TestUpdateStatusStoppedClearsStaleAvailable(t *testing.T) {
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "stopped-isvc", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(0),
		},
	}
	r := newStatusBuilderReconciler(t, isvc)
	seedReadyAvailable(t, ctx, r.Client, isvc)

	if _, err := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseStopped, false, 0, 0, "", "", nil); err != nil {
		t.Fatalf("updateStatusWithSchedulingInfo: %v", err)
	}

	got := &inferencev1alpha1.InferenceService{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Status.Phase != PhaseStopped {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseStopped)
	}

	cond := meta.FindStatusCondition(got.Status.Conditions, "Available")
	if cond == nil {
		t.Fatal("Available condition not set on Stopped InferenceService")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Available condition status = %q, want False", cond.Status)
	}
	if cond.Reason != "ManuallyScaledToZero" {
		t.Errorf("Available condition reason = %q, want ManuallyScaledToZero", cond.Reason)
	}
}

// TestUpdateStatusSuspendedClearsStaleAvailable verifies that a previously-Ready
// InferenceService suspended (Phase=Suspended) reports Available: False with
// reason Suspended, rather than keeping the stale Available: True from the
// Ready era. This mirrors the metal-agent's markStopped convention so the two
// status writers agree.
func TestUpdateStatusSuspendedClearsStaleAvailable(t *testing.T) {
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "suspended-isvc", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(2),
			Suspend:  true,
		},
	}
	r := newStatusBuilderReconciler(t, isvc)
	seedReadyAvailable(t, ctx, r.Client, isvc)

	if _, err := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseSuspended, false, 0, 0, "", "", nil); err != nil {
		t.Fatalf("updateStatusWithSchedulingInfo: %v", err)
	}

	got := &inferencev1alpha1.InferenceService{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Status.Phase != PhaseSuspended {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseSuspended)
	}

	cond := meta.FindStatusCondition(got.Status.Conditions, "Available")
	if cond == nil {
		t.Fatal("Available condition not set on Suspended InferenceService")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Available condition status = %q, want False", cond.Status)
	}
	if cond.Reason != "Suspended" {
		t.Errorf("Available condition reason = %q, want Suspended", cond.Reason)
	}
}

// TestUpdateStatusStoppedAndSuspendedReasonsDiffer asserts the two scaled-away
// phases cannot silently collapse into one: Stopped and Suspended must use
// distinct reasons so downstream observers can tell them apart.
func TestUpdateStatusStoppedAndSuspendedReasonsDiffer(t *testing.T) {
	ctx := context.Background()

	stopped := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "stopped-isvc", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(0),
		},
	}
	rStopped := newStatusBuilderReconciler(t, stopped)
	seedReadyAvailable(t, ctx, rStopped.Client, stopped)
	if _, err := rStopped.updateStatusWithSchedulingInfo(ctx, stopped, PhaseStopped, false, 0, 0, "", "", nil); err != nil {
		t.Fatalf("stopped updateStatusWithSchedulingInfo: %v", err)
	}

	suspended := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "suspended-isvc", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "some-model",
			Replicas: ptrInt32(2),
			Suspend:  true,
		},
	}
	rSuspended := newStatusBuilderReconciler(t, suspended)
	seedReadyAvailable(t, ctx, rSuspended.Client, suspended)
	if _, err := rSuspended.updateStatusWithSchedulingInfo(ctx, suspended, PhaseSuspended, false, 0, 0, "", "", nil); err != nil {
		t.Fatalf("suspended updateStatusWithSchedulingInfo: %v", err)
	}

	stoppedCond := meta.FindStatusCondition(stopped.Status.Conditions, "Available")
	suspendedCond := meta.FindStatusCondition(suspended.Status.Conditions, "Available")
	if stoppedCond == nil || suspendedCond == nil {
		t.Fatal("both Available conditions must be set")
	}
	if stoppedCond.Reason == suspendedCond.Reason {
		t.Errorf("Stopped and Suspended reasons collapsed to %q; they must differ", stoppedCond.Reason)
	}
}
