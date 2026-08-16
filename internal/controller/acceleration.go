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
	"math"
	"regexp"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Surface actual GPU offload in InferenceService status (#1385). A service can
// report Ready and answer requests while every layer runs on CPU — the
// silent-fallback failure the operator was blind to because nothing in status
// said where the weights actually landed. The serving engine already reports
// the result at load (llama.cpp prints "offloaded N/M layers to GPU" and the
// backend it offloaded to); this reads that from the ready pod's logs and
// stamps it on status.acceleration so the fallback is visible in the API.
//
// The read is advisory and best-effort: it never fails a reconcile. When the
// offload result is unknown (no ready pod, a runtime that does not report it,
// or an unparseable log) status.acceleration is left unset, which existing
// consumers already treat as "unknown" because the field is optional.

// offloadLogTailLines bounds how many trailing lines of a ready pod's serving
// log the offload read inspects. The offload summary is printed once at load,
// long before readiness, so the tail must be deep enough to still include it
// on a busy server, but bounded so the read stays cheap.
const offloadLogTailLines = 2048

// PodLogReader reads a serving pod's container log. The operator needs this to
// surface the engine's own offload result (#1385); it is a seam so unit tests
// supply logs without a live cluster and the reader itself (the kubernetes
// clientset) stays out of the reconcile hot path.
type PodLogReader interface {
	// ReadPodLogs returns the last tailLines lines of a ready container's log,
	// or an error if the log cannot be read. An empty string with nil error
	// means the log is present but not long enough to contain a tail.
	ReadPodLogs(ctx context.Context, namespace, podName, containerName string, tailLines int64) (string, error)
}

// readPodLogs delegates to the configured PodLogReader. When no reader is
// configured (e.g. a test that does not exercise the offload path) it returns
// an empty string so the caller leaves status.acceleration unset rather than
// failing the reconcile.
func (r *InferenceServiceReconciler) readPodLogs(ctx context.Context, namespace, podName, containerName string) (string, error) {
	if r.PodLogReader == nil {
		return "", nil
	}
	return r.PodLogReader.ReadPodLogs(ctx, namespace, podName, containerName, offloadLogTailLines)
}

// offloadSummaryRe matches llama.cpp's per-model offload summary line, printed
// once the tensors are loaded, e.g. "llm_load_tensors: offloaded 63/63 layers
// to GPU". This is the only line that carries the final offloaded count and
// the model's total layer count.
var offloadSummaryRe = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)

// backendOffloadRe matches llama.cpp's per-backend offload line, printed while
// layers are assigned to a device, e.g.
// "llama_model_load_internal: [vulkan] offloading 63 layers to GPU". The
// bracketed tag is the backend (vulkan, cublas, clblast, hipblas, metal); the
// count is the layers assigned to THAT backend. Only a positive count names a
// real offload target; a zero count is a backend the engine probed and skipped.
var backendOffloadRe = regexp.MustCompile(`\[(\w+)\] offloading (\d+) layers to GPU`)

// llamaOffloadResult is the parsed offload result of one serving log.
type llamaOffloadResult struct {
	device          string
	layersOffloaded int
	layersTotal     int
}

// backendDeviceNames maps llama.cpp backend tags to the device class that
// served the offloaded layers. Tags without an entry fall back to the tag
// itself (title-cased) so a future backend is still surfaced rather than
// dropped.
var backendDeviceNames = map[string]string{
	"vulkan":  "Vulkan",
	"cublas":  "CUDA",
	"clblast": "OpenCL",
	"hipblas": "ROCm",
	"metal":   "Metal",
}

