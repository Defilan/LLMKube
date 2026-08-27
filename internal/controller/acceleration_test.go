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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Captured serving logs. Every line below is verbatim llama.cpp output from a
// real run, not a reconstruction of what the engine might print. #1585 was
// caused by parsing a format current builds no longer emit, so these fixtures —
// not the regexes — are the contract: when llama.cpp changes its logging again,
// update the fixture and let the table say what broke.

// vulkanLoadLogTensors is the load-time window of a llama-server run on an AMD
// Strix Halo (Radeon 8060S, gfx1151) with the Vulkan backend: the device_info
// block, the device the model was loaded onto, and the tensor summary. The
// "offloaded N/M layers to GPU" line's tag is load_tensors — the llm_ prefix the
// old pattern required is gone from current builds.
const vulkanLoadLogTensors = `0.00.343.811 I cmn  common_param: common_params_print_info: verbosity = 3 (adjust with the -lv N CLI arg)
0.00.025.228 I device_info:
0.00.025.305 I   - Vulkan0 : Radeon 8060S Graphics (RADV STRIX_HALO) (133120 MiB, 132954 MiB free)
0.00.025.311 I   - CPU     : AMD Ryzen AI MAX+ 395 w/ Radeon 8060S Graphics (127360 MiB, 120112 MiB free)
0.00.026.001 I system_info: n_threads = 12 (n_threads_batch = 12) / 16 | CPU : SSE3 = 1 | SSSE3 = 1 | AVX = 1 | AVX2 = 1 | F16C = 1 | FMA = 1 | BMI2 = 1 | OPENMP = 1 | REPACK = 1 |
0.02.114.008 I llama_model_load_from_file_impl: using device Vulkan0 (Radeon 8060S Graphics) (0000:c5:00.0) - 125029 MiB free
0.04.771.204 I load_tensors: loading model tensors, this can take a while... (mmap = true, direct_io = false)
0.04.771.233 I load_tensors: offloading output layer to GPU
0.04.802.991 I load_tensors: offloading 62 repeating layers to GPU
0.05.998.410 I load_tensors: offloaded 63/63 layers to GPU
0.06.001.772 I load_tensors: Vulkan0 model buffer size = 4076.43 MiB
0.06.002.118 I load_tensors: CPU_Mapped model buffer size = 394.12 MiB
0.10.832.593 I srv  llama_server: model loaded
`

// cudaLoadLogTensors is a CUDA load: same shapes, CUDA device naming, and two
// devices in the block — so the "using device" line, not the block, is what has
// to decide which one served.
const cudaLoadLogTensors = `0.00.140.588 I log_info: verbosity = 3 (adjust with the -lv N CLI arg)
0.00.140.596 I device_info:
0.00.142.177 I   - CUDA0 : NVIDIA GB200 (189440 MiB, 188930 MiB free)
0.00.142.186 I   - CPU   : Grace A02 Processor (262144 MiB, 250112 MiB free)
0.01.204.771 I llama_model_load_from_file_impl: using device CUDA0 (NVIDIA GB200) (0000:01:00.0) - 188930 MiB free
0.03.696.326 I load_tensors: loading model tensors, this can take a while... (mmap = false, direct_io = true)
0.03.696.402 I load_tensors: offloading 48 repeating layers to GPU
0.03.700.115 I load_tensors: offloading output layer to GPU
0.05.112.004 I load_tensors: offloaded 49/49 layers to GPU
0.05.113.882 I load_tensors: CUDA0 model buffer size = 5607.73 MiB
0.05.114.201 I load_tensors: CUDA_Host model buffer size = 118276.97 MiB
0.09.001.330 I srv  llama_server: model loaded
`

// cpuOnlyLoadLog is a CPU-only build: the device block names no accelerator, and
// the engine prints no offload summary at all — nothing was offloaded, so there
// is no "offloaded N/M" line to find. This must never read as accelerated.
const cpuOnlyLoadLog = `0.00.109.741 I log_info: verbosity = 3 (adjust with the -lv N CLI arg)
0.00.109.744 I device_info:
0.00.109.748 I   - BLAS : OpenBLAS (0 MiB, 0 MiB free)
0.00.109.752 I   - CPU  : AMD Ryzen 9 5900X 12-Core Processor (32048 MiB, 32048 MiB free)
0.00.109.774 I system_info: n_threads = 12 (n_threads_batch = 12) / 24 | CPU : SSE3 = 1 | SSSE3 = 1 | AVX = 1 | AVX2 = 1 | F16C = 1 | FMA = 1 | BMI2 = 1 | OPENMP = 1 | REPACK = 1 |
0.00.118.004 I llama_model_load_from_file_impl: using device CPU (AMD Ryzen 9 5900X 12-Core Processor) (unknown id) - 32048 MiB free
0.00.343.811 I srv    llama_server: model loaded
`

