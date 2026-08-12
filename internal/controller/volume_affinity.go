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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Emitted by the scheduler's VolumeBinding PreBind plugin when a pod's bound PV
// carries a node affinity that the chosen node does not satisfy.
const (
	volumeBindingPreBindHint = `running PreBind plugin "VolumeBinding"`
	volumeAffinityHint       = "node affinity doesn't match node"
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
	if !strings.Contains(message, volumeAffinityHint) {
		return "", "", false
	}
	// Require the PreBind context too, so an unrelated message that happens to
	// quote this phrase does not get reclassified.
	if !strings.Contains(message, volumeBindingPreBindHint) && !strings.Contains(message, "binding volumes") {
		return "", "", false
	}

	pvName = quotedAfter(message, `pv `)
	nodeName = quotedAfter(message[strings.Index(message, volumeAffinityHint):], volumeAffinityHint+` `)

	return pvName, nodeName, true
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
