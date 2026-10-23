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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests drive the cobra commands end to end against envtest, so they pin
// what the user sees and the pod a run creates, not a helper's return value.

// cacheCommandEnv is one envtest namespace holding a cache claim and its pods.
type cacheCommandEnv struct {
	cfg       *rest.Config
	namespace string
	clientset kubernetes.Interface
	cr        client.Client
}

// cache2155Env creates namespace holding claim, plus model when non-nil, and
// switches config.GetConfig over to it.
func cache2155Env(t *testing.T, namespace string, claim *corev1.PersistentVolumeClaim,
	pods []*corev1.Pod, model *inferencev1alpha1.Model,
) *cacheCommandEnv {
	t.Helper()
	cfg := cacheListEnvtest(t)
	pointKubeconfigAt(t, cfg)
	ctx := context.Background()

	cr, err := client.New(cfg, client.Options{Scheme: cacheListEnvScheme()})
	if err != nil {
		t.Fatalf("controller-runtime client: %v", err)
	}
	if err := cr.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	claim.Namespace = namespace
	if err := cr.Create(ctx, claim); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	for _, pod := range pods {
		pod.Namespace = namespace
		if err := cr.Create(ctx, pod); err != nil {
			t.Fatalf("create pod %s: %v", pod.Name, err)
		}
		pod.Status.Phase = corev1.PodRunning
		if err := cr.Status().Update(ctx, pod); err != nil {
			t.Fatalf("mark pod %s running: %v", pod.Name, err)
		}
	}
	if model != nil {
		model.Namespace = namespace
		if err := cr.Create(ctx, model); err != nil {
			t.Fatalf("create model: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	return &cacheCommandEnv{cfg: cfg, namespace: namespace, clientset: clientset, cr: cr}
}

// stubCacheCommandPods fakes the pod start and the async delete. The real
// inspector cannot start on envtest (no kubelet), so the start wait alone would
// time out. With keep set, the delete is a no-op so the created pod can be read
// back; the delete also completes at once, as the fake deleteCacheAccessPod
// helper does, so a run that replaces a stale pod is not left waiting on it.
func stubCacheCommandPods(t *testing.T, answer execFunc, keep bool) {
	t.Helper()
	var calls []execCall
	stubCacheExec(t, answer, &calls)
	// The fake clientset never acks a delete, so the delete removes the object
	// here; a run that replaces a stale pod then does not wait on it.
	origDelete := deleteCacheAccessPod
	t.Cleanup(func() { deleteCacheAccessPod = origDelete })
	deleteCacheAccessPod = func(ctx context.Context, c kubernetes.Interface, ns, name string) {
		// No grace period: envtest's apiserver keeps a pod with a deletion
		// timestamp until a finalizer or grace period lets it go, as a real
		// kubelet confirms termination.
		grace := int64(0)
		_ = c.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
	}
	if keep {
		deleteCacheAccessPod = func(context.Context, kubernetes.Interface, string, string) {}
	}
}

// dynamicClaim is a claim of the given access modes, discovered by the cache
// list label, bound by whatever default class the cluster has.
func dynamicClaim(modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelCachePVCName,
			Labels:    map[string]string{modelCacheLabel: modelCacheLabelValue},
			Namespace: "placeholder",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: modes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

// mountedPod is an InferenceService-shaped pod on node-b that mounts the shared
// cache claim read-only, the way a serving pod does.
func mountedPod(name string) *corev1.Pod {
	pod := shelllessCachePod()
	pod.Name = name
	pod.Spec.NodeName = "node-b"
	return pod
}

// getCreatedPod reads a pod the command created, failing when it does not
// exist.
func getCreatedPod(t *testing.T, clientset kubernetes.Interface, namespace, name string) *corev1.Pod {
	t.Helper()
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return pod
}

// podExists reports whether a pod with this name is in the namespace.
func podExists(t *testing.T, clientset kubernetes.Interface, namespace, name string) bool {
	t.Helper()
	_, err := clientset.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

// runCacheCommand executes a cache subcommand with args, returning its error.
func runCacheCommand(t *testing.T, args ...string) error {
	t.Helper()
	cmd := NewCacheCommand()
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var err error
	captureStdout(t, func() { err = cmd.Execute() })
	return err
}

// modelWithCacheKey is a Model whose files live under a cache key, which is
// what `cache clear --model` needs.
func modelWithCacheKey() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "a-model"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
	}
}

// A shareable claim is attached wherever it is needed, so pinning the inspector
// to the mounting pod's node only adds a way to fail: a cordoned or NotReady
// node there stalls a claim that could be read anywhere (#2155, item 1).
func TestCacheListDoesNotPinTheInspectorForAShareableClaim(t *testing.T) {
	holder := mountedPod("serving")
	holder.Spec.Tolerations = []corev1.Toleration{{
		Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	}}
	env := cache2155Env(t, "cache-2155-rwx", dynamicClaim(corev1.ReadWriteMany),
		[]*corev1.Pod{holder}, modelWithCacheKey())
	stubCacheCommandPods(t, func([]string) (string, error) {
		return "4294967296\t/models/a1b2c3d4e5f60718/\n", nil
	}, true)

	if err := runCacheCommand(t, "list", "-n", env.namespace); err != nil {
		t.Fatalf("cache list: %v", err)
	}

	pod := getCreatedPod(t, env.clientset, env.namespace, inspectorPodName(modelCachePVCName))
	if got := pinnedTo(pod); len(got) != 0 {
		t.Errorf("inspector pinned to %v, want no pin: a ReadWriteMany claim is attached anywhere", got)
	}
	// The holder's toleration still comes along: a cluster whose nodes are all
	// GPU-tainted has nowhere else to run the inspector.
	found := false
	for _, tol := range pod.Spec.Tolerations {
		if tol.Key == "nvidia.com/gpu" {
			found = true
		}
	}
	if !found {
		t.Errorf("inspector tolerations = %v, want the holder's nvidia.com/gpu one",
			pod.Spec.Tolerations)
	}
}

// A ReadWriteOncePod claim another pod holds cannot be mounted a second time on
// any node, so the command must say so at once instead of waiting for a pod
// that can never run (#2155, related edge).
func TestCacheListFailsFastOnAHeldReadWriteOncePodClaim(t *testing.T) {
	env := cache2155Env(t, "cache-2155-rwop-list", dynamicClaim(corev1.ReadWriteOncePod),
		[]*corev1.Pod{mountedPod("serving")}, modelWithCacheKey())
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

	err := runCacheCommand(t, "list", "-n", env.namespace)
	if err == nil {
		t.Fatal("cache list returned nil; want an error naming ReadWriteOncePod")
	}
	if !strings.Contains(err.Error(), "ReadWriteOncePod") || !strings.Contains(err.Error(), "serving") {
		t.Errorf("error = %q, want it to name the access mode and the holding pod", err)
	}
	if podExists(t, env.clientset, env.namespace, inspectorPodName(modelCachePVCName)) {
		t.Error("an inspector pod was created for a claim it cannot mount")
	}
}

// The purge path stalls on the same claim for the same reason.
func TestCacheClearFailsFastOnAHeldReadWriteOncePodClaim(t *testing.T) {
	env := cache2155Env(t, "cache-2155-rwop-clear", dynamicClaim(corev1.ReadWriteOncePod),
		[]*corev1.Pod{mountedPod("serving")}, nil)
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

	err := runCacheCommand(t, "clear", "--force", "-n", env.namespace)
	if err == nil {
		t.Fatal("cache clear succeeded; want a ReadWriteOncePod refusal")
	}
	if !strings.Contains(err.Error(), "ReadWriteOncePod") {
		t.Errorf("error = %q, want it to name the access mode", err)
	}
	if podExists(t, env.clientset, env.namespace, purgePodName(modelCachePVCName)) {
		t.Error("a purge pod was created for a claim it cannot mount")
	}
}

// The cache-access image can be pointed at an air-gapped mirror (#2155, item 2).
const (
	cacheImageEnv2155  = "LLMKUBE_CACHE_IMAGE"
	cacheMirrorFixture = "registry.local:5000/llmkube/busybox:latest"
)

func TestCacheAccessPodsUseTheConfiguredImage(t *testing.T) {
	const mirror = cacheMirrorFixture

	t.Run("flag on cache list reaches the inspector", func(t *testing.T) {
		env := cache2155Env(t, "cache-2155-image-list", dynamicClaim(corev1.ReadWriteOnce),
			[]*corev1.Pod{mountedPod("serving")}, modelWithCacheKey())
		stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

		if err := runCacheCommand(t, "list", "-n", env.namespace, "--cache-image", mirror); err != nil {
			t.Fatalf("cache list --cache-image: %v", err)
		}
		container := getCreatedPod(t, env.clientset, env.namespace,
			inspectorPodName(modelCachePVCName)).Spec.Containers[0]
		if container.Image != mirror {
			t.Errorf("image = %q, want %q", container.Image, mirror)
		}
		if container.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("pull policy = %q, want %q", container.ImagePullPolicy, corev1.PullIfNotPresent)
		}
	})

	t.Run("flag on cache clear reaches the purge pod", func(t *testing.T) {
		env := cache2155Env(t, "cache-2155-image-clear", dynamicClaim(corev1.ReadWriteOnce), nil, nil)
		stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

		if err := runCacheCommand(t, "clear", "--force", "-n", env.namespace, "--cache-image", mirror); err != nil {
			t.Fatalf("cache clear --cache-image: %v", err)
		}
		container := getCreatedPod(t, env.clientset, env.namespace,
			purgePodName(modelCachePVCName)).Spec.Containers[0]
		if container.Image != mirror {
			t.Errorf("image = %q, want %q", container.Image, mirror)
		}
		if container.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("pull policy = %q, want %q", container.ImagePullPolicy, corev1.PullIfNotPresent)
		}
	})

	t.Run("environment reaches the inspector with no flag", func(t *testing.T) {
		t.Setenv(cacheImageEnv2155, mirror)
		env := cache2155Env(t, "cache-2155-image-env", dynamicClaim(corev1.ReadWriteOnce),
			[]*corev1.Pod{mountedPod("serving")}, modelWithCacheKey())
		stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

		if err := runCacheCommand(t, "list", "-n", env.namespace); err != nil {
			t.Fatalf("cache list: %v", err)
		}
		container := getCreatedPod(t, env.clientset, env.namespace,
			inspectorPodName(modelCachePVCName)).Spec.Containers[0]
		if container.Image != mirror {
			t.Errorf("image = %q, want %q", container.Image, mirror)
		}
	})

	t.Run("flag beats the environment", func(t *testing.T) {
		t.Setenv(cacheImageEnv2155, "other.local/busybox:1.37.0")
		env := cache2155Env(t, "cache-2155-image-flag", dynamicClaim(corev1.ReadWriteOnce),
			[]*corev1.Pod{mountedPod("serving")}, modelWithCacheKey())
		stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

		if err := runCacheCommand(t, "list", "-n", env.namespace, "--cache-image", mirror); err != nil {
			t.Fatalf("cache list --cache-image: %v", err)
		}
		container := getCreatedPod(t, env.clientset, env.namespace,
			inspectorPodName(modelCachePVCName)).Spec.Containers[0]
		if container.Image != mirror {
			t.Errorf("image = %q, want the flag's %q", container.Image, mirror)
		}
	})
}

// Parity: a strict RWO claim a pod holds is attached on that pod's node, so the
// inspector must land there. Passing on base and the branch alike.
func TestCacheListPinsTheInspectorForAReadWriteOnceClaim(t *testing.T) {
	env := cache2155Env(t, "cache-2155-rwo", dynamicClaim(corev1.ReadWriteOnce),
		[]*corev1.Pod{mountedPod("serving")}, modelWithCacheKey())
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

	if err := runCacheCommand(t, "list", "-n", env.namespace); err != nil {
		t.Fatalf("cache list: %v", err)
	}
	pod := getCreatedPod(t, env.clientset, env.namespace, inspectorPodName(modelCachePVCName))
	if got := pinnedTo(pod); len(got) != 1 || got[0] != "node-b" {
		t.Errorf("inspector pinned to %v, want [node-b]", got)
	}
}

// Parity: a holder whose containers have all stopped is no holder at all
// (holderTerminated), so the claim is free and the command proceeds.
func TestCacheListProceedsWhenTheHolderOfAReadWriteOncePodClaimIsTerminated(t *testing.T) {
	holder := mountedPod("serving")
	holder.Finalizers = []string{"test/hold"}

	env := cache2155Env(t, "cache-2155-rwop-terminating", dynamicClaim(corev1.ReadWriteOncePod),
		[]*corev1.Pod{holder}, modelWithCacheKey())
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

	// Stopped before it was deleted, so the pod is not running any container:
	// the claim is free and the inspector may mount it on any node.
	holder.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "manager", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
		}},
	}
	// No grace period: the kubelet stops the container as it deletes the pod,
	// which is what holderTerminated reads.
	grace := int64(0)
	if err := env.clientset.CoreV1().Pods(env.namespace).Delete(context.Background(), holder.Name,
		metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil {
		t.Fatalf("delete the holder: %v", err)
	}

	if err := runCacheCommand(t, "list", "-n", env.namespace); err != nil {
		t.Fatalf("cache list against a terminated holder: %v", err)
	}
}

