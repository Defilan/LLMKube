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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Surface actual GPU offload in InferenceService status (#1385). A service can
// report Ready and answer requests while every layer runs on CPU — the
// silent-fallback failure the operator was blind to because nothing in status
// said where the weights actually landed. The serving engine already reports
// the result at load (llama.cpp prints "load_tensors: offloaded N/M layers to
// GPU" and, just before it, the device it loaded onto); this reads that from
// the ready pod's logs and stamps it on status.acceleration so the fallback is
// visible in the API.
//
// The read is advisory and best-effort: it never fails a reconcile. When the
// offload result is unknown (no ready pod, a runtime that does not report it,
// or an unparseable log) status.acceleration keeps its last observed value, so
// a service that has already reported an offload does not lose it to a later
// read that found nothing.

// offloadLogHeadLines bounds how many leading lines of a ready pod's serving
// log the offload read inspects. The offload result is printed at load — both
// the device line and the tensor summary land within the first few dozen lines
// of llama-server's output, long before the readiness probe passes — so the
// window is taken from the head. A tail window silently loses the value on any
// pod that has served real traffic: the Vulkan pod measured in #1585 was
// 11,412 lines by the time it was checked, and its startup banner was still
// the first retained line. Bounded so the read stays cheap.
const offloadLogHeadLines = 2048

// PodLogReader reads a serving pod's container log. The operator needs this to
// surface the engine's own offload result (#1385); it is a seam so unit tests
// supply logs without a live cluster and the reader itself (the kubernetes
// clientset) stays out of the reconcile hot path.
type PodLogReader interface {
	// ReadPodLogs returns the first headLines lines of a container's log, or
	// an error if the log cannot be read. An empty string with nil error means
	// the log is present but empty.
	ReadPodLogs(ctx context.Context, namespace, podName, containerName string, headLines int64) (string, error)
}

// readPodLogs delegates to the configured PodLogReader for the load-time window
// of a serving pod's log. When no reader is configured (e.g. a test that does
// not exercise the offload path) it returns an empty string so the caller
// leaves status.acceleration untouched rather than failing the reconcile.
func (r *InferenceServiceReconciler) readPodLogs(ctx context.Context, namespace, podName, containerName string) (string, error) {
	if r.PodLogReader == nil {
		return "", nil
	}
	return r.PodLogReader.ReadPodLogs(ctx, namespace, podName, containerName, offloadLogHeadLines)
}

// llamaTimestampRe matches the prefix llama.cpp's own logger stamps on every
// line of server output: a dotted elapsed-time field and a single-letter log
// level, as in "0.10.832.593 I srv  llama_server: model loaded". Lines the
// engine wrote itself always carry it; lines captured from stdout by something
// else (a harness, a wrapper) do not.
var llamaTimestampRe = regexp.MustCompile(`^[\d.]+ [A-Z] `)

// offloadSummaryRe matches llama.cpp's per-model offload summary line, printed
// once the tensors are loaded:
//
//	0.04.123.456 I load_tensors: offloaded 63/63 layers to GPU
//
// The tag is anchored at a token boundary so it matches both the current loader
// tag and its "llm_" predecessor, but never a substring of unrelated text: bare
// matching is what let #1585 go unnoticed, since an unanchored pattern silently
// keeps matching whatever surrounds it (a captured request body, an echoed
// prompt). This is the only line carrying both the final offloaded count and the
// model's total layer count, so it decides whether an offload happened at all.
var offloadSummaryRe = regexp.MustCompile(`(?:^|[\s\]])\S*load_tensors: offloaded (\d+)/(\d+) layers to GPU`)

// deviceInfoLineRe matches one per-device line of the device block llama.cpp
// prints before loading, e.g.
//
//	0.00.025.305 I   - Vulkan0 : Radeon 8060S Graphics (RADV STRIX_HALO) (133120 MiB, 132954 MiB free)
//
// The block is what names the accelerator the engine can see. Its header
// ("device_info:") carries no device, and host entries are skipped by name in
// isAcceleratorDevice, so a CPU-only block yields nothing.
//
// The memory tail has to be consumed rather than asserted: an entry's description
// is itself parenthesized ("Radeon 8060S Graphics (RADV STRIX_HALO)"), so a
// lazy capture would stop inside it and leave the byte counts in the device name.
var deviceInfoLineRe = regexp.MustCompile(`-\s*([A-Za-z][\w-]*)\s*:\s*(.*?)\s*\(\d+ MiB, \d+ MiB free\)`)

// deviceInfoHeaderRe matches the block header, which is printed with its own tag
// ("device_info:") on every line of the block.
var deviceInfoHeaderRe = regexp.MustCompile(`(?:^|[\s\]])device_info:`)

// usingDeviceRe matches the line naming the device a model was actually loaded
// onto:
//
//	llama_model_load_from_file_impl: using device Vulkan0 (AMD Radeon 8060S Graphics) (0000:c5:00.0) - 125029 MiB free
//
// Preferred over the device block because it names the device the load used
// rather than every device present; the trailing PCI address or "(unknown id)"
// is not captured.
var usingDeviceRe = regexp.MustCompile(`using device (\S+) \(([^)]*)\)`)

// llamaOffloadResult is the parsed offload result of one serving log.
type llamaOffloadResult struct {
	device          string
	layersOffloaded int
	layersTotal     int
}

// parseLlamaOffload extracts the offload result from a llama.cpp serving log.
// It returns found=false when no offload summary line is present (the log does
// not report a result — e.g. an engine that never loaded, or a runtime the
// reader did not recognize), in which case the other fields are meaningless.
//
// device is the accelerator the engine named for the load ("Vulkan0", "CUDA0",
// …), or "CPU" when the model loaded with zero offloaded layers and no
// accelerator was involved. The latter is the silent-fallback state: the service
// is Ready and serving but every layer is on CPU.
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

	device := deviceFromLog(logText)
	if off == 0 && !hasAcceleratorName(logText) {
		// Nothing landed on an accelerator and the log names none: this is the
		// CPU fallback #1385 exists to make visible. A log that does name one
		// keeps it, so "GPU present, zero layers offloaded" stays distinguishable
		// from "no GPU at all".
		device = "CPU"
	}
	return llamaOffloadResult{device: device, layersOffloaded: off, layersTotal: total}, true
}

