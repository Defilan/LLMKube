package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestResolveGPUResourceName(t *testing.T) {
	t.Run("defaults to nvidia when model hardware is missing", func(t *testing.T) {
		if got := resolveGPUResourceName(&inferencev1alpha1.Model{}); got != nvidiaGPUResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, nvidiaGPUResourceName)
		}
	})

	t.Run("uses nvidia for non-intel vendor", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{Vendor: "nvidia"},
				},
			},
		}

		if got := resolveGPUResourceName(model); got != nvidiaGPUResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, nvidiaGPUResourceName)
		}
	})

	t.Run("uses intel i915 resource by default", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{Vendor: "intel"},
				},
			},
		}

		if got := resolveGPUResourceName(model); got != intelGPUResourceNameI915 {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, intelGPUResourceNameI915)
		}
	})

	t.Run("uses intel i915 resource when accelerator is intel and gpu vendor is unset", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "intel",
				},
			},
		}

		if got := resolveGPUResourceName(model); got != intelGPUResourceNameI915 {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, intelGPUResourceNameI915)
		}
	})

	t.Run("uses configured intel resource override", func(t *testing.T) {
		t.Setenv(intelGPUResourceEnvVar, "gpu.intel.com/xe")

		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					GPU: &inferencev1alpha1.GPUSpec{Vendor: "intel"},
				},
			},
		}

		if got := resolveGPUResourceName(model); got != intelGPUResourceNameXE {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, intelGPUResourceNameXE)
		}
	})

	t.Run("uses vulkan resource when accelerator is vulkan", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "vulkan",
				},
			},
		}

		if got := resolveGPUResourceName(model); got != vulkanDRIResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, vulkanDRIResourceName)
		}
	})

	t.Run("uses vulkan resource when accelerator is vulkan with amd gpu vendor", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "vulkan",
					GPU:         &inferencev1alpha1.GPUSpec{Vendor: "amd"},
				},
			},
		}

		if got := resolveGPUResourceName(model); got != vulkanDRIResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, vulkanDRIResourceName)
		}
	})

	t.Run("uses the shared dri-render resource when accelerator is rocm", func(t *testing.T) {
		model := &inferencev1alpha1.Model{
			Spec: inferencev1alpha1.ModelSpec{
				Hardware: &inferencev1alpha1.HardwareSpec{
					Accelerator: "rocm",
				},
			},
		}

		if got := resolveGPUResourceName(model); got != vulkanDRIResourceName {
			t.Fatalf("resolveGPUResourceName() = %q, want %q", got, vulkanDRIResourceName)
		}
	})
}

func TestDetectInsufficientGPUResource(t *testing.T) {
	t.Run("detects nvidia insufficient resource", func(t *testing.T) {
		message := "0/1 nodes are available: 1 Insufficient nvidia.com/gpu."

		got, ok := detectInsufficientGPUResource(message)
		if !ok {
			t.Fatalf("detectInsufficientGPUResource() ok = false, want true")
		}
		if got != nvidiaGPUResourceName {
			t.Fatalf("detectInsufficientGPUResource() = %q, want %q", got, nvidiaGPUResourceName)
		}
	})

	t.Run("detects intel i915 insufficient resource", func(t *testing.T) {
		message := "0/1 nodes are available: 1 Insufficient gpu.intel.com/i915."

		got, ok := detectInsufficientGPUResource(message)
		if !ok {
			t.Fatalf("detectInsufficientGPUResource() ok = false, want true")
		}
		if got != intelGPUResourceNameI915 {
			t.Fatalf("detectInsufficientGPUResource() = %q, want %q", got, intelGPUResourceNameI915)
		}
	})

	t.Run("detects intel custom resource from env override", func(t *testing.T) {
		t.Setenv(intelGPUResourceEnvVar, "gpu.intel.com/custom")
		message := "0/1 nodes are available: 1 Insufficient gpu.intel.com/custom."

		got, ok := detectInsufficientGPUResource(message)
		if !ok {
			t.Fatalf("detectInsufficientGPUResource() ok = false, want true")
		}
		if got != "gpu.intel.com/custom" {
			t.Fatalf("detectInsufficientGPUResource() = %q, want %q", got, "gpu.intel.com/custom")
		}
	})

	t.Run("ignores non-GPU insufficient resources", func(t *testing.T) {
		message := "0/1 nodes are available: 1 Insufficient cpu."

		if _, ok := detectInsufficientGPUResource(message); ok {
			t.Fatalf("detectInsufficientGPUResource() ok = true, want false")
		}
	})

	t.Run("detects vulkan insufficient resource", func(t *testing.T) {
		message := "0/1 nodes are available: 1 Insufficient devic.es/dri-render."

		got, ok := detectInsufficientGPUResource(message)
		if !ok {
			t.Fatalf("detectInsufficientGPUResource() ok = false, want true")
		}
		if got != vulkanDRIResourceName {
			t.Fatalf("detectInsufficientGPUResource() = %q, want %q", got, vulkanDRIResourceName)
		}
	})
}

// #2059: the GPU count can come from the Model (hardware.gpu.count) rather than
// spec.resources.gpu. WaitingFor used to read only the latter and told the user
// "nvidia.com/gpu: 0" while the pod was waiting for 1.
func TestGetPodSchedulingInfoReportsThePodsGPULimit(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("inferencev1alpha1.AddToScheme: %v", err)
	}

	// No spec.resources: the count came from the Model.
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "embed", Namespace: "default"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "embed-abc",
			Namespace: "default",
			Labels: map[string]string{
				"app":                           "embed",
				"inference.llmkube.dev/service": "embed",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "llama-server",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{nvidiaGPUResourceName: resource.MustParse("1")},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: "0/4 nodes are available: 1 node(s) had untolerated taint(s), 3 Insufficient nvidia.com/gpu.",
			}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, pod).Build()
	r := &InferenceServiceReconciler{Client: c, Scheme: scheme}

	info, err := r.getPodSchedulingInfo(context.Background(), isvc)
	if err != nil {
		t.Fatalf("getPodSchedulingInfo() error = %v", err)
	}
	if info == nil || info.Status != "InsufficientGPU" {
		t.Fatalf("getPodSchedulingInfo() = %+v, want Status InsufficientGPU", info)
	}
	if info.WaitingFor != "nvidia.com/gpu: 1" {
		t.Fatalf("WaitingFor = %q, want %q", info.WaitingFor, "nvidia.com/gpu: 1")
	}
}
