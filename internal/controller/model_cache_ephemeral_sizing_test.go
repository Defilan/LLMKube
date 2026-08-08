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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Bounding the ephemeral model cache (#1451 follow-up).
//
// An emptyDir with no sizeLimit is written to the node's ephemeral storage,
// which the scheduler cannot see and the kubelet will not charge to this Pod
// alone. A multi-gigabyte model on a node with a small boot disk therefore
// fills the node and triggers DiskPressure, and the kubelet then evicts by QoS
// class, which can take out unrelated workloads. Node disk size varies wildly
// across distributions, so this must be expressible rather than assumed.
//
// spec.resources.ephemeralStorage drives both halves: the emptyDir's sizeLimit
// (so the kubelet evicts THIS Pod instead of starving the node) and the
// container's ephemeral-storage request (so the scheduler stops overcommitting
// the node in the first place).

func isvcWithEphemeralCache(storage string) *inferencev1alpha1.InferenceService {
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "m",
			ModelCache: &inferencev1alpha1.ModelCacheSpec{
				Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral,
			},
		},
	}
	if storage != "" {
		isvc.Spec.Resources = &inferencev1alpha1.InferenceResourceRequirements{
			EphemeralStorage: storage,
		}
	}
	return isvc
}

func TestEphemeralCacheSizeLimit_SetFromResources(t *testing.T) {
	got := ephemeralCacheSizeLimit(isvcWithEphemeralCache("40Gi"))
	if got == nil {
		t.Fatal("expected a sizeLimit when resources.ephemeralStorage is set")
	}
	want := resource.MustParse("40Gi")
	if got.Cmp(want) != 0 {
		t.Errorf("sizeLimit = %s, want %s", got.String(), want.String())
	}
}

// No declared budget means no sizeLimit rather than a guessed one. Model size
// is not reliably known (Status.Size is populated from the GGUF metadata range
// read, which only runs for remote http(s) sources), so inventing a limit would
// evict Pods for exceeding a number the user never chose.
func TestEphemeralCacheSizeLimit_UnsetWithoutBudget(t *testing.T) {
	if got := ephemeralCacheSizeLimit(isvcWithEphemeralCache("")); got != nil {
		t.Errorf("sizeLimit = %v, want nil when no ephemeralStorage is declared", got)
	}
	if got := ephemeralCacheSizeLimit(nil); got != nil {
		t.Errorf("sizeLimit = %v, want nil for a nil isvc", got)
	}
}

// The limit belongs to the ephemeral path only. A Cached service writes to the
// cache PVC, where a sizeLimit on the (unused) emptyDir would mean nothing.
func TestEphemeralCacheSizeLimit_OnlyWhenEphemeral(t *testing.T) {
	cached := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:  "m",
			Resources: &inferencev1alpha1.InferenceResourceRequirements{EphemeralStorage: "40Gi"},
		},
	}
	if got := ephemeralCacheSizeLimit(cached); got != nil {
		t.Errorf("sizeLimit = %v, want nil when persistence is not Ephemeral", got)
	}
}

// The scheduler half: without an ephemeral-storage REQUEST the scheduler cannot
// see the download coming and will stack several such Pods onto one node.
func TestEphemeralStorage_BecomesRequestAndLimit(t *testing.T) {
	isvc := isvcWithEphemeralCache("40Gi")
	res := buildContainerResources(isvc, &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
	}, 0, "")

	req, ok := res.Requests[corev1.ResourceEphemeralStorage]
	if !ok {
		t.Fatal("no ephemeral-storage request; the scheduler cannot account for the download")
	}
	if want := resource.MustParse("40Gi"); req.Cmp(want) != 0 {
		t.Errorf("request = %s, want %s", req.String(), want.String())
	}

	// A limit as well as a request: the request is what the scheduler reserves,
	// the limit is what makes the kubelet evict THIS Pod rather than let the
	// node fill and evict by QoS class.
	lim, ok := res.Limits[corev1.ResourceEphemeralStorage]
	if !ok {
		t.Fatal("no ephemeral-storage limit; overrun would starve the node instead of this Pod")
	}
	if want := resource.MustParse("40Gi"); lim.Cmp(want) != 0 {
		t.Errorf("limit = %s, want %s", lim.String(), want.String())
	}
}

func TestEphemeralStorage_AbsentByDefault(t *testing.T) {
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:  "m",
			Resources: &inferencev1alpha1.InferenceResourceRequirements{CPU: "2", Memory: "4Gi"},
		},
	}
	res := buildContainerResources(isvc, &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
	}, 0, "")
	if _, ok := res.Requests[corev1.ResourceEphemeralStorage]; ok {
		t.Error("ephemeral-storage must not appear unless the user asked for it")
	}
	if _, ok := res.Limits[corev1.ResourceEphemeralStorage]; ok {
		t.Error("ephemeral-storage limit must not appear unless the user asked for it")
	}
}