// hasAcceleratorName reports whether the log names an accelerator anywhere — in
// a "using device" line or in a device_info block.
func hasAcceleratorName(logText string) bool {
	return usingDeviceRe.MatchString(logText) || len(acceleratorDevicesFromDeviceInfo(logText)) > 0
}

// deviceFromLog returns the accelerator the engine reported for this load, or
// "" when the log names none. The "using device" line wins because it is the
// engine naming the device the model went onto; the device_info block is the
// fallback for builds whose load-time detail lines are logged below the server's
// verbosity. A log with several accelerator devices (multi-GPU, or a Vulkan and
// a CUDA device on the same host) has no single answer in these lines, so it
// reports none rather than guessing one — leaving status.acceleration's device
// empty is honest, picking an arbitrary device is not.
func deviceFromLog(logText string) string {
	if using := usingDeviceRe.FindAllStringSubmatch(logText, -1); len(using) > 0 {
		// Last wins: with a router-mode server that loaded several models, the
		// most recent load is the one serving now.
		last := using[len(using)-1]
		if dev := deviceFromID(last[1]); isAcceleratorDevice(dev) {
			if desc := strings.TrimSpace(last[2]); desc != "" && desc != "unknown id" {
				return dev + " (" + desc + ")"
			}
			return dev
		}
		return ""
	}

	devices := acceleratorDevicesFromDeviceInfo(logText)
	if len(devices) == 1 {
		return devices[0]
	}
	return ""
}

// acceleratorDevicesFromDeviceInfo returns the accelerator entries of the last
// device_info block in the log, formatted as "Vulkan0 (Radeon 8060S Graphics
// (RADV STRIX_HALO))". The CPU entry is skipped: it names the host, not an
// offload target, and counting it would make a CPU-only build look accelerated.
func acceleratorDevicesFromDeviceInfo(logText string) []string {
	// Scan line by line so the block boundaries are explicit: entries belong to
	// the most recent "device_info:" header, and anything after the last one is
	// unrelated log output that happens to start with a dash.
	var devices []string
	inBlock := false
	for _, line := range strings.Split(logText, "\n") {
		if deviceInfoHeaderRe.MatchString(line) {
			inBlock = true
			devices = nil
			continue
		}
		if !inBlock {
			continue
		}
		m := deviceInfoLineRe.FindStringSubmatch(line)
		if m == nil {
			// The block is contiguous: a line that is not an entry ends it.
			inBlock = false
			continue
		}
		dev := deviceFromID(m[1])
		if !isAcceleratorDevice(dev) {
			continue
		}
		if desc := strings.TrimSpace(m[2]); desc != "" {
			devices = append(devices, dev+" ("+desc+")")
			continue
		}
		devices = append(devices, dev)
	}
	return devices
}