// Parity: a cache pod this run may replace is not a reason to refuse: it is
// cleared and the claim becomes free.
func TestCacheListReplacesItsOwnStaleInspectorOnAReadWriteOncePodClaim(t *testing.T) {
	stale := mountedPod(inspectorPodName(modelCachePVCName))
	stale.Labels = map[string]string{
		managedByLabel:  managedByValue,
		modelCacheLabel: "cache-inspector",
	}
	env := cache2155Env(t, "cache-2155-rwop-stale", dynamicClaim(corev1.ReadWriteOncePod),
		[]*corev1.Pod{stale}, modelWithCacheKey())
	// Not kept: this run replaces the stale pod, and the test needs the
	// termination the real kubelet would ack.
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, false)

	if err := runCacheCommand(t, "list", "-n", env.namespace); err != nil {
		t.Fatalf("cache list replaced a stale inspector pod on base; got %v", err)
	}
}

// Parity: a claim that cannot be read tells nothing about its access mode, and
// an unpinned strict claim stalls on Multi-Attach, so base's pin is kept.
func TestCacheListKeepsTheBasePinWhenTheClaimReadIsRefused(t *testing.T) {
	pvc := dynamicClaim(corev1.ReadWriteOnce)
	holder := mountedPod("serving")
	c := fake.NewClientBuilder().WithScheme(cacheListScheme()).
		WithObjects(pvc, holder).Build()
	clientset := fakeclientset.NewClientset()
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

	// A claim read the caller may not make, as a namespace-scoped user without
	// RBAC on persistentvolumeclaims gets.
	forbidClaimGet = true
	t.Cleanup(func() { forbidClaimGet = false })

	if _, err := inspectPVCInfos(context.Background(), nilRESTConfig, c,
		clientset, "test-ns", []PVCInfo{{Name: modelCachePVCName}}); err != nil {
		t.Fatalf("inspectPVCInfos against a refusing claim read: %v", err)
	}
	pod := getCreatedPod(t, clientset, "test-ns", inspectorPodName(modelCachePVCName))
	if got := pinnedTo(pod); len(got) != 1 || got[0] != "node-b" {
		t.Errorf("inspector pinned to %v, want [node-b]: the access mode is unknown", got)
	}
}