// parseLlamaOffload extracts the offload result from a llama.cpp serving log.
// It returns found=false when no offload summary line is present (the log does
// not report a result — e.g. an engine that never loaded, or a runtime the
// reader did not recognize), in which case the other fields are meaningless.
//
// device is the offload backend that received the layers ("Vulkan", "CUDA", …)
// when any layer was offloaded, or "CPU" when the model loaded with zero
// offloaded layers. The latter is the silent-fallback state: the service is
// Ready and serving but every layer is on CPU.
func parseLlamaOffload(logText string) (res llamaOffloadResult, found bool) {
	// Walk the summary lines; a router-mode server loads more than one model,
	// each emitting its own line, and the most recent load is the one serving
	// now, so keep the last.
	summary := offloadSummaryRe.FindAllStringSubmatch(logText, -1)
	if len(summary) == 0 {
		return llamaOffloadResult{}, false
	}
	last := summary[len(summary)-1]
	off, _ := strconv.Atoi(last[1])
	total, _ := strconv.Atoi(last[2])

	device := "CPU"
	if off > 0 {
		// A positive offload: attribute it to the backend that received the
		// layers. Take the last positive-count backend line, mirroring the
		// last-summary choice above.
		backends := backendOffloadRe.FindAllStringSubmatch(logText, -1)
		for i := len(backends) - 1; i >= 0; i-- {
			n, _ := strconv.Atoi(backends[i][2])
			if n <= 0 {
				continue
			}
			tag := backends[i][1]
			if name, ok := backendDeviceNames[tag]; ok {
				device = name
			} else {
				device = titleWord(tag)
			}
			break
		}
	}
	return llamaOffloadResult{device: device, layersOffloaded: off, layersTotal: total}, true
}

// titleWord uppercases the first rune of a backend tag and leaves the rest, so
// an unmapped tag ("somebackend") still yields a readable device ("Somebackend")
// rather than a lowercased blob.
func titleWord(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// reconcileAccelerationStatus stamps status.acceleration from the ready pod's
// serving log. It is called on the Deployment path, just before the status
// update, so the value is persisted with the phase/conditions in the same
// Status().Update.
//
// It is deliberately conservative about what it writes:
//   - a runtime that does not report offload (everything but llama.cpp) leaves
//     the field unset,
//   - no ready pod leaves it unset, and
//   - a ready pod whose log does not report a result leaves it unset.
//
// In all three cases the field is the honest "unknown" (nil), so a service
// that stops serving does not keep advertising a device it is no longer on.
func (r *InferenceServiceReconciler) reconcileAccelerationStatus(ctx context.Context, isvc *inferencev1alpha1.InferenceService) {
	// Only llama.cpp reports the offload result in the format we parse. Other
	// runtimes leave the field unset rather than guess.
	switch resolveBackend(isvc).(type) {
	case *LlamaCppBackend, *LlamaCppRouterBackend:
	default:
		isvc.Status.Acceleration = nil
		return
	}

	podList := &corev1.PodList{}
	labels := client.MatchingLabels{
		"app":                           isvc.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}
	if err := r.List(ctx, podList, client.InNamespace(isvc.Namespace), labels); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list pods for offload read")
		isvc.Status.Acceleration = nil
		return
	}

	containerName := resolveBackend(isvc).ContainerName()
	pod := readyRuntimePod(podList.Items, containerName)
	if pod == nil {
		isvc.Status.Acceleration = nil
		return
	}

	logText, err := r.readPodLogs(ctx, isvc.Namespace, pod.Name, containerName)
	if err != nil || logText == "" {
		// Reading the log is advisory: a transient read failure must not
		// clobber a previously-observed result, and an empty read means the
		// engine has not printed its load line yet. Leave the field unset.
		isvc.Status.Acceleration = nil
		return
	}

	res, found := parseLlamaOffload(logText)
	if !found {
		isvc.Status.Acceleration = nil
		return
	}
	off := clampInt32(res.layersOffloaded)
	total := clampInt32(res.layersTotal)
	isvc.Status.Acceleration = &inferencev1alpha1.AccelerationStatus{
		Device:          res.device,
		LayersOffloaded: &off,
		LayersTotal:     &total,
	}
}

// readyRuntimePod returns the first pod whose runtime container (the one named
// containerName) is Ready, or nil. The offload read is keyed on readiness
// because that is when the engine has finished loading and printed the
// offload summary; a not-ready pod may be mid-load and its log would understate
// the offload.
func readyRuntimePod(pods []corev1.Pod, containerName string) *corev1.Pod {
	for _, pod := range pods {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == containerName && cs.Ready {
				return &pod
			}
		}
	}
	return nil
}

// clampInt32 converts an int to int32, saturating at the int32 bounds. A layer
// count is small in practice, but the value comes from the container's own log
// text, so a corrupt or adversarial log must not panic the controller with an
// overflow; saturation keeps the field a sane non-negative count.
func clampInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
}
