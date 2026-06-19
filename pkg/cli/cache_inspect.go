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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
)

const (
	modelCachePVCName     = "llmkube-model-cache"
	defaultModelMountPath = "/models"
	cacheComponentLabel   = "app.kubernetes.io/component"
	cacheComponentValue   = "model-cache"
)

// PVCCacheEntry represents a cache entry discovered on a PVC.
type PVCCacheEntry struct {
	CacheKey  string
	SizeBytes int64
	// OwnerService is the InferenceService that owns this PVC (empty for shared cache).
	OwnerService string
}

// inspectPVCCache discovers all model-cache PVCs in the namespace (by label),
// inspects each one via a transient inspector pod, and aggregates entries with
// the owning InferenceService attached.
func inspectPVCCache(
	ctx context.Context, cfg *rest.Config, k8sClient client.Client, namespace string,
) ([]PVCCacheEntry, error) {
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := k8sClient.List(ctx, pvcList,
		client.InNamespace(namespace),
		client.MatchingLabels{cacheComponentLabel: cacheComponentValue},
	); err != nil {
		return nil, fmt.Errorf("failed to list model-cache PVCs: %w", err)
	}

	if len(pvcList.Items) == 0 {
		return nil, nil
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	// Build a map of PVC name -> owning InferenceService.
	pvcOwnerMap := buildPVCOwnerMap(ctx, k8sClient, namespace, pvcList.Items)

	var allEntries []PVCCacheEntry
	for _, pvc := range pvcList.Items {
		entries, err := inspectSinglePVC(ctx, cfg, clientset, namespace, pvc.Name, pvcOwnerMap[pvc.Name])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not inspect PVC %q: %v\n", pvc.Name, err)
			continue
		}
		allEntries = append(allEntries, entries...)
	}

	return allEntries, nil
}

// buildPVCOwnerMap returns a map from PVC name to the owning InferenceService
// name. For per-isvc caches the owner ref points to the InferenceService; for
// the shared cache the value is the empty string.
func buildPVCOwnerMap(
	ctx context.Context, k8sClient client.Client, namespace string,
	pvcs []corev1.PersistentVolumeClaim,
) map[string]string {
	ownerMap := make(map[string]string, len(pvcs))

	// First pass: check owner references.
	for i := range pvcs {
		pvc := &pvcs[i]
		for _, ref := range pvc.OwnerReferences {
			if ref.Kind == "InferenceService" && ref.Controller != nil && *ref.Controller {
				ownerMap[pvc.Name] = ref.Name
				break
			}
		}
	}

	// Second pass: for PVCs without an owner ref (shared cache or legacy),
	// derive the owner from the naming convention <isvc>-model-cache.
	for i := range pvcs {
		pvc := &pvcs[i]
		if _, ok := ownerMap[pvc.Name]; ok {
			continue
		}
		// Check if this PVC name matches <isvc>-model-cache pattern.
		if isvcName := extractISVCFromPVCName(pvc.Name); isvcName != "" {
			// Verify the InferenceService exists in this namespace.
			var isvc inferencev1alpha1.InferenceService
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: isvcName, Namespace: namespace}, &isvc); err == nil {
				ownerMap[pvc.Name] = isvcName
				continue
			}
		}
		// Shared cache or unrecognised name: no owner.
		ownerMap[pvc.Name] = ""
	}

	return ownerMap
}

// extractISVCFromPVCName returns the InferenceService name from a PVC named
// "<isvc>-model-cache", or an empty string if the pattern doesn't match.
func extractISVCFromPVCName(pvcName string) string {
	const suffix = "-model-cache"
	if strings.HasSuffix(pvcName, suffix) {
		return strings.TrimSuffix(pvcName, suffix)
	}
	return ""
}

// inspectSinglePVC creates a transient inspector pod for a single PVC, runs du,
// and returns the parsed entries tagged with the owning InferenceService.
func inspectSinglePVC(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, pvcName, ownerService string,
) ([]PVCCacheEntry, error) {
	// Check if there's already a running pod that mounts this PVC.
	podList := &corev1.PodList{}
	if _, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		containerName := findContainerWithPVC(pod, pvcName)
		if containerName != "" {
			mountPath := findMountPathForPVC(pod, containerName, pvcName)
			output, err := execInPod(ctx, cfg, clientset, namespace, pod.Name, containerName,
				[]string{"sh", "-c", fmt.Sprintf("du -sb %s/*/ 2>/dev/null || true", mountPath)})
			if err != nil {
				return nil, fmt.Errorf("failed to exec in pod %q: %w", pod.Name, err)
			}
			entries := parseDuOutput(output)
			for i := range entries {
				entries[i].OwnerService = ownerService
			}
			return entries, nil
		}
	}

	// No running pod found; create a transient inspector pod.
	podName, err := createInspectorPodForPVC(ctx, clientset, namespace, pvcName)
	if err != nil {
		return nil, fmt.Errorf("failed to create inspector pod: %w", err)
	}
	defer deleteInspectorPod(context.Background(), clientset, namespace, podName)

	if err := waitForPodRunning(ctx, clientset, namespace, podName, 120*time.Second); err != nil {
		return nil, fmt.Errorf("inspector pod failed to start: %w", err)
	}

	output, err := execInPod(ctx, cfg, clientset, namespace, podName, "inspector",
		[]string{"sh", "-c", fmt.Sprintf("du -sb %s/*/ 2>/dev/null || true", defaultModelMountPath)})
	if err != nil {
		return nil, fmt.Errorf("failed to exec in pod: %w", err)
	}

	entries := parseDuOutput(output)
	for i := range entries {
		entries[i].OwnerService = ownerService
	}
	return entries, nil
}

