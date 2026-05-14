/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestSetControllerReferenceUnblocked verifies that the helper sets a
// controller owner reference but clears BlockOwnerDeletion. Without
// BlockOwnerDeletion cleared, the API server's GarbageCollector
// admission plugin tries to look up the owner Kind via RESTMapper to
// validate the caller's permission on the owner's `finalizers`
// subresource. That lookup races against fresh-CRD discovery on
// MicroShift and rejects every first-reconcile Create until the
// API server's discovery cache catches up.
func TestSetControllerReferenceUnblocked(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}

	owner := &inferencev1alpha1.InferenceService{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "inference.llmkube.dev/v1alpha1",
			Kind:       "InferenceService",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owner-isvc",
			Namespace: "ns",
			UID:       types.UID("owner-uid-1234"),
		},
	}
	controlled := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "owned-deploy", Namespace: "ns"},
	}

	if err := setControllerReferenceUnblocked(owner, controlled, scheme); err != nil {
		t.Fatalf("setControllerReferenceUnblocked: %v", err)
	}

	refs := controlled.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("want 1 owner ref, got %d: %+v", len(refs), refs)
	}
	ref := refs[0]
	if ref.UID != owner.UID {
		t.Errorf("owner UID = %q, want %q", ref.UID, owner.UID)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Errorf("Controller = %v, want true", ref.Controller)
	}
	if ref.BlockOwnerDeletion == nil {
		t.Error("BlockOwnerDeletion is nil; want explicitly *false")
	} else if *ref.BlockOwnerDeletion {
		t.Errorf("BlockOwnerDeletion = true, want false (avoids MicroShift RESTMapper bootstrap race)")
	}
}
