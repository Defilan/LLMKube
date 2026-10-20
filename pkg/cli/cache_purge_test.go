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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

// nilRESTConfig is never dereferenced: the tests replace purgeExec and
// purgeWait, so no SPDY executor or API poll is ever built.
var nilRESTConfig = &rest.Config{}

// fakePurgeExec replaces purgeExec and purgeWait for the duration of a test.
// Every exec'd command is appended to *cmds in order.
func fakePurgeExec(t *testing.T, result execFunc, cmds *[][]string) {
	t.Helper()
	origExec, origWait := purgeExec, purgeWait
	t.Cleanup(func() { purgeExec, purgeWait = origExec, origWait })
	purgeExec = func(_ context.Context, _ *rest.Config, _ kubernetes.Interface,
		_, _, _ string, command []string) (string, error) {
		*cmds = append(*cmds, command)
		return result(command)
	}
	purgeWait = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ time.Duration) error {
		return nil
	}
}

// execFunc answers one exec'd command.
type execFunc func(command []string) (string, error)

func TestValidateCacheKey(t *testing.T) {
	valid := []string{
		"a3b8c9d4e5f67890",
		"deadbeef01234567",
		"0123456789abcdef",
		"a",
		strings.Repeat("a", 63),
	}
	for _, key := range valid {
		if err := validateCacheKey(key); err != nil {
			t.Errorf("validateCacheKey(%q) = %v, want nil", key, err)
		}
	}

	invalid := []string{
		"",
		".",
		"..",
		"../etc",
		"a/b",
		"/models/abc",
		"-rf",
		".hidden",
		"a b",
		strings.Repeat("a", 64),
	}
	for _, key := range invalid {
		if err := validateCacheKey(key); err == nil {
			t.Errorf("validateCacheKey(%q) = nil, want an error", key)
		}
	}
}

func TestPurgePodName(t *testing.T) {
	a := purgePodName("llmkube-model-cache")
	b := purgePodName("isvc-a-model-cache")
	if a == b {
		t.Fatalf("purge pod names collided: both %q", a)
	}
	for _, name := range []string{a, b} {
		if !strings.HasPrefix(name, cachePurgePodPrefix) {
			t.Errorf("purge pod name %q missing prefix %q", name, cachePurgePodPrefix)
		}
		if len(name) > 63 {
			t.Errorf("purge pod name %q exceeds the 63-char DNS-1123 limit", name)
		}
	}
}

func TestNewPurgePod(t *testing.T) {
	pvcName := "my-isvc-model-cache"
	clientset := fakeclientset.NewClientset()

	podName, err := createPurgePodForPVC(context.Background(), clientset, "test-ns", pvcName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pod, err := clientset.CoreV1().Pods("test-ns").Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get created pod: %v", err)
	}
	if pod.Labels[managedByLabel] != managedByValue {
		t.Errorf("managed-by label = %q, want %q", pod.Labels[managedByLabel], managedByValue)
	}
	if pod.Labels[modelCacheLabel] != cachePurgeComponent {
		t.Errorf("component label = %q, want %q", pod.Labels[modelCacheLabel], cachePurgeComponent)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want %q", pod.Spec.RestartPolicy, corev1.RestartPolicyNever)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("container count = %d, want 1", len(pod.Spec.Containers))
	}

	container := pod.Spec.Containers[0]
	if container.Name != "purge" {
		t.Errorf("container name = %q, want %q", container.Name, "purge")
	}
	if container.Image != "busybox:1.37.0" {
		t.Errorf("image = %q, want %q", container.Image, "busybox:1.37.0")
	}
	if len(container.VolumeMounts) != 1 {
		t.Fatalf("volume mount count = %d, want 1", len(container.VolumeMounts))
	}
	if container.VolumeMounts[0].MountPath != defaultModelMountPath {
		t.Errorf("mount path = %q, want %q", container.VolumeMounts[0].MountPath, defaultModelMountPath)
	}
	if container.VolumeMounts[0].ReadOnly {
		t.Error("purge mount must be read-write")
	}
	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("volume count = %d, want 1", len(pod.Spec.Volumes))
	}
	vol := pod.Spec.Volumes[0]
	if vol.PersistentVolumeClaim == nil {
		t.Fatal("volume PVC source is nil")
	}
	if vol.PersistentVolumeClaim.ClaimName != pvcName {
		t.Errorf("PVC claim = %q, want %q", vol.PersistentVolumeClaim.ClaimName, pvcName)
	}
	if vol.PersistentVolumeClaim.ReadOnly {
		t.Error("PVC volume source must be read-write")
	}
}

