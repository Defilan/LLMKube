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

package cli

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// defaultCacheAccessImage is the image both cache-access pods run when
	// nothing sets one.
	defaultCacheAccessImage = "busybox:1.37.0"
	// cacheImageEnv sets the cache-access pod image for every cache command.
	cacheImageEnv = "LLMKUBE_CACHE_IMAGE"
)

// cacheAccessImage is the image the --cache-image flag selected for this run;
// empty when the flag did not set one.
var cacheAccessImage string

// cacheAccessImageName resolves the cache-access pod image: the flag, then
// LLMKUBE_CACHE_IMAGE, then the busybox default. Both the inspector and the
// purge pod read it here, so an air-gapped mirror reaches all three commands.
func cacheAccessImageName() string {
	if cacheAccessImage != "" {
		return cacheAccessImage
	}
	if image := os.Getenv(cacheImageEnv); image != "" {
		return image
	}
	return defaultCacheAccessImage
}

// withCacheImage runs fn with image as the cache-access pod image, restoring
// the previous setting afterwards.
func withCacheImage(image string, fn func() error) error {
	previous := cacheAccessImage
	cacheAccessImage = image
	defer func() { cacheAccessImage = previous }()
	return fn()
}

// claimReadClient is the client the cache commands read claims through. It is
// a variable so a test can refuse the read, as a caller without RBAC on
// persistentvolumeclaims gets.
var claimReadClient = func(c client.Client) client.Client { return c }

// Second-mount snapshots, taken by a caller before the delete that collects the
// claims. A perService claim is owner-ref'd to its InferenceService, so it can
// be gone by purge time; its access mode is what was read before that delete.
//
// The map is replaced as a whole by snapshotSecondMountClaims and never mutated
// afterwards, so a purge in another run cannot see a half snapshot.
var (
	// secondMountSinglePod marks a claim the snapshot read as ReadWriteOncePod.
	secondMountSinglePod = map[secondMountClaimKey]bool{}
)

// clearSecondMountSnapshot drops the snapshot so a later command reads the
// claims afresh.
func clearSecondMountSnapshot() {
	secondMountSinglePod = map[secondMountClaimKey]bool{}
}

// secondMountClaimKey identifies a claim in a snapshot.
type secondMountClaimKey struct{ namespace, name string }

// snapshotSecondMountClaims reads the access mode of every claim a purge may
// act on before the caller deletes anything, and returns what a later
// secondMountBlocker needs when the claim itself is gone.
func snapshotSecondMountClaims(
	ctx context.Context, k8sClient client.Client, namespace string, claims []string,
) {
	// A claim the read fails for stays absent, so an unknown mode refuses
	// nothing and keeps base behavior.
	singlePod := map[secondMountClaimKey]bool{}
	for _, claim := range claims {
		key := secondMountClaimKey{namespace: namespace, name: claim}
		claimObject := readCacheClaim(ctx, k8sClient, namespace, claim)
		if claimObject == nil {
			continue
		}
		// Presence keys the snapshot, so a claim read as not single-pod is still
		// answerable from it.
		singlePod[key] = claimSinglePod(claimObject)
	}
	secondMountSinglePod = singlePod
}

// readCacheClaim reads a cache claim. A claim that cannot be read (including a
// forbidden one) yields nil, and the access mode counts as unknown: an
// unpinned strict claim stalls on Multi-Attach, so the pre-#2155 pin is the
// safer of the two.
func readCacheClaim(
	ctx context.Context, k8sClient client.Client, namespace, pvcName string,
) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := claimReadClient(k8sClient).Get(ctx, client.ObjectKey{Name: pvcName, Namespace: namespace}, pvc); err != nil {

		return nil
	}

	return pvc
}

