package apiutil

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func modelWithGPU(gpu *inferencev1alpha1.GPUSpec) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{GPU: gpu},
		},
	}
}

func TestGPUResourceName(t *testing.T) {
	cases := []struct {
		name  string
		model *inferencev1alpha1.Model
		want  corev1.ResourceName
	}{
		{"nil model defaults to nvidia", nil, corev1.ResourceName("nvidia.com/gpu")},
		{"explicit override wins", modelWithGPU(&inferencev1alpha1.GPUSpec{ResourceName: "squat.ai/dri-render", Vendor: "amd"}), corev1.ResourceName("squat.ai/dri-render")},
		{"amd vulkan uses dri-render", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: "vulkan"}), corev1.ResourceName("devic.es/dri-render")},
		{"amd rocm uses dri-render", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: "rocm"}), corev1.ResourceName("devic.es/dri-render")},
		{"amd default uses amd.com/gpu", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "amd"}), corev1.ResourceName("amd.com/gpu")},
		{"intel uses i915", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "intel"}), corev1.ResourceName("gpu.intel.com/i915")},
		{"unknown vendor defaults to nvidia", modelWithGPU(&inferencev1alpha1.GPUSpec{Vendor: "other"}), corev1.ResourceName("nvidia.com/gpu")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GPUResourceName(tc.model); got != tc.want {
				t.Fatalf("GPUResourceName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGPUResourceNameLiterals is a drift guard for this package only:
// it pins the four exported GPU resource-name values against their
// string literals so a typo or copy/paste here is caught. It does not
// and cannot cover internal/controller, because that package imports
// pkg/apiutil (an import cycle would block the reverse). The real
// cross-package protection is the compile-time aliasing in
// internal/controller/gpu_resources.go, which assigns these exported
// values to its own package-level variables; any divergent literal
// there would have to be a hand-written value that no longer matches
// the alias, and is guarded by the build, not by this test.
func TestGPUResourceNameLiterals(t *testing.T) {
	cases := []struct {
		name string
		got  corev1.ResourceName
		want string
	}{
		{"nvidia", NvidiaGPUResourceName, "nvidia.com/gpu"},
		{"amd", AmdGPUResourceName, "amd.com/gpu"},
		{"intel i915", IntelGPUResourceNameI915, "gpu.intel.com/i915"},
		{"vulkan dri-render", VulkanDRIResourceName, "devic.es/dri-render"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.want {
				t.Fatalf("%s GPU resource name = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestGPUCount(t *testing.T) {
	isvcWith := func(gpu int32) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Resources: &inferencev1alpha1.InferenceResourceRequirements{GPU: gpu},
			},
		}
	}
	cases := []struct {
		name  string
		isvc  *inferencev1alpha1.InferenceService
		model *inferencev1alpha1.Model
		want  int32
	}{
		{"model count wins", isvcWith(1), modelWithGPU(&inferencev1alpha1.GPUSpec{Count: 2}), 2},
		{"isvc count when model has none", isvcWith(3), modelWithGPU(&inferencev1alpha1.GPUSpec{}), 3},
		{"zero when neither set", &inferencev1alpha1.InferenceService{}, modelWithGPU(nil), 0},
		{"nil model is safe", isvcWith(2), nil, 2},
		{"nil isvc is safe", nil, modelWithGPU(&inferencev1alpha1.GPUSpec{Count: 4}), 4},
		{"both nil is zero", nil, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GPUCount(tc.isvc, tc.model); got != tc.want {
				t.Fatalf("GPUCount() = %d, want %d", got, tc.want)
			}
		})
	}
}
