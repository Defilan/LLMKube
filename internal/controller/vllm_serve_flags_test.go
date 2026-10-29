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
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// vllmServeFlagsOracle is the pinned image's own flag list. Regenerate it
// from the new image whenever VLLMBackend.DefaultImage moves (see its header).
const vllmServeFlagsOracle = "testdata/vllm-serve-flags-v0.32.0.txt"

func loadVLLMServeFlags(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(vllmServeFlagsOracle)
	if err != nil {
		t.Fatalf("open oracle: %v", err)
	}
	defer func() { _ = f.Close() }()
	flags := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		flags[line] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read oracle: %v", err)
	}
	// Guard the fixture: an empty or truncated capture would make every flag
	// look unknown, which reads as a code bug rather than a fixture bug.
	for _, must := range []string{"--port", "--tensor-parallel-size", "--speculative-config", "--max-model-len"} {
		if !flags[must] {
			t.Fatalf("%s is missing %q; it is not a valid capture of `vllm serve --help=all`", vllmServeFlagsOracle, must)
		}
	}
	if len(flags) < 200 {
		t.Fatalf("%s has only %d flags; expected the full `vllm serve --help=all` list", vllmServeFlagsOracle, len(flags))
	}
	return flags
}

// TestVLLMBuilderFlagLiteralsArePinnedVLLMFlags pins #2212: every flag
// literal in the vLLM argument builders is a flag the pinned image
// registers. vLLM exits at startup on an unknown flag, so a flag that a
// release removed, or that never existed (#2190 shipped --speculative-model),
// fails here instead of on a cluster. Reading the source covers branches no
// rendered configuration below reaches.
func TestVLLMBuilderFlagLiteralsArePinnedVLLMFlags(t *testing.T) {
	flags := loadVLLMServeFlags(t)
	files, err := filepath.Glob("runtime_vllm*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	literal := regexp.MustCompile(`"(--[a-z0-9][a-z0-9-]*)`)
	seen := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			seen++
			if !flags[m[1]] {
				t.Errorf("%s emits %s, which %s does not register; vLLM would exit at startup", path, m[1], vllmServeFlagsOracle)
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no flag literals in runtime_vllm*.go; the scan is not reading the builders")
	}
}

// TestVLLMRenderedFlagsArePinnedVLLMFlags renders a service that sets every
// VLLMConfig field that becomes a flag, plus a multi-node worker, and checks
// each rendered flag against the pinned image's list.
func TestVLLMRenderedFlagsArePinnedVLLMFlags(t *testing.T) {
	flags := loadVLLMServeFlags(t)
	kvDtype := "fp8_e4m3"
	gpuMem := 0.8
	isvc := &inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:       "vllm",
			ParallelSlots: ptrInt32(8),
			VLLMConfig: &inferencev1alpha1.VLLMConfig{
				TensorParallelSize:   ptrInt32(2),
				PipelineParallelSize: ptrInt32(2),
				MaxModelLen:          ptrInt32(8192),
				Quantization:         "fp8",
				Dtype:                "bfloat16",
				KVCacheDtype:         &kvDtype,
				EnablePrefixCaching:  ptrBool(true),
				EnableChunkedPrefill: ptrBool(true),
				MaxNumBatchedTokens:  ptrInt32(4096),
				AttentionBackend:     "FLASHINFER",
				Speculative:          &inferencev1alpha1.SpeculativeConfig{Enabled: ptrBool(true), Method: "mtp", NumSpeculativeTokens: ptrInt32(1)},
				EnableExpertParallel: ptrBool(true),
				ToolCallParser:       "hermes",
				ReasoningParser:      "qwen3",
				CPUOffloadGB:         ptrInt32(4),
				GPUMemoryUtilization: &gpuMem,
			},
		},
	}
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "org/model",
			Format:   "safetensors",
			Hardware: &inferencev1alpha1.HardwareSpec{GPU: &inferencev1alpha1.GPUSpec{Enabled: true, Count: 2, Vendor: "nvidia"}},
		},
	}
	b := &VLLMBackend{}
	args := b.BuildArgs(isvc, model, "/models/x", "", 8000)
	args = append(args, b.BuildMultiNodeArgs(multiNodeISVCForTest(), 1)...)

	rendered := map[string]bool{}
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		rendered[name] = true
	}
	var unknown []string
	for name := range rendered {
		if !flags[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("rendered flags %v are not in %s; vLLM would exit at startup (args %v)", unknown, vllmServeFlagsOracle, args)
	}
	// The configuration must actually reach the typed flags, or the check
	// above passes vacuously.
	for _, want := range []string{"--tensor-parallel-size", "--kv-cache-dtype", "--speculative-config", "--max-num-seqs", "--cpu-offload-gb", "--headless"} {
		if !rendered[want] {
			t.Errorf("the rendered configuration did not emit %s; widen it so every typed flag is checked (args %v)", want, args)
		}
	}
}
