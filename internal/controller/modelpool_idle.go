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
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// idleVerdict is the answer to "may this member's slot be freed right now".
type idleVerdict int

const (
	// idleVerdictIdle means every Pod that could be serving is idle: the member
	// holds no in-flight request and can be unloaded.
	idleVerdictIdle idleVerdict = iota

	// idleVerdictBusy means a Pod answered the probe and reported in-flight work.
	idleVerdictBusy

	// idleVerdictUnknown means idleness could not be established. The caller
	// fails closed and keeps the member resident.
	idleVerdictUnknown
)

// memberPods lists the member's Pods by the labels the InferenceService
// reconciler applies. A Terminating Pod counts as present: its GPU or
// unified-memory allocation is released only when its process exits.
func (r *ModelPoolReconciler) memberPods(ctx context.Context, isvc *inferencev1alpha1.InferenceService) ([]corev1.Pod, error) {
	if isvc == nil {
		return nil, nil
	}
	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if err := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// classifyMemberPods counts the member's Pods with at least one running
// container. A Pod under deletion, or with no container in Running, cannot be
// serving. A Pod whose sidecar runs while its server container has exited is
// still counted, which only matters for the two-or-more-Pod refusal case and
// only biases it toward failing closed.
func classifyMemberPods(pods []corev1.Pod) int {
	running := 0
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Running != nil {
				running++
				break
			}
		}
	}
	return running
}

// memberIdleVerdict decides whether a member that reports Ready can be
// unloaded. The runtime idle probe answers first; the member's Pod state is
// consulted only when the probe is refused.
//
// The caller only asks about a member that reports Ready: one that is still
// booting answers 503 on every path (llama.cpp server-http.cpp
// middleware_server_state), so a probe there can only say "unknown". For a
// Ready member, a refused dial means nothing on its Service path is accepting
// new work: kube-proxy rejects the connection when the Service has no ready
// endpoints, and a forwarded Pod with no listener resets it. So a refusal is
// not ambiguity, and the Pod state says the rest: no Pod, a Terminating Pod, or
// no running container is not serving, and one running Pod is the Pod the
// Service would have forwarded to. A refusal with two or more running Pods stays
// ambiguous because it does not say which Pod refused, and every other probe
// failure stays ambiguous too.
func (r *ModelPoolReconciler) memberIdleVerdict(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	pods []corev1.Pod,
) idleVerdict {
	idle, err := r.memberIdle(ctx, isvc)
	switch {
	case err == nil && idle:
		return idleVerdictIdle
	case err == nil:
		return idleVerdictBusy
	case idleProbeRefused(err):
		// Nothing is accepting the dial, so the Pods decide. Two or more
		// running Pods cannot: the refusal does not say which one answered.
		if classifyMemberPods(pods) > 1 {
			return idleVerdictUnknown
		}
		return idleVerdictIdle
	default:
		return idleVerdictUnknown
	}
}

// idleProbeRefused reports whether a probe failed because nothing was
// listening at the other end.
func idleProbeRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