// deviceFromID normalizes a device identifier as the engine printed it, dropping
// any ":<index>" suffix ("CUDA0:0" → "CUDA0"). An empty input yields "".
func deviceFromID(id string) string {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[:i]
	}
	return id
}

// hostDevicePrefixes are the device_info entries that name the machine or a
// CPU-side library rather than an accelerator. Counting them would make a
// CPU-only build look accelerated — the exact false positive this field must
// never produce, since its whole purpose is to distinguish "no GPU" from "GPU
// silently unused".
var hostDevicePrefixes = []string{"CPU", "BLAS"}

// isAcceleratorDevice reports whether a device_info entry names an accelerator.
// The engine's accelerator IDs are the backend name plus an index ("Vulkan0",
// "CUDA0", "HIP0", "Metal0", "RPC0"); everything else is host or library.
func isAcceleratorDevice(id string) bool {
	for _, host := range hostDevicePrefixes {
		if strings.HasPrefix(id, host) {
			return false
		}
	}
	return true
}

// hasEngineOutput reports whether a log looks like llama.cpp's own output: any
// line carries the engine's timestamp prefix, as in
// "0.10.832.593 I srv  llama_server: model loaded". A read that returned no
// engine lines (an empty log, or a container whose stdout is not the engine)
// must not be mistaken for "loaded with nothing offloaded".
func hasEngineOutput(logText string) bool {
	for _, line := range strings.Split(logText, "\n") {
		if llamaTimestampRe.MatchString(line) {
			return true
		}
	}
	return false
}

// reconcileAccelerationStatus stamps status.acceleration from the ready pod's
// serving log. It is called on the Deployment path, just before the status
// update, so the value is persisted with the phase/conditions in the same
// Status().Update.
//
// It is deliberately conservative about what it writes:
//   - a runtime that does not report offload (everything but llama.cpp) clears
//     the field,
//   - no ready pod leaves the last observed value alone, and
//   - a ready pod whose log does not report a result also leaves it alone.
//
// In all three cases the field keeps its last observed value, except for a
// non-llama.cpp runtime, which clears it because that value was never this
// runtime's to report. The offload result is a load-time fact: once the engine
// has printed it, nothing about a later reconcile makes it less true, and
// clearing on every miss made the field flap back to empty (#1585) — the same
// silence that hid the CPU fallback in #1572. A genuinely new load prints a new
// summary line and overwrites the value; a restart that loses the accelerator
// reports zero offloaded layers and overwrites it too.
func (r *InferenceServiceReconciler) reconcileAccelerationStatus(ctx context.Context, isvc *inferencev1alpha1.InferenceService) {
	// Only llama.cpp reports the offload result in the format we parse. Other
	// runtimes clear the field rather than keep a value they did not produce.
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
		return
	}

	containerName := resolveBackend(isvc).ContainerName()
	pod := readyRuntimePod(podList.Items, containerName)
	if pod == nil {
		return
	}

	logText, err := r.readPodLogs(ctx, isvc.Namespace, pod.Name, containerName)
	if err != nil || logText == "" {
		// Reading the log is advisory: a transient read failure or an empty log
		// means "not known right now", not "no acceleration". Retain.
		return
	}

	res, found := parseLlamaOffload(logText)
	if !found {
		// The window held no offload summary. If it also held none of the
		// engine's own output, the read told us nothing about a load and the
		// last observed value stands. If it did hold engine output, the engine
		// loaded without ever reporting an offload — a CPU-only build prints no
		// summary line at all — which is precisely the fallback worth naming.
		if !hasEngineOutput(logText) {
			return
		}
		device := "CPU"
		if devices := acceleratorDevicesFromDeviceInfo(logText); len(devices) == 1 {
			// An accelerator is visible but nothing was offloaded onto it: name
			// the device so a service that could have used the GPU and did not
			// stays distinguishable from one with no GPU at all.
			device = devices[0]
		}
		isvc.Status.Acceleration = &inferencev1alpha1.AccelerationStatus{Device: device}
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
