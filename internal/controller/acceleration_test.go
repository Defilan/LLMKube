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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// stubLogReader is a PodLogReader that returns a fixed log, so the offload
// read is exercised without a live cluster.
type stubLogReader struct {
	text string
	err  error
}

func (s *stubLogReader) ReadPodLogs(_ context.Context, _, _, _ string, _ int64) (string, error) {
	return s.text, s.err
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

// TestParseLlamaOffload covers the log parser across the shapes llama.cpp
// actually prints, including the silent-CPU case (zero offloaded layers) that
// motivated #1385.
func TestParseLlamaOffload(t *testing.T) {
	cases := []struct {
		name    string
		log     string
		want    llamaOffloadResult
		wantGot bool
	}{
		{
			name:    "full Vulkan offload",
			log:     "llm_load_tensors: offloaded 63/63 layers to GPU\nllama_model_load_internal: [vulkan] offloading 63 layers to GPU\n",
			want:    llamaOffloadResult{device: "Vulkan", layersOffloaded: 63, layersTotal: 63},
			wantGot: true,
		},
		{
			name:    "partial offload keeps the GPU device",
			log:     "llm_load_tensors: offloaded 12/63 layers to GPU\nllama_model_load_internal: [cublas] offloading 12 layers to GPU\n",
			want:    llamaOffloadResult{device: "CUDA", layersOffloaded: 12, layersTotal: 63},
			wantGot: true,
		},
		{
			name:    "silent CPU fallback reports CPU",
			log:     "llm_load_tensors: offloaded 0/63 layers to GPU\nllama_model_load_internal: [vulkan] offloading 0 layers to GPU\n",
			want:    llamaOffloadResult{device: "CPU", layersOffloaded: 0, layersTotal: 63},
			wantGot: true,
		},
		{
			name:    "zero offload with no backend line still reports CPU",
			log:     "llm_load_tensors: offloaded 0/35 layers to GPU\n",
			want:    llamaOffloadResult{device: "CPU", layersOffloaded: 0, layersTotal: 35},
			wantGot: true,
		},
		{
			name:    "unmapped backend tag is title-cased",
			log:     "llm_load_tensors: offloaded 5/10 layers to GPU\nllama_model_load_internal: [somebackend] offloading 5 layers to GPU\n",
			want:    llamaOffloadResult{device: "Somebackend", layersOffloaded: 5, layersTotal: 10},
			wantGot: true,
		},
		{
			name:    "last summary wins in router mode",
			log:     "llm_load_tensors: offloaded 8/10 layers to GPU\nllm_load_tensors: offloaded 20/20 layers to GPU\nllama_model_load_internal: [metal] offloading 20 layers to GPU\n",
			want:    llamaOffloadResult{device: "Metal", layersOffloaded: 20, layersTotal: 20},
			wantGot: true,
		},
		{
			name:    "no offload summary is not found",
			log:     "server listening on :8080\nllm_load_tensors: ggml ctx size = 0.14 MiB\n",
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
				t.Fatalf("found = %v, want %v", found, tc.wantGot)
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

// TestReconcileAccelerationStatusStampsReadyPod verifies the full path: a
// ready llama-server pod whose log reports an offload result has it stamped on
// status.acceleration, so the offload is visible in the API.
func TestReconcileAccelerationStatusStampsReadyPod(t *testing.T) {
	pod := readyLlamaPod("svc-1", "default")
	r := newOffloadReconciler(t, &stubLogReader{
		text: "llm_load_tensors: offloaded 63/63 layers to GPU\nllama_model_load_internal: [vulkan] offloading 63 layers to GPU\n",
	}, pod)

	isvc := offloadIsvc("svc-1")
	r.reconcileAccelerationStatus(context.Background(), isvc)

	acc := isvc.Status.Acceleration
	if acc == nil {
		t.Fatal("status.acceleration is nil; want offload result from the ready pod")
	}
	if acc.Device != "Vulkan" {
		t.Errorf("device = %q, want Vulkan", acc.Device)
	}
	if acc.LayersOffloaded == nil || *acc.LayersOffloaded != 63 {
		t.Errorf("layersOffloaded = %v, want 63", acc.LayersOffloaded)
	}
	if acc.LayersTotal == nil || *acc.LayersTotal != 63 {
		t.Errorf("layersTotal = %v, want 63", acc.LayersTotal)
	}
}

// TestReconcileAccelerationStatusSilentCPUFallback is the regression test for
// #1385: a service that requested a GPU and reached Ready while offloading zero
// layers must surface CPU (0/total) on status.acceleration rather than staying
// indistinguishable from a healthy GPU service.
func TestReconcileAccelerationStatusSilentCPUFallback(t *testing.T) {
	pod := readyLlamaPod("svc-cpu", "default")
	r := newOffloadReconciler(t, &stubLogReader{
		text: "llm_load_tensors: offloaded 0/63 layers to GPU\nllama_model_load_internal: [vulkan] offloading 0 layers to GPU\n",
	}, pod)

	isvc := offloadIsvc("svc-cpu") // GPU requested
	r.reconcileAccelerationStatus(context.Background(), isvc)

	acc := isvc.Status.Acceleration
	if acc == nil {
		t.Fatal("status.acceleration is nil on a GPU service that fell back to CPU")
	}
	if acc.Device != "CPU" {
		t.Errorf("device = %q, want CPU", acc.Device)
	}
	if acc.LayersOffloaded == nil || *acc.LayersOffloaded != 0 {
		t.Errorf("layersOffloaded = %v, want 0", acc.LayersOffloaded)
	}
	if acc.LayersTotal == nil || *acc.LayersTotal != 63 {
		t.Errorf("layersTotal = %v, want 63", acc.LayersTotal)
	}
}

// TestReconcileAccelerationStatusNonLlamaCppLeavesUnset verifies that a runtime
// that does not report offload leaves status.acceleration unset rather than
// guessing.
func TestReconcileAccelerationStatusNonLlamaCppLeavesUnset(t *testing.T) {
	pod := readyLlamaPod("svc-vllm", "default")
	r := newOffloadReconciler(t, &stubLogReader{text: "offloaded 63/63 layers to GPU"}, pod)

	isvc := offloadIsvc("svc-vllm")
	isvc.Spec.Runtime = "vllm"
	r.reconcileAccelerationStatus(context.Background(), isvc)

	if isvc.Status.Acceleration != nil {
		t.Errorf("status.acceleration = %+v, want nil for a non-llama.cpp runtime", isvc.Status.Acceleration)
	}
}

// TestReconcileAccelerationStatusNoReadyPodLeavesUnset verifies that with no
// ready pod the field stays unset (unknown), not stale.
func TestReconcileAccelerationStatusNoReadyPodLeavesUnset(t *testing.T) {
	pod := notReadyLlamaPod("svc-notready", "default")
	r := newOffloadReconciler(t, &stubLogReader{text: "offloaded 63/63 layers to GPU"}, pod)

	isvc := offloadIsvc("svc-notready")
	r.reconcileAccelerationStatus(context.Background(), isvc)

	if isvc.Status.Acceleration != nil {
		t.Errorf("status.acceleration = %+v, want nil when no pod is ready", isvc.Status.Acceleration)
	}
}

// TestReconcileAccelerationStatusReadErrorLeavesUnset verifies that a failed log
// read is advisory: it leaves the field unset and does not fail the reconcile.
func TestReconcileAccelerationStatusReadErrorLeavesUnset(t *testing.T) {
	pod := readyLlamaPod("svc-err", "default")
	r := newOffloadReconciler(t, &stubLogReader{err: errors.New("boom")}, pod)

	isvc := offloadIsvc("svc-err")
	isvc.Status.Acceleration = &inferencev1alpha1.AccelerationStatus{Device: "Vulkan"}
	r.reconcileAccelerationStatus(context.Background(), isvc)

	// A read failure must clear to unknown, not keep a stale result, and must
	// not panic or return an error (the reconcile never fails on this).
	if isvc.Status.Acceleration != nil {
		t.Errorf("status.acceleration = %+v, want nil after a read failure", isvc.Status.Acceleration)
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
