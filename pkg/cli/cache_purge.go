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
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

const (
	cachePurgePodPrefix = "llmkube-cache-purge-"
	cachePurgeComponent = "cache-purge"
	managedByLabel      = "app.kubernetes.io/managed-by"
	managedByValue      = "llmkube-cli"

	// cacheKeyDirPattern matches the directory names the operator writes under
	// the cache mount (/models/<key>, key from cachekey.Compute).
	cacheKeyDirPattern = `^[0-9a-f]{16}$`
	// cacheKeySegmentPattern bounds a key to one path segment that cannot be
	// read as a flag.
	cacheKeySegmentPattern = `^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
)

// Test seams: the unit tests have no cluster to exec in or wait on.
var (
	purgeExec = execInPod
	purgeWait = waitForPodRunning
)

var (
	cacheKeyDirRe     = regexp.MustCompile(cacheKeyDirPattern)
	cacheKeySegmentRe = regexp.MustCompile(cacheKeySegmentPattern)
)

// purgeOutcome is what one PVC's purge did.
type purgeOutcome struct {
	PVC     string
	Removed []string
	Absent  []string
	// Skipped holds directory names the purge left alone: entries that are not
	// cache keys on a user-owned claim, and names that fail validation.
	Skipped []string
}

// isEmpty reports whether the purge had no effect on this PVC.
func (o purgeOutcome) isEmpty() bool {
	return len(o.Removed) == 0 && len(o.Absent) == 0 && len(o.Skipped) == 0
}

// validateCacheKey rejects any key that is not a single path segment. The key
// is interpolated into a shell command run as root inside a pod, so a key that
// could contain a separator or start with a dash is the one thing standing
// between "delete this entry" and "delete the volume".
func validateCacheKey(key string) error {
	if !cacheKeySegmentRe.MatchString(key) {
		return fmt.Errorf("invalid cache key %q: expected a single path segment", key)
	}
	return nil
}

// targetedClaims narrows the claims resolved for modelName to the ones that
// can hold that model's files: the claims its own InferenceServices bring with
// spec.modelCache.claimName, plus the shared cache; otherwise the shared cache
// alone. The per-service "<isvc>-model-cache" claims of unrelated services
// stay untouched, so clearing one model does not create a read-write pod on
// every running service's volume. all is every claim in the namespace.
func targetedClaims(all []purgeTarget, modelName string) []purgeTarget {
	own := map[string]bool{}
	for _, target := range claimsOfModelServices(all) {
		own[target.Name] = true
	}
	own[modelCachePVCName] = true

	// The shared claim is right whenever the model has no claimName override;
	// with one, the operator warns the override is ignored and everything stays
	// in that first-consumer claim instead (see delete.go).
	var selected []purgeTarget
	for _, target := range all {
		if target.Name == modelCachePVCName {
			selected = append(selected, target)
		}
	}
	for _, target := range all {
		if target.UserOwned && own[target.Name] {
			selected = append(selected, target)
		}
	}
	if len(selected) == 0 {
		// perService mode without a claimName override: the model's own
		// "<isvc>-model-cache" is the only place its files can be.
		if ownTargets := perServiceClaimsForModel(all, modelName); len(ownTargets) > 0 {
			return ownTargets
		}
	}
	return selected
}

// perServiceClaimsForModel returns the operator per-service claims that a
// model's own InferenceServices mount.
func perServiceClaimsForModel(all []purgeTarget, modelName string) []purgeTarget {
	expected := modelName + "-model-cache"
	var own []purgeTarget
	for _, target := range all {
		if !target.UserOwned && target.Name == expected {
			own = append(own, target)
		}
	}
	return own
}

// claimsOfModelServices returns the user-owned claims among the targets.
func claimsOfModelServices(targets []purgeTarget) []purgeTarget {
	var own []purgeTarget
	for _, target := range targets {
		if target.UserOwned {
			own = append(own, target)
		}
	}
	return own
}

// purgeKey returns the key `cache clear --model` removes for a model, or an
// error when the model has none.
func purgeKey(model *inferencev1alpha1.Model, modelName string) (string, error) {
	key := cachekey.EffectiveKey(model)
	if key == "" {
		return "", fmt.Errorf("model '%s' has no cache key (not cached, or a metal/single-file source)", modelName)
	}
	return key, nil
}

// purgePodName returns the deterministic purge pod name for a claim. The hash
// keeps it unique per claim and DNS-1123 safe, as inspectorPodName does for the
// inspector.
func purgePodName(pvcName string) string {
	return cachePurgePodPrefix + cachekey.Compute(pvcName)
}

// createPurgePodForPVC creates the pod that removes entries from a cache claim.
// It mounts the claim read-write at the same path the inspector reads it from.
func createPurgePodForPVC(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName string,
) (string, error) {
	podName := purgePodName(pvcName)
	pod := newCacheAccessPod(podName, pvcName, cachePurgeComponent, "purge", false)
	pod.Namespace = namespace
	if _, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	return podName, nil
}

// ensurePurgePod returns a running purge pod, creating it when absent.
//
// The pod is ours to replace only when it carries both the managed-by and
// component labels; an unlabelled or foreign pod at that name is an error
// rather than something to delete.
func ensurePurgePod(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName string,
) (string, error) {
	podName := purgePodName(pvcName)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := createPurgePodForPVC(ctx, clientset, namespace, pvcName); err == nil {
			break
		} else if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("failed to create purge pod for PVC %s: %w", pvcName, err)
		}

		existing, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to read existing purge pod %s: %w", podName, err)
		}
		if existing.Labels[managedByLabel] != managedByValue ||
			existing.Labels[modelCacheLabel] != cachePurgeComponent {
			return "", fmt.Errorf("refusing to delete pod %s: it is not an llmkube cache purge pod", podName)
		}
		// A pod left by an interrupted run still holds the claim; removing it
		// lets the create above succeed on the second attempt.
		deleteCacheAccessPod(ctx, clientset, namespace, podName)
	}

	if err := purgeWait(ctx, clientset, namespace, podName, 120*time.Second); err != nil {
		deleteCacheAccessPod(context.Background(), clientset, namespace, podName)
		return "", fmt.Errorf("purge pod for PVC %s failed to start: %w", pvcName, err)
	}
	return podName, nil
}

// withPurgePod runs fn inside a pod that mounts pvcName read-write, then
// deletes the pod.
//
// It always uses its own pod rather than a serving pod: the serving container
// mounts the claim read-only and may have no shell at all, and a strict RWO
// claim cannot be double-attached while that pod runs.
func withPurgePod(
	ctx context.Context, clientset kubernetes.Interface, namespace, pvcName string,
	fn func(ctx context.Context, podName string) error,
) error {
	podName, err := ensurePurgePod(ctx, clientset, namespace, pvcName)
	if err != nil {
		return err
	}
	defer deleteCacheAccessPod(context.Background(), clientset, namespace, podName)
	return fn(ctx, podName)
}

// listCacheDirs returns the entry names directly under the cache mount.
func listCacheDirs(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName string,
) ([]string, error) {
	output, err := purgeExec(ctx, cfg, clientset, namespace, podName, "purge",
		[]string{"sh", "-c", "ls -1 " + defaultModelMountPath})
	if err != nil {
		return nil, fmt.Errorf("failed to list the cache: %w", err)
	}
	var entries []string
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			entries = append(entries, line)
		}
	}
	return entries, nil
}

// deleteCachePaths removes the given absolute paths and verifies each is gone.
// It returns the paths that were removed and the ones that were already absent;
// an exec failure or a path that survives the rm is an error.
func deleteCachePaths(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName string, paths []string,
) (removed, absent []string, err error) {
	if len(paths) == 0 {
		return nil, nil, nil
	}

	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = "'" + p + "'"
	}
	script := `for p in ` + strings.Join(quoted, " ") + `; do ` +
		`if [ -e "$p" ]; then rm -rf "$p" && echo "removed $p" || exit 1; ` +
		`else echo "absent $p"; fi; done`

	output, err := purgeExec(ctx, cfg, clientset, namespace, podName, "purge",
		[]string{"sh", "-c", script})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to remove cache entries: %w", err)
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "removed":
			removed = append(removed, fields[1])
		case "absent":
			absent = append(absent, fields[1])
		}
	}

	if len(removed) == 0 {
		return removed, absent, nil
	}
	return removed, absent, verifyCachePathsGone(ctx, cfg, clientset, namespace, podName, removed)
}

// verifyCachePathsGone fails when any of paths still exists after the removal.
func verifyCachePathsGone(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName string, paths []string,
) error {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = "'" + p + "'"
	}
	script := `for p in ` + strings.Join(quoted, " ") + `; do [ -e "$p" ] && echo "still $p"; done; exit 0`

	output, err := purgeExec(ctx, cfg, clientset, namespace, podName, "purge",
		[]string{"sh", "-c", script})
	if err != nil {
		return fmt.Errorf("failed to verify the cache entries were removed: %w", err)
	}
	var still []string
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "still" {
			still = append(still, fields[1])
		}
	}
	if len(still) > 0 {
		return fmt.Errorf("cache entries still present after removal: %s", strings.Join(still, ", "))
	}
	return nil
}

// purgeListed removes what choose selects from the claim's cache directories.
func purgeListed(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, pvcName string, choose func(entries []string) (del, skip []string),
) (purgeOutcome, error) {
	outcome := purgeOutcome{PVC: pvcName}
	err := withPurgePod(ctx, clientset, namespace, pvcName,
		func(ctx context.Context, podName string) error {
			entries, err := listCacheDirs(ctx, cfg, clientset, namespace, podName)
			if err != nil {
				return err
			}
			del, skip := choose(entries)
			outcome.Skipped = append(outcome.Skipped, skip...)

			paths := make([]string, len(del))
			for i, name := range del {
				paths[i] = defaultModelMountPath + "/" + name
			}
			removed, absent, err := deleteCachePaths(ctx, cfg, clientset, namespace, podName, paths)
			outcome.Removed = removed
			outcome.Absent = absent
			return err
		})
	return outcome, err
}

// purgeCacheKeys removes the named entries from one claim. A key that is not
// there is reported, not failed: the entry is already gone. Every key is
// validated before a pod is created.
func purgeCacheKeys(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, pvcName string, keys []string,
) (purgeOutcome, error) {
	for _, key := range keys {
		if err := validateCacheKey(key); err != nil {
			return purgeOutcome{PVC: pvcName}, err
		}
	}

	paths := make([]string, len(keys))
	for i, key := range keys {
		paths[i] = defaultModelMountPath + "/" + key
	}

	outcome := purgeOutcome{PVC: pvcName}
	err := withPurgePod(ctx, clientset, namespace, pvcName,
		func(ctx context.Context, podName string) error {
			removed, absent, err := deleteCachePaths(ctx, cfg, clientset, namespace, podName, paths)
			outcome.Removed = removed
			outcome.Absent = absent
			return err
		})
	return outcome, err
}

// purgeAllEntries removes every cache entry from one claim. On a user-owned
// claim (/models may hold data the operator did not write) only the
// cache-key-shaped directories are eligible.
func purgeAllEntries(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, pvcName string, userOwned bool,
) (purgeOutcome, error) {
	return purgeListed(ctx, cfg, clientset, namespace, pvcName,
		func(entries []string) (del, skip []string) {
			for _, name := range entries {
				if userOwned && !cacheKeyDirRe.MatchString(name) {
					skip = append(skip, name)
					continue
				}
				del = append(del, name)
			}
			return del, skip
		})
}

// purgeOrphanedEntries removes the cache-key directories on a claim that no
// Model references.
func purgeOrphanedEntries(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, pvcName string, active map[string]bool,
) (purgeOutcome, error) {
	return purgeListed(ctx, cfg, clientset, namespace, pvcName,
		func(entries []string) (del, skip []string) {
			for _, name := range entries {
				if !cacheKeyDirRe.MatchString(name) {
					skip = append(skip, name)
					continue
				}
				if active[name] {
					continue
				}
				del = append(del, name)
			}
			return del, skip
		})
}

// cachePVCandidates returns every cache claim in namespace that a purge may
// target, sorted and deduplicated: the label- and name-discovered operator
// caches, plus the spec.modelCache.claimName of an InferenceService named
// modelName or serving it. User-owned claims are outside the discovery label
// contract, so they only appear through the service spec.
func cachePVCandidates(
	ctx context.Context, k8sClient client.Client, namespace, modelName string,
) ([]string, error) {
	infos, err := discoverCachePVCs(ctx, k8sClient, namespace)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var candidates []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		candidates = append(candidates, name)
	}
	for _, info := range infos {
		add(info.Name)
	}

	if modelName != "" {
		claims, err := userClaimsForModel(ctx, k8sClient, namespace, modelName)
		if err != nil {
			return nil, err
		}
		for _, claim := range claims {
			add(claim)
		}
	}

	sort.Strings(candidates)
	return candidates, nil
}

// userClaimsForModel returns the claims that mount a model's cache: those of an
// InferenceService named after the model, and those of every service whose
// modelRef names it.
func userClaimsForModel(
	ctx context.Context, k8sClient client.Client, namespace, modelName string,
) ([]string, error) {
	var claims []string

	named := &inferencev1alpha1.InferenceService{}
	err := k8sClient.Get(ctx, client.ObjectKey{Name: modelName, Namespace: namespace}, named)
	switch {
	case err == nil:
		if claim := userModelCacheClaimName(named); claim != "" {
			claims = append(claims, claim)
		}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("failed to get InferenceService '%s': %w", modelName, err)
	}

	list := &inferencev1alpha1.InferenceServiceList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list InferenceServices: %w", err)
	}
	for i := range list.Items {
		svc := &list.Items[i]
		if svc.Name == modelName || svc.Spec.ModelRef != modelName {
			continue
		}
		if claim := userModelCacheClaimName(svc); claim != "" {
			claims = append(claims, claim)
		}
	}
	return claims, nil
}

// userModelCacheClaimName mirrors the controller's helper: the claim a service
// brings with spec.modelCache.claimName, or "".
func userModelCacheClaimName(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil || isvc.Spec.ModelCache == nil {
		return ""
	}
	return isvc.Spec.ModelCache.ClaimName
}

// purgePlan is resolved once per command: which claims exist, and whether each
// is user-owned (so --all must not delete data the operator did not write).
type purgeTarget struct {
	Name      string
	UserOwned bool
}

// resolvePurgeTargets keeps the candidates that exist and records who owns each.
func resolvePurgeTargets(
	ctx context.Context, k8sClient client.Client, namespace, modelName string,
) ([]purgeTarget, error) {
	candidates, err := cachePVCandidates(ctx, k8sClient, namespace, modelName)
	if err != nil {
		return nil, err
	}

	claims, err := userClaimsForModel(ctx, k8sClient, namespace, modelName)
	if err != nil {
		return nil, err
	}
	userOwned := map[string]bool{}
	for _, claim := range claims {
		userOwned[claim] = true
	}

	var targets []purgeTarget
	for _, name := range candidates {
		pvc := &corev1.PersistentVolumeClaim{}
		err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, pvc)
		switch {
		case err == nil:
			targets = append(targets, purgeTarget{Name: name, UserOwned: userOwned[name]})
		case apierrors.IsNotFound(err):
			continue
		default:
			return nil, fmt.Errorf("failed to get cache PVC %s: %w", name, err)
		}
	}
	return targets, nil
}