// Parity: purge pods are never pinned on base, and this change does not start
// pinning them.
func TestPurgePodIsNotPinnedToTheHolder(t *testing.T) {
	env := cache2155Env(t, "cache-2155-purge-pin", dynamicClaim(corev1.ReadWriteOnce),
		[]*corev1.Pod{mountedPod("serving")}, nil)
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

	if err := runCacheCommand(t, "clear", "--force", "-n", env.namespace); err != nil {
		t.Fatalf("cache clear: %v", err)
	}
	pod := getCreatedPod(t, env.clientset, env.namespace, purgePodName(modelCachePVCName))
	if got := pinnedTo(pod); len(got) != 0 {
		t.Errorf("purge pod pinned to %v, want no pin", got)
	}
}

// Parity: with nothing configured, both cache-access pods run the image they
// always did.
func TestCacheAccessPodsDefaultToBusybox(t *testing.T) {
	t.Setenv(cacheImageEnv2155, "")

	env := cache2155Env(t, "cache-2155-default-image", dynamicClaim(corev1.ReadWriteOnce),
		[]*corev1.Pod{mountedPod("serving")}, modelWithCacheKey())
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

	if err := runCacheCommand(t, "list", "-n", env.namespace); err != nil {
		t.Fatalf("cache list: %v", err)
	}
	if got := getCreatedPod(t, env.clientset, env.namespace,
		inspectorPodName(modelCachePVCName)).Spec.Containers[0].Image; got != "busybox:1.37.0" {
		t.Errorf("inspector image = %q, want %q", got, "busybox:1.37.0")
	}

	if err := runCacheCommand(t, "clear", "--force", "-n", env.namespace); err != nil {
		t.Fatalf("cache clear: %v", err)
	}
	if got := getCreatedPod(t, env.clientset, env.namespace,
		purgePodName(modelCachePVCName)).Spec.Containers[0].Image; got != "busybox:1.37.0" {
		t.Errorf("purge image = %q, want %q", got, "busybox:1.37.0")
	}
}