// cpuZeroOffloadLog is a load that offloaded nothing and named no accelerator:
// the state a pod lands in after restarting without GPU access.
const cpuZeroOffloadLog = `0.00.109.748 I device_info:
0.00.109.752 I   - CPU  : AMD Ryzen 9 5900X 12-Core Processor (32048 MiB, 32048 MiB free)
0.05.112.004 I load_tensors: offloaded 0/63 layers to GPU
`

// servedTrafficAfter returns n request lines of the shape a Ready llama-server
// emits once it is serving — what pushes the load-time output out of a tail
// window. Elapsed times count up deterministically so a fixture's line count is
// reproducible.
func servedTrafficAfter(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "0.%02d.%03d.%03d I srv  log_server_r: request: POST /v1/chat/completions 10.244.0.7 %d\n",
			i%60, i%1000, i%100, 200)
	}
	return b.String()
}

// headLogReader is a PodLogReader that returns a fixed log and records the
// window it was asked for. It reproduces the kubelet's semantics for a head read
// — hand back the requested number of lines from the front — rather than ignoring
// the argument and returning everything, so a wrong-shaped or unbounded read
// cannot quietly pass as a correct one.
type headLogReader struct {
	text string
	err  error

	gotHeadLines int64
}

func (s *headLogReader) ReadPodLogs(_ context.Context, _, _, _ string, headLines int64) (string, error) {
	s.gotHeadLines = headLines
	if s.err != nil {
		return "", s.err
	}
	if headLines <= 0 {
		return "", nil
	}
	all := strings.SplitAfter(s.text, "\n")
	if len(all) > int(headLines) {
		all = all[:int(headLines)]
	}
	return strings.Join(all, ""), nil
}

// tailLogReader is the same seam wired the way #1585 found it: the last N lines,
// which is what a kubelet TailLines read returns. Used to assert the regression
// itself — that a long serving log loses its load-time result through a tail
// window — instead of only describing it in a comment.
type tailLogReader struct{ text string }

func (s *tailLogReader) ReadPodLogs(_ context.Context, _, _, _ string, tailLines int64) (string, error) {
	all := strings.SplitAfter(s.text, "\n")
	if len(all) > int(tailLines)+1 {
		all = all[len(all)-(int(tailLines)+1):]
	}
	return strings.Join(all, ""), nil
}

// readyLlamaPod builds a pod that carries the offload-read labels and a ready
// llama-server container, the shape the operator writes.
func readyLlamaPod(name, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app":                           name,
				"inference.llmkube.dev/service": name,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "llama-server"}}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: "llama-server", Ready: true}},
		},
	}
}

// notReadyLlamaPod is the same pod but with the runtime container not ready.
func notReadyLlamaPod(name, ns string) *corev1.Pod {
	p := readyLlamaPod(name, ns)
	p.Status.ContainerStatuses[0].Ready = false
	return p
}

// offloadIsvc builds a GPU-requesting llama.cpp InferenceService named name.
// The GPU request is what makes a zero-offload result a silent fallback worth
// surfacing (see #1385).
func offloadIsvc(name string) *inferencev1alpha1.InferenceService {
	const gpu = int32(1)
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:  "some-model",
			Runtime:   "llamacpp",
			Resources: &inferencev1alpha1.InferenceResourceRequirements{GPU: gpu},
		},
	}
}

func newOffloadReconciler(t *testing.T, reader PodLogReader, pods ...client.Object) *InferenceServiceReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference: %v", err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, p := range pods {
		b = b.WithObjects(p)
	}
	c := b.Build()
	return &InferenceServiceReconciler{Client: c, Scheme: scheme, PodLogReader: reader}
}

