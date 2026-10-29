/*
Copyright 2026.

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
	"regexp"
	"strings"
	"testing"
)

// TestLlamaCppDefaultImagesShareOneBuild pins #2214: the CPU, NVIDIA and
// router-mode defaults come from one immutable upstream build. A floating tag
// lets two rollouts of the same service run different servers, and a split
// pin makes a flag work or fail depending on whether the Model declares a GPU.
func TestLlamaCppDefaultImagesShareOneBuild(t *testing.T) {
	if !regexp.MustCompile(`^b[0-9]+$`).MatchString(llamaCppBuild) {
		t.Fatalf("llamaCppBuild = %q, want an upstream build tag like b11854", llamaCppBuild)
	}
	for name, image := range map[string]string{
		"LlamaCppBackend.DefaultImage":       (&LlamaCppBackend{}).DefaultImage(),
		"llamaCppCUDAImage":                  llamaCppCUDAImage,
		"LlamaCppRouterBackend.DefaultImage": (&LlamaCppRouterBackend{}).DefaultImage(),
	} {
		if !strings.HasPrefix(image, "ghcr.io/ggml-org/llama.cpp:server") {
			t.Errorf("%s = %q, want an upstream ghcr.io/ggml-org/llama.cpp server image", name, image)
		}
		if !strings.HasSuffix(image, "-"+llamaCppBuild) {
			t.Errorf("%s = %q, want the per-build tag for %s", name, image, llamaCppBuild)
		}
	}
}
