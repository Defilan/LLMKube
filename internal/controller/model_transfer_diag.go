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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Model-transfer diagnosis. The sibling of the CUDA driver/runtime diagnosis in
// driver_compat.go, aimed at the containers that actually fail on a fresh
// deploy: the init containers that fetch the weights.
//
// The engine crashing is diagnosed there and deliberately scans only the
// runtime container. This scans only the init containers, for the same reason
// in reverse: a curl failure in the engine would be a different problem, and
// one failure must never collect two contradictory diagnoses.
//
// Without this, a transfer failure surfaces as a Pod wedged in Init with a
// non-zero exit code and nothing naming the cause. The operator already knows
// the cause; it is in the termination message, which every generated init
// container now reports (#1437).

const (
	// ConditionModelTransferHealthy reports whether the init containers that
	// stage model weights completed. Diagnostic only: it never feeds phase or
	// the Available condition. Absent until a failure is first diagnosed;
	// True once a subsequent pod gets past its init containers.
	ConditionModelTransferHealthy = "ModelTransferHealthy"

	// ReasonModelSourceUnauthorized: the source rejected our credentials
	// (401/403). Usually a missing or wrong sourceSecretRef, or a signature
	// the object store did not accept.
	ReasonModelSourceUnauthorized = "ModelSourceUnauthorized"

	// ReasonModelSourceNotFound: the source returned 404. Usually a wrong
	// bucket, key, or file name in spec.source.
	ReasonModelSourceNotFound = "ModelSourceNotFound"

	// ReasonModelSourceUntrusted: TLS verification failed. Typically a private
	// endpoint signed by an internal CA that the downloader does not trust,
	// which is what --ca-cert-configmap exists to fix.
	ReasonModelSourceUntrusted = "ModelSourceUntrusted"

	// ReasonModelSourceUnreachable: DNS or connection failure reaching the
	// source. A wrong endpoint, a NetworkPolicy, or an egress-blocked cluster.
	ReasonModelSourceUnreachable = "ModelSourceUnreachable"

	// ReasonModelTransferOutOfSpace: the write ran out of room. Node
	// ephemeral storage, or an emptyDir sizeLimit smaller than the model.
	ReasonModelTransferOutOfSpace = "ModelTransferOutOfSpace"

	// ReasonModelTransferSucceeded is set when ModelTransferHealthy flips back
	// to True: a pod has since got past its init containers.
	ReasonModelTransferSucceeded = "ModelTransferSucceeded"
)

// modelTransferSignature pairs a diagnosis with the substrings that justify it.
// Matching is case-insensitive, so entries must be lowercase.
type modelTransferSignature struct {
	reason  string
	remedy  string
	matches []string
}

// modelTransferSignatures are matched in order, so the more specific entries
// come first. Every entry is a failure observed in practice; a signature that
// cannot be tied to a real message does not belong here, because a confident
// wrong label is worse than the bare exit code the user already has.
var modelTransferSignatures = []modelTransferSignature{
	{
		reason: ReasonModelSourceUntrusted,
		remedy: "the endpoint's CA is not trusted by the downloader; " +
			"publish it via the operator's --ca-cert-configmap (the ConfigMap must exist " +
			"in the workload's namespace, not the operator's)",
		// Both curl spellings: 8.x prints "SSL certificate OpenSSL verify
		// result: ...", older builds print "SSL certificate problem: ...".
		// Verified against curl 8.18 (the pinned init image) hitting a private
		// endpoint with no CA bundle.
		matches: []string{
			"ssl certificate problem",
			"ssl certificate openssl verify result",
			"unable to get local issuer certificate",
			"self signed certificate",
			"certificate verify failed",
		},
	},
	{
		reason: ReasonModelSourceUnauthorized,
		remedy: "check spec.sourceSecretRef and the credentials it names",
		matches: []string{
			"returned error: 401",
			"returned error: 403",
			"403 forbidden",
			"401 unauthorized",
			"signaturedoesnotmatch",
			"invalidaccesskeyid",
		},
	},
	{
		reason:  ReasonModelSourceNotFound,
		remedy:  "check spec.source: the bucket, key or file name does not exist at the endpoint",
		matches: []string{"returned error: 404", "404 not found", "nosuchkey", "nosuchbucket"},
	},
	{
		reason: ReasonModelTransferOutOfSpace,
		remedy: "raise spec.resources.ephemeralStorage (which also sizes the emptyDir) " +
			"or give the model cache a larger volume",
		matches: []string{"no space left on device", "disk quota exceeded"},
	},
	{
		reason: ReasonModelSourceUnreachable,
		remedy: "check the endpoint address, cluster DNS, and any NetworkPolicy or egress restriction",
		matches: []string{
			"could not resolve host",
			"failed to connect to",
			"connection refused",
			"connection timed out",
		},
	},
}