func TestEnsurePurgePod_ReplacesStale(t *testing.T) {
	pvcName := "llmkube-model-cache"
	stale := newCacheAccessPod(purgePodName(pvcName), pvcName, cachePurgeComponent, "purge", false)
	stale.Namespace = "default"
	clientset := fakeclientset.NewClientset(stale)
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) { return "", nil }, &cmds)

	podName, err := ensurePurgePod(context.Background(), clientset, "default", pvcName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if podName != purgePodName(pvcName) {
		t.Errorf("pod name = %q, want %q", podName, purgePodName(pvcName))
	}
	if _, err := clientset.CoreV1().Pods("default").Get(
		context.Background(), podName, metav1.GetOptions{}); err != nil {
		t.Fatalf("purge pod not recreated: %v", err)
	}
}

func TestEnsurePurgePod_RefusesForeignPod(t *testing.T) {
	pvcName := "llmkube-model-cache"
	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      purgePodName(pvcName),
			Namespace: "default",
			Labels:    map[string]string{"app": "someone-else"},
		},
	}
	clientset := fakeclientset.NewClientset(foreign)

	_, err := ensurePurgePod(context.Background(), clientset, "default", pvcName)
	if err == nil {
		t.Fatal("expected an error for a foreign pod at the purge pod name")
	}
	if !strings.Contains(err.Error(), "refusing to delete") {
		t.Errorf("error = %q, want it to refuse the foreign pod", err)
	}
	if _, getErr := clientset.CoreV1().Pods("default").Get(
		context.Background(), purgePodName(pvcName), metav1.GetOptions{}); getErr != nil {
		t.Errorf("foreign pod was deleted: %v", getErr)
	}
}

func TestDeleteCachePaths_RemovesValidatedPaths(t *testing.T) {
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if strings.HasPrefix(command[2], "for p in") {
			return "removed /models/aaa\nabsent /models/bbb\n", nil
		}
		return "", nil
	}, &cmds)
	clientset := fakeclientset.NewClientset()

	removed, absent, err := deleteCachePaths(context.Background(), nilRESTConfig, clientset,
		"default", "pod", []string{"/models/aaa", "/models/bbb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(removed) != 1 || removed[0] != "/models/aaa" {
		t.Errorf("removed = %v, want [/models/aaa]", removed)
	}
	if len(absent) != 1 || absent[0] != "/models/bbb" {
		t.Errorf("absent = %v, want [/models/bbb]", absent)
	}
	if len(cmds) != 2 {
		t.Fatalf("exec count = %d, want 2 (remove then verify)", len(cmds))
	}
	if !strings.Contains(cmds[0][2], "'/models/aaa' '/models/bbb'") {
		t.Errorf("remove command = %q, want both validated paths", cmds[0][2])
	}
	if strings.Contains(cmds[0][2], "*") {
		t.Errorf("remove command = %q, want no glob", cmds[0][2])
	}
}

func TestDeleteCachePaths_StillPresent(t *testing.T) {
	fakePurgeExec(t, func(command []string) (string, error) {
		if strings.Contains(command[2], "echo \"still $p\"") {
			return "still /models/aaa\n", nil
		}
		return "removed /models/aaa\n", nil
	}, &[][]string{})
	clientset := fakeclientset.NewClientset()

	_, _, err := deleteCachePaths(context.Background(), nilRESTConfig, clientset,
		"default", "pod", []string{"/models/aaa"})
	if err == nil {
		t.Fatal("expected an error when the entry survives the rm")
	}
	if !strings.Contains(err.Error(), "/models/aaa") {
		t.Errorf("error = %q, want it to name the surviving path", err)
	}
}

func TestDeleteCachePaths_ExecError(t *testing.T) {
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		return "", fmt.Errorf("exec failed: container not running")
	}, &cmds)
	clientset := fakeclientset.NewClientset()

	_, _, err := deleteCachePaths(context.Background(), nilRESTConfig, clientset,
		"default", "pod", []string{"/models/aaa"})
	if err == nil {
		t.Fatal("expected the exec error to be returned, not swallowed")
	}
}

