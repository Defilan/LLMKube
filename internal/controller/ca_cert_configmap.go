/*
Copyright 2026.

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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Custom CA ConfigMap preflight.
//
// --ca-cert-configmap names a ConfigMap that addCACertVolume mounts into every
// model downloader init container. A ConfigMap volume resolves in the POD's
// namespace, so the ConfigMap has to exist in every namespace that runs a
// workload, not only the operator's. When it is missing the kubelet cannot
// mount it: the pod sits in ContainerCreating with FailedMount events, holding
// the node resources (a GPU) it was scheduled with, while the InferenceService
// reports nothing but Creating.
//
// The volume deliberately stays non-optional. An optional volume would let the
// downloader start without the CA, and every download from the private
// endpoint the flag exists for would then fail TLS verification, one layer
// further from the cause. Instead the controller checks the ConfigMap before it
// creates a workload that mounts it, holds the create while it is missing, and
// says so on the object (condition, Warning event, Progressing message).

const (
	// ConditionCACertConfigMapAvailable reports whether the operator's custom
	// CA ConfigMap exists in the object's namespace. Present only while a
	// rendered workload mounts it: absent when --ca-cert-configmap is unset or
	// the workload stages no download (pvc://, a skipped init).
	ConditionCACertConfigMapAvailable = "CACertConfigMapAvailable"

	// ReasonCACertConfigMapMissing: the ConfigMap is not in the namespace, so
	// the workload is not created.
	ReasonCACertConfigMapMissing = "CACertConfigMapMissing"

	// ReasonCACertConfigMapFound: the ConfigMap is in the namespace.
	ReasonCACertConfigMapFound = "CACertConfigMapFound"

	// caCertConfigMapMissingRequeue backstops recovery on a blocked object.
	// The InferenceService controller also watches the ConfigMap and reacts
	// at once; the Model controller watches nothing but Models, so for a
	// blocked prefetch this requeue is what notices the ConfigMap appearing.
	caCertConfigMapMissingRequeue = time.Minute
)

// podSpecMountsConfigMap reports whether spec carries a non-optional volume
// sourced from the named ConfigMap, the only kind the kubelet refuses to start
// a pod without. It inspects the rendered spec rather than the inputs that
// produced it, so every storage path (cached, emptyDir, multi-file, draft
// model, prefetch) is covered without mirroring its source-type dispatch.
func podSpecMountsConfigMap(spec *corev1.PodSpec, name string) bool {
	if spec == nil || name == "" {
		return false
	}
	for _, v := range spec.Volumes {
		cm := v.ConfigMap
		if cm == nil || cm.Name != name {
			continue
		}
		if cm.Optional != nil && *cm.Optional {
			continue
		}
		return true
	}
	return false
}

// caCertConfigMapMissing reports whether a workload with this pod spec in this
// namespace would wedge on the operator's CA ConfigMap: spec mounts it and it
// does not exist. Any read error other than NotFound is returned, so a
// transient API failure never reads as "missing".
func caCertConfigMapMissing(ctx context.Context, c client.Reader, name, namespace string, spec *corev1.PodSpec) (required, missing bool, err error) {
	if !podSpecMountsConfigMap(spec, name) {
		return false, false, nil
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return true, true, nil
		}
		return true, false, fmt.Errorf("reading CA ConfigMap %s/%s: %w", namespace, name, err)
	}
	return true, false, nil
}

// caCertConfigMapMissingMessage is the condition, event, and Progressing text.
// It names the ConfigMap, the namespace, and the fix, because the namespace is
// the part users get wrong: the chart takes the name, and the pods resolve it
// next to themselves.
func caCertConfigMapMissingMessage(name, namespace string) string {
	return fmt.Sprintf("CA ConfigMap %q (operator flag --ca-cert-configmap) not found in namespace %q; "+
		"the model downloader mounts it from the workload's own namespace, so no pod is created until it exists. "+
		"Copy it there, e.g. kubectl -n %s create configmap %s --from-file=ca.crt=<ca.pem>; "+
		"the workload is created automatically once it appears",
		name, namespace, namespace, name)
}

// setCACertConfigMapCondition records the preflight outcome on conds and
// reports whether the False condition is new (a transition), so the caller
// emits one Warning event per episode rather than one per requeue.
func setCACertConfigMapCondition(conds *[]metav1.Condition, generation int64, name, namespace string, required, missing bool) (transitioned bool) {
	if !required {
		meta.RemoveStatusCondition(conds, ConditionCACertConfigMapAvailable)
		return false
	}
	if missing {
		transitioned = !meta.IsStatusConditionFalse(*conds, ConditionCACertConfigMapAvailable)
		meta.SetStatusCondition(conds, metav1.Condition{
			Type:               ConditionCACertConfigMapAvailable,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			Reason:             ReasonCACertConfigMapMissing,
			Message:            caCertConfigMapMissingMessage(name, namespace),
		})
		return transitioned
	}
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               ConditionCACertConfigMapAvailable,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             ReasonCACertConfigMapFound,
		Message:            fmt.Sprintf("CA ConfigMap %q found in namespace %q", name, namespace),
	})
	return false
}

// reconcileCACertConfigMap runs the preflight for an InferenceService workload
// whose rendered pod spec is spec, updates the condition in memory (the
// caller's status write persists it), and emits a Warning event on the
// transition to missing. It returns whether the ConfigMap is missing; the
// caller decides whether that blocks (a create) or only informs (an existing
// workload, whose running pods already have the volume mounted).
func (r *InferenceServiceReconciler) reconcileCACertConfigMap(ctx context.Context, isvc *inferencev1alpha1.InferenceService, spec *corev1.PodSpec) (bool, error) {
	required, missing, err := caCertConfigMapMissing(ctx, r.Client, r.CACertConfigMap, isvc.Namespace, spec)
	if err != nil {
		return false, err
	}
	if setCACertConfigMapCondition(&isvc.Status.Conditions, isvc.Generation, r.CACertConfigMap, isvc.Namespace, required, missing) && r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, ReasonCACertConfigMapMissing, "Reconcile",
			"%s", caCertConfigMapMissingMessage(r.CACertConfigMap, isvc.Namespace))
	}
	return missing, nil
}

// waitForCACertConfigMap persists the blocked state: phase Pending with the
// fix in the Progressing message, alongside the condition set by
// reconcileCACertConfigMap.
func (r *InferenceServiceReconciler) waitForCACertConfigMap(ctx context.Context, isvc *inferencev1alpha1.InferenceService, modelReady bool, desiredReplicas int32) (*ctrl.Result, error) {
	result, err := r.updateStatusWithSchedulingInfo(ctx, isvc, "Pending", modelReady, 0, isvc.Status.Replicas, desiredReplicas, "",
		caCertConfigMapMissingMessage(r.CACertConfigMap, isvc.Namespace), nil)
	result.RequeueAfter = earliestPositive(result.RequeueAfter, caCertConfigMapMissingRequeue)
	return &result, err
}

// findInferenceServicesForCACertConfigMap enqueues every InferenceService in
// the namespace of the operator's CA ConfigMap when it changes, so a service
// blocked on it is created the moment the ConfigMap is copied in, and a
// deletion surfaces on running services without waiting for a resync. Other
// ConfigMaps map to nothing.
func (r *InferenceServiceReconciler) findInferenceServicesForCACertConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	if r.CACertConfigMap == "" || obj.GetName() != r.CACertConfigMap {
		return nil
	}
	var list inferencev1alpha1.InferenceServiceList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
		}})
	}
	return requests
}
