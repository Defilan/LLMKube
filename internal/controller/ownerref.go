/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// setControllerReferenceUnblocked sets controlled's owner reference to
// owner (Controller=true) but explicitly clears BlockOwnerDeletion.
//
// Why: controller-runtime's SetControllerReference sets
// BlockOwnerDeletion=true by default. The kube-apiserver
// GarbageCollector admission plugin validates that by looking up the
// owner Kind in its RESTMapper to check the caller's `update`
// permission on the owner's `finalizers` subresource. If the API
// server's discovery cache hasn't populated for a freshly-registered
// CRD yet, the lookup fails and Create is rejected with:
//
//	deployments.apps "foo" is forbidden: cannot set blockOwnerDeletion
//	in this case because cannot find RESTMapping for APIVersion ...
//	Kind InferenceService: no matches for kind "InferenceService"
//
// On kind that race window is microseconds and we never see it. On
// MicroShift-in-MINC the in-container apiserver populates discovery
// lazily on first request and the window is wide enough to trip every
// first-reconcile Create. The same race could in principle hit any
// fresh-install path; bundling the fix here means it won't.
//
// We don't actually need BlockOwnerDeletion. Cascading delete still
// cleans up operator-managed children when their parent CR is
// deleted; the "block" semantics only matter for finalizer-based
// cleanup workflows LLMKube does not use today. Trade: cleaner
// bootstrap for slightly looser cascade ordering guarantees we
// weren't relying on.
func setControllerReferenceUnblocked(
	owner, controlled metav1.Object,
	scheme *runtime.Scheme,
) error {
	if err := controllerutil.SetControllerReference(owner, controlled, scheme); err != nil {
		return err
	}
	refs := controlled.GetOwnerReferences()
	for i := range refs {
		if refs[i].UID == owner.GetUID() {
			falseVal := false
			refs[i].BlockOwnerDeletion = &falseVal
		}
	}
	controlled.SetOwnerReferences(refs)
	return nil
}
