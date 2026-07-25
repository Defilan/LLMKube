package controller

import (
	"testing"

	"github.com/defilantech/llmkube/pkg/apiutil"
)

// TestGPUResourceNameLiteralsMatchAPIUtil is the cross-package drift guard
// (#1255): the internal controller package must alias the GPU resource-name
// literals from pkg/apiutil rather than redeclaring the strings, so a pod's
// GPU resource request (apiutil path) can never disagree with its toleration
// key (internal path). If someone re-declares a literal here, this test fails
// the build.
func TestGPUResourceNameLiteralsMatchAPIUtil(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"nvidia", string(nvidiaGPUResourceName), string(apiutil.NvidiaGPUResourceName)},
		{"amd", string(amdGPUResourceName), string(apiutil.AmdGPUResourceName)},
		{"intel i915", string(intelGPUResourceNameI915), string(apiutil.IntelGPUResourceNameI915)},
		{"vulkan dri-render", string(vulkanDRIResourceName), string(apiutil.VulkanDRIResourceName)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s resource name = %q, want %q (must alias pkg/apiutil)",
					tc.name, tc.got, tc.want)
			}
		})
	}
}