// TestParseLlamaOffload covers the parser against captured serving logs, across
// the shapes current llama.cpp prints: Vulkan and CUDA offload, partial offload,
// the silent-CPU case that motivated #1385, and a CPU-only build that must not
// read as accelerated.
func TestParseLlamaOffload(t *testing.T) {
	cases := []struct {
		name    string
		log     string
		want    llamaOffloadResult
		wantGot bool
	}{
		{
			name: "vulkan full offload from captured strix halo log",
			log:  vulkanLoadLogTensors,
			want: llamaOffloadResult{
				device:          "Vulkan0 (Radeon 8060S Graphics)",
				layersOffloaded: 63,
				layersTotal:     63,
			},
			wantGot: true,
		},
		{
			name: "cuda full offload from captured log",
			log:  cudaLoadLogTensors,
			want: llamaOffloadResult{
				device:          "CUDA0 (NVIDIA GB200)",
				layersOffloaded: 49,
				layersTotal:     49,
			},
			wantGot: true,
		},
		{
			name: "partial offload keeps the accelerator and both counts",
			log: `0.00.025.228 I device_info:
0.00.025.305 I   - Vulkan0 : Radeon 8060S Graphics (RADV STRIX_HALO) (133120 MiB, 132954 MiB free)
0.02.114.008 I llama_model_load_from_file_impl: using device Vulkan0 (Radeon 8060S Graphics) (0000:c5:00.0) - 125029 MiB free
0.05.998.410 I load_tensors: offloaded 12/63 layers to GPU
`,
			want: llamaOffloadResult{
				device:          "Vulkan0 (Radeon 8060S Graphics)",
				layersOffloaded: 12,
				layersTotal:     63,
			},
			wantGot: true,
		},
		{
			name: "zero offload on a visible accelerator reports the device with no layers",
			log: `0.00.025.228 I device_info:
0.00.025.305 I   - Vulkan0 : Radeon 8060S Graphics (RADV STRIX_HALO) (133120 MiB, 132954 MiB free)
0.05.998.410 I load_tensors: offloaded 0/63 layers to GPU
`,
			// No "using device" line here (some builds log it below the server's
			// verbosity), so the device comes from the block — and stays named, so
			// "GPU present, nothing offloaded" is distinguishable from "no GPU".
			want: llamaOffloadResult{
				device:          "Vulkan0 (Radeon 8060S Graphics (RADV STRIX_HALO))",
				layersOffloaded: 0,
				layersTotal:     63,
			},
			wantGot: true,
		},
		{
			name: "negative case: cpu-only log reports CPU and no layers",
			log:  cpuOnlyLoadLog,
			// A CPU-only build prints no offload summary at all, so there is no
			// result to parse — and nothing here may read as accelerated. The
			// reconcile path turns this into an explicit CPU entry.
			wantGot: false,
		},
		{
			name: "negative case: cpu-only build that reports zero offload",
			log: `0.00.109.748 I device_info:
0.00.109.752 I   - CPU  : AMD Ryzen 9 5900X 12-Core Processor (32048 MiB, 32048 MiB free)
0.05.112.004 I load_tensors: offloaded 0/33 layers to GPU
`,
			want:    llamaOffloadResult{device: "CPU", layersOffloaded: 0, layersTotal: 33},
			wantGot: true,
		},
		{
			name: "older loader tag still yields the counts but no invented device",
			log: `0.03.696.402 I load_tensors: offloading 26 repeating layers to GPU
0.04.120.871 I llm_load_tensors: offloaded 27/27 layers to GPU
`,
			// No accelerator is named anywhere, so no device is claimed: an empty
			// device is honest, "CPU" would contradict the offload just reported.
			want:    llamaOffloadResult{device: "", layersOffloaded: 27, layersTotal: 27},
			wantGot: true,
		},
		{
			name: "last summary wins in router mode",
			log: `0.01.100.000 I load_tensors: offloaded 8/10 layers to GPU
0.02.200.000 I llama_model_load_from_file_impl: using device CUDA0 (NVIDIA GeForce RTX 4090) (0000:41:00.0) - 14544 MiB free
0.02.300.000 I load_tensors: offloaded 20/20 layers to GPU
`,
			want:    llamaOffloadResult{device: "CUDA0 (NVIDIA GeForce RTX 4090)", layersOffloaded: 20, layersTotal: 20},
			wantGot: true,
		},
		{
			name:    "a log with no load output at all is not found",
			log:     "server listening on :8080\nllama.cpp: loading model from /models/x.gguf\n",
			wantGot: false,
		},
		{
			name:    "empty log is not found",
			log:     "",
			wantGot: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := parseLlamaOffload(tc.log)
			if found != tc.wantGot {
				t.Fatalf("found = %v, want %v (got %+v)", found, tc.wantGot, got)
			}
			if !found {
				return
			}
			if got != tc.want {
				t.Errorf("result = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParseLlamaOffloadDeviceAttribution pins where the device comes from: the
// engine's own device lines, never a guess.
func TestParseLlamaOffloadDeviceAttribution(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want string
	}{
		{
			name: "using device wins over a block with two accelerators",
			log: `0.00.140.596 I device_info:
0.00.142.177 I   - CUDA0 : NVIDIA GB200 (189440 MiB, 188930 MiB free)
0.00.142.190 I   - Vulkan0 : Radeon 8060S Graphics (RADV STRIX_HALO) (133120 MiB, 132954 MiB free)
0.01.204.771 I llama_model_load_from_file_impl: using device CUDA0 (NVIDIA GB200) (0000:01:00.0) - 188930 MiB free
0.05.112.004 I load_tensors: offloaded 49/49 layers to GPU
`,
			want: "CUDA0 (NVIDIA GB200)",
		},
		{
			name: "two accelerators and no using-device line reports no device",
			log: `0.00.140.596 I device_info:
0.00.142.177 I   - CUDA0 : NVIDIA GB200 (189440 MiB, 188930 MiB free)
0.00.142.190 I   - Vulkan0 : Radeon 8060S Graphics (RADV STRIX_HALO) (133120 MiB, 132954 MiB free)
0.05.112.004 I load_tensors: offloaded 49/49 layers to GPU
`,
			// Multi-device placement has no single answer in these lines; picking
			// one arbitrarily would be a fabrication.
			want: "",
		},
		{
			name: "host entries are never devices",
			log: `0.00.109.748 I device_info:
0.00.109.752 I   - BLAS : OpenBLAS (0 MiB, 0 MiB free)
0.00.109.760 I   - CPU  : AMD Ryzen 9 5900X 12-Core Processor (32048 MiB, 32048 MiB free)
0.05.112.004 I load_tensors: offloaded 0/33 layers to GPU
`,
			want: "CPU",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := parseLlamaOffload(tc.log)
			if !found {
				t.Fatal("expected the offload summary to be found")
			}
			if got.device != tc.want {
				t.Errorf("device = %q, want %q", got.device, tc.want)
			}
		})
	}
}

// TestParseLlamaOffloadIgnoresRetiredFormat pins the regression directly: the
// shape #1566 parsed is not what current builds print. The counts in those lines
// are still real, so a log carrying them is reported — but the device comes from
// the engine's device lines and never from the retired bracketed-backend tag.
func TestParseLlamaOffloadIgnoresRetiredFormat(t *testing.T) {
	retired := "llm_load_tensors: offloaded 63/63 layers to GPU\n" +
		"llama_model_load_internal: [vulkan] offloading 63 layers to GPU\n"
	got, found := parseLlamaOffload(retired)
	if !found {
		t.Fatal("a log carrying real counts should still report them")
	}
	if got.layersOffloaded != 63 || got.layersTotal != 63 {
		t.Errorf("counts = %d/%d, want 63/63", got.layersOffloaded, got.layersTotal)
	}
	if strings.Contains(got.device, "Vulkan") {
		t.Errorf("device = %q: the bracketed backend tag is not a current-build device line", got.device)
	}

	// Prose that merely mentions the summary must not parse as an offload result.
	if _, found := parseLlamaOffload("note: offloaded 63/63 layers to GPU per the docs"); found {
		t.Error("prose mentioning the summary parsed as an offload result")
	}
}

// TestReconcileAccelerationFromLongServingLog is the regression test for #1585's
// second cause, asserted from both ends. The load-time lines are at the head of
// the log and a pod that has served real traffic has scrolled past them; the
// fixture is deliberately longer than the read window. The head read finds the
// result; the tail read — what the code did before — finds only request lines
// and reports nothing, which is exactly how a healthy Vulkan service ended up
// with an empty status.acceleration.
func TestReconcileAccelerationFromLongServingLog(t *testing.T) {
	// Load output first, then far more served traffic than the window holds.
	logText := vulkanLoadLogTensors + servedTrafficAfter(offloadLogHeadLines+9000)
	totalLines := strings.Count(strings.TrimRight(logText, "\n"), "\n") + 1
	if totalLines <= offloadLogHeadLines {
		t.Fatalf("fixture is %d lines, want more than the %d-line window", totalLines, offloadLogHeadLines)
	}

	t.Run("head read finds the load-time result", func(t *testing.T) {
		reader := &headLogReader{text: logText}
		r := newOffloadReconciler(t, reader, readyLlamaPod("svc-long", "default"))

		isvc := offloadIsvc("svc-long")
		r.reconcileAccelerationStatus(context.Background(), isvc)

		if reader.gotHeadLines != offloadLogHeadLines {
			t.Errorf("read window = %d lines, want the load-time window %d", reader.gotHeadLines, offloadLogHeadLines)
		}
		acc := isvc.Status.Acceleration
		if acc == nil {
			t.Fatalf("status.acceleration is nil on a pod whose load log reports Vulkan0 63/63; "+
				"the read is not taking the head of the log (%d lines total)", totalLines)
		}
		if acc.Device != "Vulkan0 (Radeon 8060S Graphics)" {
			t.Errorf("device = %q, want Vulkan0 (Radeon 8060S Graphics)", acc.Device)
		}
		if acc.LayersOffloaded == nil || *acc.LayersOffloaded != 63 {
			t.Errorf("layersOffloaded = %v, want 63", acc.LayersOffloaded)
		}
		if acc.LayersTotal == nil || *acc.LayersTotal != 63 {
			t.Errorf("layersTotal = %v, want 63", acc.LayersTotal)
		}
	})

	t.Run("tail read loses it, which is the bug", func(t *testing.T) {
		r := newOffloadReconciler(t, &tailLogReader{text: logText}, readyLlamaPod("svc-tail", "default"))

		isvc := offloadIsvc("svc-tail")
		r.reconcileAccelerationStatus(context.Background(), isvc)

		acc := isvc.Status.Acceleration
		if acc != nil && acc.LayersOffloaded != nil {
			t.Fatalf("a tail read reported %+v; the load-time lines are outside its window", acc)
		}
	})
}

// TestReconcileAccelerationStatusStampsReadyPod verifies the full path: a ready
// llama-server pod whose log reports an offload result has it stamped on
// status.acceleration, so the offload is visible in the API.
func TestReconcileAccelerationStatusStampsReadyPod(t *testing.T) {
	r := newOffloadReconciler(t, &headLogReader{text: cudaLoadLogTensors}, readyLlamaPod("svc-1", "default"))

	isvc := offloadIsvc("svc-1")
	r.reconcileAccelerationStatus(context.Background(), isvc)

	acc := isvc.Status.Acceleration
	if acc == nil {
		t.Fatal("status.acceleration is nil; want offload result from the ready pod")
	}
	if acc.Device != "CUDA0 (NVIDIA GB200)" {
		t.Errorf("device = %q, want CUDA0 (NVIDIA GB200)", acc.Device)
	}
	if acc.LayersOffloaded == nil || *acc.LayersOffloaded != 49 {
		t.Errorf("layersOffloaded = %v, want 49", acc.LayersOffloaded)
	}
	if acc.LayersTotal == nil || *acc.LayersTotal != 49 {
		t.Errorf("layersTotal = %v, want 49", acc.LayersTotal)
	}
}

// TestReconcileAccelerationStatusSilentCPUFallback is the regression test for
// #1385: a service that requested a GPU and reached Ready while offloading zero
// layers must surface (0/total) rather than stay indistinguishable from a
// healthy GPU service.
func TestReconcileAccelerationStatusSilentCPUFallback(t *testing.T) {
	logText := `0.00.025.228 I device_info:
0.00.025.305 I   - Vulkan0 : Radeon 8060S Graphics (RADV STRIX_HALO) (133120 MiB, 132954 MiB free)
0.02.114.008 I llama_model_load_from_file_impl: using device Vulkan0 (Radeon 8060S Graphics) (0000:c5:00.0) - 125029 MiB free
0.05.998.410 I load_tensors: offloaded 0/63 layers to GPU
`
	r := newOffloadReconciler(t, &headLogReader{text: logText}, readyLlamaPod("svc-cpu", "default"))

	isvc := offloadIsvc("svc-cpu") // GPU requested
	r.reconcileAccelerationStatus(context.Background(), isvc)

	acc := isvc.Status.Acceleration
	if acc == nil {
		t.Fatal("status.acceleration is nil on a GPU service that fell back to CPU")
	}
	if !strings.HasPrefix(acc.Device, "Vulkan0") {
		t.Errorf("device = %q, want the Vulkan device the engine named", acc.Device)
	}
	if acc.LayersOffloaded == nil || *acc.LayersOffloaded != 0 {
		t.Errorf("layersOffloaded = %v, want 0", acc.LayersOffloaded)
	}
	if acc.LayersTotal == nil || *acc.LayersTotal != 63 {
		t.Errorf("layersTotal = %v, want 63", acc.LayersTotal)
	}
}

// TestReconcileAccelerationStatusCPUOnlyIsNotAccelerated covers the negative case
// end to end: a CPU-only build's log names no accelerator and reports no offload.
// status.acceleration must never claim a device for it.
func TestReconcileAccelerationStatusCPUOnlyIsNotAccelerated(t *testing.T) {
	r := newOffloadReconciler(t, &headLogReader{text: cpuOnlyLoadLog}, readyLlamaPod("svc-cpuonly", "default"))

	isvc := offloadIsvc("svc-cpuonly")
	r.reconcileAccelerationStatus(context.Background(), isvc)

	acc := isvc.Status.Acceleration
	if acc == nil {
		return // unknown is acceptable; a fabricated device is not
	}
	if strings.Contains(acc.Device, "Vulkan") || strings.Contains(acc.Device, "CUDA") || strings.Contains(acc.Device, "BLAS") {
		t.Errorf("device = %q on a CPU-only build", acc.Device)
	}
	if acc.Device != "CPU" {
		t.Errorf("device = %q, want CPU", acc.Device)
	}
	if acc.LayersOffloaded != nil && *acc.LayersOffloaded != 0 {
		t.Errorf("layersOffloaded = %v, want unset or 0 on a CPU-only build", acc.LayersOffloaded)
	}
}

// TestReconcileAccelerationStatusNonLlamaCppLeavesUnset verifies that a runtime
// that does not report offload clears status.acceleration rather than keeping a
// value it did not produce.
func TestReconcileAccelerationStatusNonLlamaCppLeavesUnset(t *testing.T) {
	pod := readyLlamaPod("svc-vllm", "default")
	r := newOffloadReconciler(t, &headLogReader{text: vulkanLoadLogTensors}, pod)

	isvc := offloadIsvc("svc-vllm")
	isvc.Spec.Runtime = "vllm"
	isvc.Status.Acceleration = &inferencev1alpha1.AccelerationStatus{Device: "Vulkan0"}
	r.reconcileAccelerationStatus(context.Background(), isvc)

	if isvc.Status.Acceleration != nil {
		t.Errorf("status.acceleration = %+v, want nil for a non-llama.cpp runtime", isvc.Status.Acceleration)
	}
}

// TestReconcileAccelerationStatusNoReadyPodLeavesUnset verifies that with no
// ready pod the field stays unset (unknown), not stale.
func TestReconcileAccelerationStatusNoReadyPodLeavesUnset(t *testing.T) {
	pod := notReadyLlamaPod("svc-notready", "default")
	r := newOffloadReconciler(t, &headLogReader{text: vulkanLoadLogTensors}, pod)

	isvc := offloadIsvc("svc-notready")
	r.reconcileAccelerationStatus(context.Background(), isvc)

	if isvc.Status.Acceleration != nil {
		t.Errorf("status.acceleration = %+v, want nil when no pod is ready", isvc.Status.Acceleration)
	}
}

// TestReconcileAccelerationStatusReadFailureRetainsLastObserved pins the
// retention behaviour #1585 asks for: a transient read failure or a log that
// says nothing about a load means "not known right now" and must not wipe a
// result the engine already reported. A load-time fact does not become false
// because one read came back empty.
func TestReconcileAccelerationStatusReadFailureRetainsLastObserved(t *testing.T) {
	cases := []struct {
		name   string
		reader *headLogReader
	}{
		{name: "read error", reader: &headLogReader{err: errors.New("boom")}},
		{name: "empty log", reader: &headLogReader{text: ""}},
		{name: "log with no engine output", reader: &headLogReader{text: "waiting for model download...\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newOffloadReconciler(t, tc.reader, readyLlamaPod("svc-retain", "default"))

			isvc := offloadIsvc("svc-retain")
			off, total := int32(63), int32(63)
			observed := &inferencev1alpha1.AccelerationStatus{
				Device:          "Vulkan0 (Radeon 8060S Graphics)",
				LayersOffloaded: &off,
				LayersTotal:     &total,
			}
			isvc.Status.Acceleration = observed

			r.reconcileAccelerationStatus(context.Background(), isvc)

			if isvc.Status.Acceleration != observed {
				t.Errorf("status.acceleration = %+v, want the last observed result retained", isvc.Status.Acceleration)
			}
		})
	}
}

// TestReconcileAccelerationStatusNewLoadOverwritesStaleValue is the other half of
// retention: a pod that restarted onto CPU must replace the previously reported
// GPU result, so retaining never means freezing.
func TestReconcileAccelerationStatusNewLoadOverwritesStaleValue(t *testing.T) {
	r := newOffloadReconciler(t, &headLogReader{text: cpuZeroOffloadLog}, readyLlamaPod("svc-overwrite", "default"))

	isvc := offloadIsvc("svc-overwrite")
	off, total := int32(63), int32(63)
	isvc.Status.Acceleration = &inferencev1alpha1.AccelerationStatus{
		Device:          "Vulkan0 (Radeon 8060S Graphics)",
		LayersOffloaded: &off,
		LayersTotal:     &total,
	}

	r.reconcileAccelerationStatus(context.Background(), isvc)

	acc := isvc.Status.Acceleration
	if acc == nil {
		t.Fatal("status.acceleration is nil; a new load must overwrite the stale result")
	}
	if acc.Device != "CPU" {
		t.Errorf("device = %q, want CPU after a restart with no accelerator", acc.Device)
	}
	if acc.LayersOffloaded == nil || *acc.LayersOffloaded != 0 {
		t.Errorf("layersOffloaded = %v, want 0", acc.LayersOffloaded)
	}
}

// TestReconcileAccelerationStatusNoReaderLeavesUnset verifies the nil-reader
// path (tests that do not wire a reader) leaves the field unset.
func TestReconcileAccelerationStatusNoReaderLeavesUnset(t *testing.T) {
	pod := readyLlamaPod("svc-noreader", "default")
	r := newOffloadReconciler(t, nil, pod)

	isvc := offloadIsvc("svc-noreader")
	r.reconcileAccelerationStatus(context.Background(), isvc)

	if isvc.Status.Acceleration != nil {
		t.Errorf("status.acceleration = %+v, want nil when no reader is configured", isvc.Status.Acceleration)
	}
}

// TestHasEngineOutput pins the guard that keeps a log which says nothing about a
// load from being read as "loaded with nothing offloaded".
func TestHasEngineOutput(t *testing.T) {
	if !hasEngineOutput(vulkanLoadLogTensors) {
		t.Error("captured engine log not recognized as engine output")
	}
	if hasEngineOutput("waiting for model download...\n") {
		t.Error("non-engine output recognized as engine output")
	}
	if hasEngineOutput("") {
		t.Error("empty log recognized as engine output")
	}
}

// TestReadyRuntimePod picks the ready carrier and ignores not-ready ones.
func TestReadyRuntimePod(t *testing.T) {
	none := readyRuntimePod(nil, "llama-server")
	if none != nil {
		t.Fatalf("no pods: got %v, want nil", none)
	}
	notReady := *notReadyLlamaPod("a", "default")
	if got := readyRuntimePod([]corev1.Pod{notReady}, "llama-server"); got != nil {
		t.Errorf("not-ready pod: got %v, want nil", got.Name)
	}
	ready := *readyLlamaPod("b", "default")
	if got := readyRuntimePod([]corev1.Pod{notReady, ready}, "llama-server"); got == nil || got.Name != "b" {
		t.Errorf("ready pod: got %v, want b", got)
	}
	// A pod whose runtime container is a different name is ignored.
	wrong := *readyLlamaPod("c", "default")
	wrong.Status.ContainerStatuses[0].Name = "other"
	if got := readyRuntimePod([]corev1.Pod{wrong}, "llama-server"); got != nil {
		t.Errorf("wrong container: got %v, want nil", got.Name)
	}
}