func TestPurgeCacheKeys_CreatesAndCleansUp(t *testing.T) {
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		if strings.Contains(command[2], "ls -1") {
			return "", nil
		}
		return "removed /models/deadbeef01234567\n", nil
	}, &cmds)
	clientset := fakeclientset.NewClientset()

	outcome, err := purgeCacheKeys(context.Background(), nilRESTConfig, clientset,
		"default", "llmkube-model-cache", []string{"deadbeef01234567"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(outcome.Removed) != 1 || outcome.Removed[0] != "/models/deadbeef01234567" {
		t.Errorf("removed = %v, want [/models/deadbeef01234567]", outcome.Removed)
	}
	if _, err := clientset.CoreV1().Pods("default").Get(context.Background(),
		purgePodName("llmkube-model-cache"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("purge pod was not deleted: err = %v", err)
	}
}

func TestPurgeCacheKeys_RejectsTraversal(t *testing.T) {
	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		return "", nil
	}, &cmds)
	clientset := fakeclientset.NewClientset()

	_, err := purgeCacheKeys(context.Background(), nilRESTConfig, clientset,
		"default", "llmkube-model-cache", []string{"../etc/passwd"})
	if err == nil {
		t.Fatal("expected an error for a key that is not one path segment")
	}
	pods, _ := clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("a pod was created for an unvalidated key: %d pods", len(pods.Items))
	}
	if len(cmds) != 0 {
		t.Errorf("exec ran for an unvalidated key: %v", cmds)
	}
}

func TestPurgeCacheKeys_Absent(t *testing.T) {
	fakePurgeExec(t, func(command []string) (string, error) {
		return "absent /models/deadbeef01234567\n", nil
	}, &[][]string{})
	clientset := fakeclientset.NewClientset()

	outcome, err := purgeCacheKeys(context.Background(), nilRESTConfig, clientset,
		"default", "llmkube-model-cache", []string{"deadbeef01234567"})
	if err != nil {
		t.Fatalf("an absent entry must not be an error: %v", err)
	}
	if len(outcome.Removed) != 0 {
		t.Errorf("removed = %v, want none", outcome.Removed)
	}
	if len(outcome.Absent) != 1 {
		t.Errorf("absent = %v, want one path", outcome.Absent)
	}
}

// purgeScripts records every rm command run through purgeListed.
func capturePurgeScripts(t *testing.T) *[]string {
	t.Helper()
	var rmCmds []string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		if strings.Contains(command[2], "ls -1 /models") {
			return "deadbeef01234567\n0123456789abcdef\nkeep-me.txt\n", nil
		}
		rmCmds = append(rmCmds, command[2])
		return "", nil
	}, &[][]string{})
	return &rmCmds
}

func lastRMCmd(t *testing.T, cmds []string) string {
	t.Helper()
	if len(cmds) == 0 {
		t.Fatal("no rm command was run")
	}
	return cmds[len(cmds)-1]
}

func TestPurgeOrphanedEntries(t *testing.T) {
	cmds := capturePurgeScripts(t)
	clientset := fakeclientset.NewClientset()

	outcome, err := purgeOrphanedEntries(context.Background(), nilRESTConfig, clientset,
		"default", "llmkube-model-cache", map[string]bool{"0123456789abcdef": true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd := lastRMCmd(t, *cmds)
	if !strings.Contains(cmd, "'/models/deadbeef01234567'") {
		t.Errorf("orphaned entry was not removed: %q", cmd)
	}
	if strings.Contains(cmd, "'/models/0123456789abcdef'") {
		t.Errorf("active entry was removed: %q", cmd)
	}
	if len(outcome.Skipped) != 1 || outcome.Skipped[0] != "keep-me.txt" {
		t.Errorf("skipped = %v, want [keep-me.txt]", outcome.Skipped)
	}
}

func TestPurgeAllEntries_OperatorClaim(t *testing.T) {
	cmds := capturePurgeScripts(t)
	clientset := fakeclientset.NewClientset()

	if _, err := purgeAllEntries(context.Background(), nilRESTConfig, clientset,
		"default", "llmkube-model-cache", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd := lastRMCmd(t, *cmds)
	for _, want := range []string{
		"'/models/deadbeef01234567'", "'/models/0123456789abcdef'", "'/models/keep-me.txt'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("operator claim: %s missing from %q", want, cmd)
		}
	}
}

func TestPurgeAllEntries_UserClaim(t *testing.T) {
	cmds := capturePurgeScripts(t)
	clientset := fakeclientset.NewClientset()

	outcome, err := purgeAllEntries(context.Background(), nilRESTConfig, clientset,
		"default", "user-claim", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd := lastRMCmd(t, *cmds)
	if !strings.Contains(cmd, "'/models/deadbeef01234567'") ||
		!strings.Contains(cmd, "'/models/0123456789abcdef'") {
		t.Errorf("user claim: cache keys not removed: %q", cmd)
	}
	if strings.Contains(cmd, "keep-me.txt") {
		t.Errorf("user claim: non-key entry was removed: %q", cmd)
	}
	if len(outcome.Skipped) != 1 || outcome.Skipped[0] != "keep-me.txt" {
		t.Errorf("skipped = %v, want [keep-me.txt]", outcome.Skipped)
	}
}

func newCLIScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = inferencev1alpha1.AddToScheme(s)
	return s
}

func TestCachePVCandidates(t *testing.T) {
	shared := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "llmkube-model-cache",
		Namespace: "default",
	}}
	perIsvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "labeled-model-cache",
		Namespace: "default",
		Labels:    map[string]string{modelCacheLabel: modelCacheLabelValue},
	}}
	userClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "my-cache-claim",
		Namespace: "default",
	}}
	otherClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "other-cache-claim",
		Namespace: "default",
	}}

	named := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "model-a", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   "model-a",
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "my-cache-claim"},
		},
	}
	refs := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-b", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   "model-a",
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "my-cache-claim"},
		},
	}
	unrelated := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-c", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   "model-c",
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "other-cache-claim"},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(newCLIScheme()).
		WithObjects(shared, perIsvc, userClaim, otherClaim, named, refs, unrelated).
		Build()

	got, err := cachePVCandidates(context.Background(), k8sClient, "default", "model-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"labeled-model-cache", "llmkube-model-cache", "my-cache-claim"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want %v (user claim from the named service, or one serving the model)", got, want)
	}

	all, err := cachePVCandidates(context.Background(), k8sClient, "default", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(all, ",") != "labeled-model-cache,llmkube-model-cache" {
		t.Errorf("candidates without a model = %v, want only the discovered operator caches", all)
	}
}