// matchModelTransferFailure classifies an init container's termination message.
// Returns the reason, the (sanitized, length-capped) line that justified it,
// and whether anything matched at all.
//
// Reuses driver_compat.go's asciiLower and sanitizeMessageLine: the input is
// arbitrary container output, so offsets must stay byte-stable and the result
// must be bounded no matter what was printed.
func matchModelTransferFailure(msg string) (reason, matchedLine string, found bool) {
	if msg == "" {
		return "", "", false
	}
	lower := asciiLower(msg)
	for _, sig := range modelTransferSignatures {
		for _, m := range sig.matches {
			idx := strings.Index(lower, m)
			if idx < 0 {
				continue
			}
			// Report the whole line the match sits on, not the fragment: the
			// surrounding text is what makes the diagnosis actionable (which
			// URL, which host, which file).
			start := strings.LastIndexByte(msg[:idx], '\n') + 1
			end := strings.IndexByte(msg[idx:], '\n')
			if end < 0 {
				end = len(msg)
			} else {
				end += idx
			}
			return sig.reason, sanitizeMessageLine(msg[start:end]), true
		}
	}
	return "", "", false
}

// isGeneratedInitContainer reports whether a container name is one the operator
// generates for model staging. Scoped by name rather than by position so an
// init container injected by a mesh or a policy controller is never diagnosed
// as our transfer failing.
func isGeneratedInitContainer(name string) bool {
	return name == "model-downloader" || name == "model-cache-prep"
}

// findModelTransferFailure scans pods for a generated init container that
// terminated non-zero with a recognisable cause. Both the current terminated
// state (fresh failure, before backoff) and the last terminated state
// (CrashLoopBackOff) are checked, mirroring findCudaDriverCrash.
func findModelTransferFailure(pods []corev1.Pod) (nodeName, reason, matchedLine string, found bool) {
	for i := range pods {
		pod := &pods[i]
		for j := range pod.Status.InitContainerStatuses {
			cs := &pod.Status.InitContainerStatuses[j]
			if !isGeneratedInitContainer(cs.Name) {
				continue
			}
			for _, term := range []*corev1.ContainerStateTerminated{
				cs.State.Terminated,
				cs.LastTerminationState.Terminated,
			} {
				if term == nil || term.ExitCode == 0 {
					continue
				}
				if r, line, ok := matchModelTransferFailure(term.Message); ok {
					return pod.Spec.NodeName, r, line, true
				}
			}
		}
	}
	return "", "", "", false
}

// initContainersCompleted reports whether any pod has got past every generated
// init container. The recovery gate: weights staged successfully somewhere is
// the only observation that justifies clearing the diagnosis.
func initContainersCompleted(pods []corev1.Pod) bool {
	for i := range pods {
		seen, ok := false, true
		for _, cs := range pods[i].Status.InitContainerStatuses {
			if !isGeneratedInitContainer(cs.Name) {
				continue
			}
			seen = true
			if cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 0 {
				ok = false
			}
		}
		if seen && ok {
			return true
		}
	}
	return false
}

// reconcileModelTransferCondition diagnoses model-staging failures onto the
// InferenceService. Mirrors reconcileDriverCompatCondition, including its
// transition-only recovery: pods merely disappearing never asserts health we
// have not observed.
func (r *InferenceServiceReconciler) reconcileModelTransferCondition(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService,
) {
	log := logf.FromContext(ctx)

	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if err := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); err != nil {
		log.Error(err, "Failed to list pods for model-transfer diagnosis")
		return
	}

	now := metav1.NewTime(time.Now())
	nodeName, reason, matchedLine, found := findModelTransferFailure(podList.Items)

	if !found {
		existing := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
		if existing == nil || existing.Status != metav1.ConditionFalse {
			return
		}
		if !initContainersCompleted(podList.Items) {
			return
		}
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type:               ConditionModelTransferHealthy,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: isvc.Generation,
			LastTransitionTime: now,
			Reason:             ReasonModelTransferSucceeded,
			Message:            "Model weights staged successfully",
		})
		return
	}

	var remedy string
	for _, sig := range modelTransferSignatures {
		if sig.reason == reason {
			remedy = sig.remedy
			break
		}
	}
	message := fmt.Sprintf("Model transfer failed: %s", matchedLine)
	if nodeName != "" {
		message += fmt.Sprintf(" (node %s)", nodeName)
	}
	if remedy != "" {
		message += ". " + remedy
	}

	existing := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
	changed := existing == nil || existing.Status != metav1.ConditionFalse || existing.Reason != reason
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionModelTransferHealthy,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, reason, "Reconcile", "%s", message)
	}
}