// Parity: the inspector pod keeps the shape #2154 gave it: our labels, the
// restart policy, a read-only claim mount at /models, and a run stamp.
func TestCacheAccessPodShapeThroughTheCommand(t *testing.T) {
	env := cache2155Env(t, "cache-2155-shape", dynamicClaim(corev1.ReadWriteOnce),
		[]*corev1.Pod{mountedPod("serving")}, modelWithCacheKey())
	stubCacheCommandPods(t, func([]string) (string, error) { return "", nil }, true)

	if err := runCacheCommand(t, "list", "-n", env.namespace); err != nil {
		t.Fatalf("cache list: %v", err)
	}
	pod := getCreatedPod(t, env.clientset, env.namespace, inspectorPodName(modelCachePVCName))
	if pod.Labels[managedByLabel] != managedByValue || pod.Labels[modelCacheLabel] != "cache-inspector" {
		t.Errorf("inspector labels = %v, want the managed-by and cache-inspector ones", pod.Labels)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want %q", pod.Spec.RestartPolicy, corev1.RestartPolicyNever)
	}
	if c := pod.Spec.Containers[0]; !c.VolumeMounts[0].ReadOnly || !pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Error("the inspector pod mounts the claim read-write")
	}
	if pod.Labels[cachePurgeRunLabel] == "" {
		t.Error("inspector pod carries no run stamp, so a concurrent run would take it over")
	}

	if err := runCacheCommand(t, "clear", "--force", "-n", env.namespace); err != nil {
		t.Fatalf("cache clear: %v", err)
	}
	purge := getCreatedPod(t, env.clientset, env.namespace, purgePodName(modelCachePVCName))
	if purge.Labels[modelCacheLabel] != cachePurgeComponent {
		t.Errorf("purge component label = %q, want %q", purge.Labels[modelCacheLabel], cachePurgeComponent)
	}
	if purge.Spec.Containers[0].VolumeMounts[0].ReadOnly {
		t.Error("the purge pod mounts the claim read-only")
	}
}

// Seams this change introduces, declared here so the file compiles on base,
// where production never reads them.
var (
	// forbidClaimGet makes every cache-claim read fail, as a namespace-scoped
	// caller without RBAC on persistentvolumeclaims gets.
	forbidClaimGet bool
)