func TestSeedCacheEntries(t *testing.T) {
	metal := inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "metal-model", Namespace: "ns"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/a.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	first := inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/repo",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
		Status: inferencev1alpha1.ModelStatus{Size: "7.2 GiB"},
	}
	second := *first.DeepCopy()
	second.Name = "m2"

	entries := map[string]*CacheEntry{}
	seedCacheEntries([]inferencev1alpha1.Model{metal, first, second}, false, entries)

	key := cachekey.Compute("hf://org/repo")
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (metal model skipped, shared key merged)", len(entries))
	}
	entry := entries[key]
	if entry == nil {
		t.Fatalf("missing entry for derived key %q", key)
	}
	if entry.Status != statusActive {
		t.Errorf("status = %q, want %q", entry.Status, statusActive)
	}
	if strings.Join(entry.ModelNames, ",") != "m1,m2" {
		t.Errorf("model names = %v, want [m1 m2]", entry.ModelNames)
	}
	if entry.SizeHuman != "7.2 GiB" {
		t.Errorf("size = %q, want %q", entry.SizeHuman, "7.2 GiB")
	}

	qualified := map[string]*CacheEntry{}
	seedCacheEntries([]inferencev1alpha1.Model{first}, true, qualified)
	if names := qualified[key].ModelNames; len(names) != 1 || names[0] != "ns/m1" {
		t.Errorf("all-namespaces names = %v, want [ns/m1]", names)
	}
}

// TestPurgeKey_MultiFileModel guards the key `cache clear --model` resolves:
// an hf:// repo model with spec.files leaves Status.CacheKey empty, so reading
// that field alone would refuse a model that is demonstrably on the cache.
func TestPurgeKey_MultiFileModel(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/repo",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
	}
	key, err := purgeKey(model, "repo-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := cachekey.Compute("hf://org/repo"); key != want {
		t.Errorf("purgeKey = %q, want the derived key %q", key, want)
	}

	metal := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/a.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	if _, err := purgeKey(metal, "metal-model"); err == nil {
		t.Error("expected an error for a model with no cache key")
	}
}

func TestTargetedClaims(t *testing.T) {
	all := []purgeTarget{
		{Name: "llmkube-model-cache"},
		{Name: "isvc-cache", UserOwned: true},
	}
	got := targetedClaims(all)
	if len(got) != 1 || got[0].Name != "isvc-cache" {
		t.Errorf("targetedClaims = %v, want only isvc-cache", got)
	}

	shared := []purgeTarget{{Name: "llmkube-model-cache"}}
	if got := targetedClaims(shared); len(got) != 1 || got[0].Name != "llmkube-model-cache" {
		t.Errorf("targetedClaims = %v, want the shared claim unchanged", got)
	}
}