// findContainerWithPVC returns the name of the first container (regular or init)
// that mounts the given PVC claim name.
func findContainerWithPVC(pod *corev1.Pod, claimName string) string {
	for _, c := range pod.Spec.Containers {
		for _, vm := range c.VolumeMounts {
			if vol := findVolumeByName(pod, vm.Name); vol != nil &&
				vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == claimName {
				return c.Name
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for _, vm := range c.VolumeMounts {
			if vol := findVolumeByName(pod, vm.Name); vol != nil &&
				vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == claimName {
				return c.Name
			}
		}
	}
	return ""
}

// findMountPathForPVC returns the mount path of the volume that backs the given
// PVC claim name, for the specified container.
func findMountPathForPVC(pod *corev1.Pod, containerName, claimName string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		for _, vm := range c.VolumeMounts {
			if vol := findVolumeByName(pod, vm.Name); vol != nil &&
				vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == claimName {
				return vm.MountPath
			}
		}
	}
	return defaultModelMountPath
}

func findVolumeByName(pod *corev1.Pod, volumeName string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == volumeName {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// findPodWithCachePVC finds a running pod that mounts the shared cache PVC.
// Kept for backward compatibility with existing tests.
func findPodWithCachePVC(ctx context.Context, k8sClient client.Client, namespace string) (*corev1.Pod, string, error) {
	podList := &corev1.PodList{}
	if err := k8sClient.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, "", fmt.Errorf("failed to list pods: %w", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == modelCachePVCName {
				containerName := findContainerWithVolume(pod, vol.Name)
				if containerName != "" {
					return pod, containerName, nil
				}
			}
		}
	}

	return nil, "", nil
}

func findContainerWithVolume(pod *corev1.Pod, volumeName string) string {
	for _, c := range pod.Spec.Containers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == volumeName {
				return c.Name
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == volumeName {
				return c.Name
			}
		}
	}
	return ""
}

func findMountPath(pod *corev1.Pod, containerName string) string {
	return findMountPathForPVC(pod, containerName, modelCachePVCName)
}

func createInspectorPod(ctx context.Context, clientset kubernetes.Interface, namespace string) (string, error) {
	return createInspectorPodForPVC(ctx, clientset, namespace, modelCachePVCName)
}

// createInspectorPodForPVC creates a transient inspector pod that mounts the
// given PVC claim name.
func createInspectorPodForPVC(
	ctx context.Context, clientset kubernetes.Interface,
	namespace, pvcName string,
) (string, error) {
	podName := "llmkube-cache-inspector"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "llmkube-cli",
				"app.kubernetes.io/component":  "cache-inspector",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "inspector",
					Image:   "busybox:1.37.0",
					Command: []string{"sleep", "300"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "model-cache",
							MountPath: defaultModelMountPath,
							ReadOnly:  true,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "model-cache",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
							ReadOnly:  true,
						},
					},
				},
			},
		},
	}

	_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return podName, nil
}

func waitForPodRunning(
	ctx context.Context, clientset kubernetes.Interface,
	namespace, name string, timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return fmt.Errorf("pod %s entered phase %s", name, pod.Status.Phase)
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timed out waiting for pod %s to be running", name)
}

func deleteInspectorPod(ctx context.Context, clientset kubernetes.Interface, namespace, name string) {
	gracePeriod := int64(0)
	_ = clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
}

func execInPod(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, podName, containerName string, command []string,
) (string, error) {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, k8sscheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.String(), nil
}

func parseDuOutput(output string) []PVCCacheEntry {
	lines := strings.Split(output, "\n")
	entries := make([]PVCCacheEntry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}

		sizeBytes, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			continue
		}

		path := strings.TrimSpace(parts[1])
		path = strings.TrimSuffix(path, "/")
		cacheKey := filepath.Base(path)
		if cacheKey == "" || cacheKey == "." || cacheKey == "/" {
			continue
		}

		entries = append(entries, PVCCacheEntry{
			CacheKey:  cacheKey,
			SizeBytes: sizeBytes,
		})
	}
	return entries
}
