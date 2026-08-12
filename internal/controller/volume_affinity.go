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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Emitted by the scheduler's VolumeBinding PreBind plugin when a pod's bound PV
// carries a node affinity that the chosen node does not satisfy.
// The scheduler reports the same underlying fault with two different wordings,
// depending on where the pod was in its lifecycle when the unusable PV appeared.
//
//   - PreBind: the pod had already been assigned a node, so binding is what
//     fails. Names the PV and the node.
//   - Filter: the pod had not been scheduled yet, so the volume's affinity is
//     just another predicate that no node satisfies. Names neither.
//
// The Filter form is what a freshly created InferenceService produces, and it
// was missed by the first cut of this code, which only knew the PreBind form.
const (
	volumeBindingPreBindHint = `running PreBind plugin "VolumeBinding"`
	volumeAffinityHint       = "node affinity doesn't match node"
	volumeAffinityFilterHint = "didn't match PersistentVolume's node affinity"
)

// detectUnbindableVolume recognises a pod that cannot start because one of its
// PersistentVolumes has a node affinity matching no node, and returns the PV
// name and the node the scheduler had chosen.
//
// This is worth special-casing because the scheduler's own wording sends you to
// the wrong place. It names a node and says affinity does not match, which reads
// like the pod is mis-scheduled -- but the scheduler picked correctly and the
// PVC's selected-node annotation agrees with it. The fault is upstream: the
// provisioner never stamped the volume's affinity, so the PV was published with
// an unsatisfiable term (in practice `kubernetes.io/hostname In [""]`).
//
// The trigger seen in the field is a provisioner whose helper pod cannot
// schedule onto the target node -- microk8s-hostpath spawns a helper carrying
// only the two default NoExecute tolerations, so any NoSchedule taint (a GPU
// taint, for instance) blocks provisioning while still creating a PV. See
// defilantech/LLMKube#1509.
func detectUnbindableVolume(message string) (pvName string, nodeName string, ok bool) {
	if strings.Contains(message, volumeAffinityHint) {
		// Require the PreBind context too, so an unrelated message that happens
		// to quote this phrase does not get reclassified.
		if !strings.Contains(message, volumeBindingPreBindHint) && !strings.Contains(message, "binding volumes") {
			return "", "", false
		}
		pvName = quotedAfter(message, `pv `)
		nodeName = quotedAfter(message[strings.Index(message, volumeAffinityHint):], volumeAffinityHint+` `)
		return pvName, nodeName, true
	}

	// Filter form carries no names; the caller resolves the PV from the pod.
	if strings.Contains(message, volumeAffinityFilterHint) {
		return "", "", true
	}

	return "", "", false
}

// unsatisfiableNodeAffinity reports whether pv carries a node affinity that no
// node can satisfy, which is the fingerprint of a volume the provisioner
// created but never stamped: `kubernetes.io/hostname In [""]`.
//
// This is what separates the provisioning bug from an ordinary mistake. A PV
// legitimately pinned to one node while the pod is pinned to another produces
// the same scheduler wording, but its values are real hostnames, and telling
// that user their provisioner failed would send them somewhere useless.
func unsatisfiableNodeAffinity(pv *corev1.PersistentVolume) bool {
	na := pv.Spec.NodeAffinity
	if na == nil || na.Required == nil || len(na.Required.NodeSelectorTerms) == 0 {
		return false
	}

	for _, term := range na.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			// An In with no values, or whose only values are empty strings,
			// can never match: node label values are non-empty.
			if len(expr.Values) == 0 {
				return true
			}
			allEmpty := true
			for _, v := range expr.Values {
				if strings.TrimSpace(v) != "" {
					allEmpty = false
					break
				}
			}
			if allEmpty {
				return true
			}
		}
	}

	return false
}

// quotedAfter returns the double-quoted token immediately following prefix, or
// "" when prefix is absent or is not followed by a quoted token. Kept local
// rather than reaching for a regexp: the scheduler's format is fixed, and the
// failure mode we care about is "no match", not partial parsing.
func quotedAfter(s, prefix string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// podModelCacheClaim returns the PVC name backing the pod's model cache, or ""
// when the pod has no claim-backed volume. The scheduler's message names the PV
// (a generated pvc-<uuid> string), which is not something anyone recognises;
// the claim name is.
func podModelCacheClaim(pod *corev1.Pod) string {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// unbindableVolumeMessage explains the failure in terms of the thing the user
// can act on. The scheduler blames node affinity; this names the claim, the
// volume and the node, and says provisioning is what actually failed.
func unbindableVolumeMessage(pvName, nodeName, claimName string) string {
	var b strings.Builder
	b.WriteString("model cache volume was never provisioned: ")
	if pvName != "" {
		b.WriteString(fmt.Sprintf("PersistentVolume %q ", pvName))
	} else {
		b.WriteString("the bound PersistentVolume ")
	}
	b.WriteString("has a node affinity that matches no node")
	if nodeName != "" {
		b.WriteString(fmt.Sprintf(", so the pod cannot start on %q", nodeName))
	}
	b.WriteString(".")

	if claimName != "" {
		b.WriteString(fmt.Sprintf(" Claim: %s.", claimName))
	}

	b.WriteString(" The scheduler chose the node correctly; the storage provisioner" +
		" failed to stamp the volume, which usually means its helper pod could not" +
		" schedule onto that node (for example, a taint the provisioner does not" +
		" tolerate). Delete the claim and use a storage class whose provisioner can" +
		" run there.")

	return b.String()
}

// confirmUnbindableModelCache resolves the pod's claim to its PersistentVolume
// and reports the PV name when that volume can never be scheduled anywhere.
//
// Returning ok=false is the safe outcome: the caller then leaves the scheduler's
// own message alone rather than asserting a cause it has not established. That
// covers the PVC that has no volume yet (provisioning still in flight) and the
// genuinely mis-pinned PV, neither of which is this bug.
func (r *InferenceServiceReconciler) confirmUnbindableModelCache(ctx context.Context, pod *corev1.Pod) (pvName string, ok bool) {
	claimName := podModelCacheClaim(pod)
	if claimName == "" {
		return "", false
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: claimName}, pvc); err != nil {
		return "", false
	}
	if pvc.Spec.VolumeName == "" {
		return "", false
	}

	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return "", false
	}
	if !unsatisfiableNodeAffinity(pv) {
		return "", false
	}

	return pv.Name, true
}

// classifyUnbindableVolume decides whether a pending pod's scheduling failure is
// the never-provisioned-cache case, combining the scheduler's wording with the
// state of the volume itself.
//
// The message alone is not enough. The Filter wording carries no PV name and is
// equally produced by a PV that is legitimately pinned to another node, so the
// PV is read to confirm the fingerprint before the operator asserts a cause. The
// PreBind wording does name the volume, but is confirmed the same way so both
// paths make the same claim on the same evidence.
func (r *InferenceServiceReconciler) classifyUnbindableVolume(ctx context.Context, pod *corev1.Pod, message string) (pvName string, nodeName string, ok bool) {
	msgPV, msgNode, matched := detectUnbindableVolume(message)
	if !matched {
		return "", "", false
	}

	confirmedPV, confirmed := r.confirmUnbindableModelCache(ctx, pod)
	if !confirmed {
		return "", "", false
	}

	if msgPV != "" {
		confirmedPV = msgPV
	}
	if msgNode == "" {
		msgNode = pod.Spec.NodeName
	}

	return confirmedPV, msgNode, true
}