// claimShareable reports whether claim is attached wherever it is needed. Any
// ReadWriteMany or ReadOnlyMany mode makes it shareable, so no pod's node
// holds it, mirroring singleWriterClaimHolder in internal/controller.
func claimShareable(pvc *corev1.PersistentVolumeClaim) bool {
	if pvc == nil {
		return false
	}
	for _, mode := range pvc.Spec.AccessModes {
		switch mode {
		case corev1.ReadWriteMany, corev1.ReadOnlyMany:
			return true
		}
	}
	return false
}

// claimSinglePod reports whether claim allows exactly one pod, so a second
// mount never succeeds while another pod holds it.
func claimSinglePod(pvc *corev1.PersistentVolumeClaim) bool {
	if pvc == nil {
		return false
	}
	for _, mode := range pvc.Spec.AccessModes {
		if mode == corev1.ReadWriteOncePod {
			return true
		}
	}
	return false
}

// claimSinglePodNow answers whether claim is ReadWriteOncePod from the caller's
// snapshot when it has one, and from the live object otherwise. The snapshot is
// the only answer once the caller has deleted what collects the claim.
func claimSinglePodNow(ctx context.Context, k8sClient client.Client, namespace, pvcName string) bool {
	key := secondMountClaimKey{namespace: namespace, name: pvcName}
	if singlePod, ok := secondMountSinglePod[key]; ok {
		return singlePod
	}
	return claimSinglePod(readCacheClaim(ctx, k8sClient, namespace, pvcName))
}

// secondMountBlocker returns the error to fail with when another pod holds a
// ReadWriteOncePod claim and will keep holding it, or nil when the run may
// create its pod; singlePod is claimSinglePodNow's answer for the claim.
//
// A pod that is being deleted releases the claim on its own, as does the pod
// this run replaces at ownPod, so waiting for either is what base did and what
// works. A pod that is merely running never releases it, so waiting only runs
// out the start timeout.
func secondMountBlocker(holder *corev1.Pod, pvcName, ownPod string,
	kind cacheAccessPodKind, runID string, singlePod bool,
) error {
	if holder == nil || !singlePod {
		return nil
	}
	if holder.DeletionTimestamp != nil {
		return nil
	}
	if holder.Name == ownPod {
		return nil
	}
	if cacheAccessPodOurs(holder, kind) && !cacheAccessPodInUse(holder, runID) {
		return nil
	}
	return fmt.Errorf("claim %s is ReadWriteOncePod and pod %q holds it; "+
		"only one pod can mount a ReadWriteOncePod claim, so the %s pod cannot start. "+
		"Wait for that pod to stop, or use a ReadWriteOnce or ReadWriteMany claim",
		pvcName, holder.Name, kind.noun)
}

// purgeSecondMountBlocker refuses a purge of a ReadWriteOncePod claim another
// pod holds, before any pod is created. Purge pods are never pinned, so this is
// the only guard the purge path needs: the claim cannot be mounted twice.
func purgeSecondMountBlocker(
	ctx context.Context, k8sClient client.Client, namespace, pvcName, runID string,
) error {
	if !claimSinglePodNow(ctx, k8sClient, namespace, pvcName) {
		return nil
	}
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		// No pod list means no evidence of a holder, and base did not look.
		return nil
	}
	holder := claimHolder(pods.Items, pvcName)
	// The decision is made; the blocker only needs the mode to report it.
	return secondMountBlocker(holder, pvcName, purgePodName(pvcName), purgePodKind, runID, true)
}

// cacheAccessPodOurs reports whether pod is a cache-access pod of this kind,
// the only kind of pod llmkube deletes to free a claim.
func cacheAccessPodOurs(pod *corev1.Pod, kind cacheAccessPodKind) bool {
	return pod.Labels[managedByLabel] == managedByValue && pod.Labels[modelCacheLabel] == kind.component
}

// cacheAccessPodInUse reports whether a cache-access pod is stamped by a run
// that is not this one and has not outlived it.
func cacheAccessPodInUse(pod *corev1.Pod, runID string) bool {
	stamp := pod.Labels[cachePurgeRunLabel]
	return stamp != "" && stamp != runID && !purgePodAbandoned(pod)
}
