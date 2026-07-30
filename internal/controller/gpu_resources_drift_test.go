package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/apiutil"
)

// TestGPUResourceNameLiteralsMatchAPIUtil is a drift guard for the four GPU
// extended-resource literals that are duplicated between this package
// (gpu_resources.go) and pkg/apiutil (gpu.go). PR #1254 exported the mapping
// via apiutil and made gpuResourceNameForSpec delegate to it, but the
// internal copies are still read directly by gpuTolerationKeyForSpec, the
// federation edge controller, and the model controller's readiness check.
//
// A future edit to one copy that silently diverges would let a pod's GPU
// resource request (apiutil path) disagree with its toleration key
// (internal path), leaving the pod unschedulable. This test pins the two
// sets of literals together for all four vendors so the build fails the
// moment they drift.
func TestGPUResourceNameLiteralsMatchAPIUtil(t *testing.T) {
	cases := []struct {
		name  string
		model *inferencev1alpha1.Model
		// internal is the literal resolved by this package's own variables.
		internal corev1.ResourceName
	}{
		{
			name:     "nvidia.com/gpu",
			model:    &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Hardware: &inferencev1alpha1.HardwareSpec{GPU: &inferencev1alpha1.GPUSpec{Vendor: "nvidia"}}}},
			internal: nvidiaGPUResourceName,
		},
		{
			name:     "amd.com/gpu",
			model:    &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Hardware: &inferencev1alpha1.HardwareSpec{GPU: &inferencev1alpha1.GPUSpec{Vendor: "amd"}}}},
			internal: amdGPUResourceName,
		},
		{
			name:     "gpu.intel.com/i915",
			model:    &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Hardware: &inferencev1alpha1.HardwareSpec{GPU: &inferencev1alpha1.GPUSpec{Vendor: "intel"}}}},
			internal: intelGPUResourceNameI915,
		},
		{
			name:     "devic.es/dri-render",
			model:    &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Hardware: &inferencev1alpha1.HardwareSpec{GPU: &inferencev1alpha1.GPUSpec{Vendor: "amd", Runtime: "vulkan"}}}},
			internal: vulkanDRIResourceName,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apiutil.GPUResourceName(tc.model)
			if got != tc.internal {
				t.Fatalf(
					"apiutil.GPUResourceName() = %q, internal literal = %q; "+
						"the two packages' GPU resource-name literals have drifted",
					got, tc.internal,
				)
			}
		})
	}
}
