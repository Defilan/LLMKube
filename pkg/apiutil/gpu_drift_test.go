package apiutil

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestGPUResourceNameLiteralsAreExported is the cross-package drift guard
// (#1255): the four GPU resource-name literals that the operator requests and
// tolerates must be the exported constants in pkg/apiutil, and the internal
// controller package aliases them rather than redeclaring the strings. This
// test pins the literal values so a future edit to one copy fails the build
// instead of silently diverging.
func TestGPUResourceNameLiteralsAreExported(t *testing.T) {
	cases := []struct {
		name     string
		got      corev1.ResourceName
		expected corev1.ResourceName
	}{
		{"nvidia", NvidiaGPUResourceName, corev1.ResourceName("nvidia.com/gpu")},
		{"amd", AmdGPUResourceName, corev1.ResourceName("amd.com/gpu")},
		{"intel i915", IntelGPUResourceNameI915, corev1.ResourceName("gpu.intel.com/i915")},
		{"vulkan dri-render", VulkanDRIResourceName, corev1.ResourceName("devic.es/dri-render")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.expected {
				t.Fatalf("%s resource name = %q, want %q", tc.name, tc.got, tc.expected)
			}
		})
	}
}
