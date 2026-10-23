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
	"reflect"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestVLLMBuildArgs is the single table-driven test that covers every new
// agentic-coding flag and the "not emitted when unset/false" counterpart.
// Each row asserts a set of must-contain and must-not-contain flags on the
// generated arg list.
func TestVLLMBuildArgs(t *testing.T) {
	backend := &VLLMBackend{}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	modelWithGpu := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{
				GPU: &inferencev1alpha1.GPUSpec{
					Enabled: true,
					Count:   1,
				},
			},
		},
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-gpu", Namespace: "default"},
	}
	// A DRA-only model: the claim provides the GPU and count is unset (#2059).
	modelWithDRA := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Hardware: &inferencev1alpha1.HardwareSpec{
				GPU: &inferencev1alpha1.GPUSpec{
					Enabled:        true,
					ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptrString("gpu-claim")}},
				},
			},
		},
		ObjectMeta: metav1.ObjectMeta{Name: "test-model-dra", Namespace: "default"},
	}
	const modelPath = "/models/test"
	const port = int32(8000)

	cases := []struct {
		// contains is a slice of flag/value pairs ("" value means "flag must be
		// present as a bare toggle").
		contains []FlagCheck
		// notContains is just a list of flags that must NOT appear anywhere in args.
		notContains []string
		// exactlyOnce is a list of flags that must appear exactly once, so an
		// extraArgs duplicate is caught.
		exactlyOnce []string
		model       *inferencev1alpha1.Model
		name        string
		spec        *inferencev1alpha1.InferenceServiceSpec
	}{
		{
			model: model,
			name:  "nil config emits only base flags (model as positional)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: nil,
			},
			// vLLM v0.20+ deprecated --model in favor of a positional argument.
			// The bare model path appears as args[0]; --model itself must NOT appear.
			contains: []FlagCheck{{"--host", "::"}, {"--port", "8000"}},
			notContains: []string{
				"--attention-backend",
				"--cpu-offload-gb",
				"--enable-chunked-prefill",
				"--enable-expert-parallel",
				"--enable-prefix-caching",
				"--gpu-memory-utilization",
				"--kv-cache-dtype",
				"--max-num-batched-tokens",
				"--model",
				"--speculative-model",
			},
		},
		{
			model: model,
			name:  "empty config emits only base flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{
				"--cpu-offload-gb",
				"--enable-expert-parallel",
				"--enable-chunked-prefill",
				"--enable-prefix-caching",
				"--gpu-memory-utilization",
				"--kv-cache-dtype",
				"--max-num-batched-tokens",
				"--speculative-model",
			},
		},
		{
			model: modelWithGpu,
			name:  "cpuOffloadGB set emits flag if gpu is enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model-gpu",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{CPUOffloadGB: ptrInt32(4)},
			},
			contains: []FlagCheck{{"--cpu-offload-gb", "4"}},
		},
		{
			model: modelWithDRA,
			name:  "cpuOffloadGB set emits flag for a DRA-only model",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model-dra",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{CPUOffloadGB: ptrInt32(4)},
			},
			contains: []FlagCheck{{"--cpu-offload-gb", "4"}},
		},
		{
			model: model,
			name:  "cpuOffloadGB set does not emit flag if gpu is not enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{CPUOffloadGB: ptrInt32(4)},
			},
			notContains: []string{"--cpu-offload-gb"},
		},
		{
			model: model,
			name:  "cpuOffloadGB nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--cpu-offload-gb"},
		},
		{
			model: modelWithGpu,
			name:  "gpuMemoryUtilization set emits flag if gpu is enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model-gpu",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{GPUMemoryUtilization: ptrFloat64(0.80)},
			},
			contains: []FlagCheck{{"--gpu-memory-utilization", "0.8"}},
		},
		{
			model: modelWithDRA,
			name:  "gpuMemoryUtilization set emits flag for a DRA-only model",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model-dra",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{GPUMemoryUtilization: ptrFloat64(0.80)},
			},
			contains: []FlagCheck{{"--gpu-memory-utilization", "0.8"}},
		},
		{
			model: model,
			name:  "gpuMemoryUtilization does not emit flag if gpu is not enabled",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{GPUMemoryUtilization: ptrFloat64(0.80)},
			},
			notContains: []string{"--gpu-memory-utilization"},
		},
		{
			model: model,
			name:  "gpuMemoryUtilization nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--gpu-memory-utilization"},
		},
		{
			model: model,
			name:  "kvCacheDtype=auto does not emit flag (vLLM default)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("auto")},
			},
			notContains: []string{"--kv-cache-dtype"},
		},
		{
			model: model,
			name:  "kvCacheDtype=fp8_e5m2 emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("fp8_e5m2")},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "fp8_e5m2"}},
		},
		{
			model: model,
			name:  "kvCacheDtype=fp8_e4m3 emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{KVCacheDtype: ptrString("fp8_e4m3")},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "fp8_e4m3"}},
		},
		{
			model: model,
			name:  "kvCacheCustomDtype=turbo2 emits flag (vLLM v0.20+ TurboQuant 2-bit)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{KVCacheCustomDtype: "turbo2"},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "turbo2"}},
		},
		{
			model: model,
			name:  "kvCacheCustomDtype wins over standard kvCacheDtype when both set",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					KVCacheDtype:       ptrString("fp8_e4m3"),
					KVCacheCustomDtype: "turbo2",
				},
			},
			contains:    []FlagCheck{{"--kv-cache-dtype", "turbo2"}},
			notContains: []string{"fp8_e4m3"},
		},
		{
			model: model,
			name:  "kvCacheCustomDtype empty falls back to standard kvCacheDtype",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					KVCacheDtype:       ptrString("fp8_e5m2"),
					KVCacheCustomDtype: "",
				},
			},
			contains: []FlagCheck{{"--kv-cache-dtype", "fp8_e5m2"}},
		},
		{
			model: model,
			name:  "enablePrefixCaching=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnablePrefixCaching: ptrBool(true)},
			},
			contains: []FlagCheck{{"--enable-prefix-caching", ""}},
		},
		{
			model: model,
			name:  "enablePrefixCaching=false does not emit flag (lets vLLM default)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnablePrefixCaching: ptrBool(false)},
			},
			notContains: []string{"--enable-prefix-caching"},
		},
		{
			model: model,
			name:  "enableChunkedPrefill=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnableChunkedPrefill: ptrBool(true)},
			},
			contains: []FlagCheck{{"--enable-chunked-prefill", ""}},
		},
		{
			model: model,
			name:  "enableChunkedPrefill=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnableChunkedPrefill: ptrBool(false)},
			},
			notContains: []string{"--enable-chunked-prefill"},
		},
		{
			model: model,
			name:  "maxNumBatchedTokens set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{MaxNumBatchedTokens: ptrInt32(8192)},
			},
			contains: []FlagCheck{{"--max-num-batched-tokens", "8192"}},
		},
		{
			model: model,
			name:  "maxNumBatchedTokens nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--max-num-batched-tokens"},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (without extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "vllm",
				ModelRef:      "test-model",
				ParallelSlots: ptrInt32(1),
				VLLMConfig:    &inferencev1alpha1.VLLMConfig{},
			},
			contains: []FlagCheck{{"--max-num-seqs", "1"}},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (with extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "vllm",
				ModelRef:      "test-model",
				ExtraArgs:     []string{"--max-num-seqs", "8"},
				ParallelSlots: ptrInt32(4),
			},
			// NOTE: Since extraArgs are always last in position but still have priority
			// and containsArgs helper function always validate first occurrence, having
			// --max-num-seqs 8 case true mean that no duplicate due to parallelSlots was
			// found along the way.
			contains: []FlagCheck{{"--max-num-seqs", "8"}},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (with extraArgs inline precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "vllm",
				ModelRef:      "test-model",
				ExtraArgs:     []string{"--max-num-seqs=8"},
				ParallelSlots: ptrInt32(4),
			},
			// NOTE: Since extraArgs are always last in position but still have priority
			// and containsArgs helper function always validate first occurrence, having
			// --max-num-seqs 8 case true mean that no duplicate due to parallelSlots was
			// found along the way.
			contains: []FlagCheck{{"--max-num-seqs=8", ""}},
		},
		{
			model: model,
			name:  "parallelSlots nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--max-num-seqs"},
		},
		{
			model: model,
			name:  "attentionBackend=FLASHINFER emits flag (uppercase)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{AttentionBackend: "FLASHINFER"},
			},
			contains: []FlagCheck{{"--attention-backend", "FLASHINFER"}},
		},
		{
			model: model,
			name:  "attentionBackend=flashinfer emits flag (lowercase compat)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{AttentionBackend: "flashinfer"},
			},
			contains: []FlagCheck{{"--attention-backend", "flashinfer"}},
		},
		{
			model: model,
			name:  "speculative enabled+model emits both flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled:              ptrBool(true),
						Model:                "Qwen/Qwen3.6-4B",
						NumSpeculativeTokens: ptrInt32(4),
					},
				},
			},
			contains: []FlagCheck{
				{"--speculative-model", "Qwen/Qwen3.6-4B"},
				{"--num-speculative-tokens", "4"},
			},
		},
		{
			model: model,
			name:  "speculative enabled without model skips both flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled:              ptrBool(true),
						NumSpeculativeTokens: ptrInt32(4),
					},
				},
			},
			notContains: []string{"--speculative-model", "--num-speculative-tokens"},
		},
		{
			model: model,
			name:  "speculative disabled does not emit flags even with model set",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "vllm",
				ModelRef: "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled: ptrBool(false),
						Model:   "Qwen/Qwen3.6-4B",
					},
				},
			},
			notContains: []string{"--speculative-model", "--num-speculative-tokens"},
		},
		{
			model: model,
			name:  "enableExpertParallel=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnableExpertParallel: ptrBool(true)},
			},
			contains: []FlagCheck{{"--enable-expert-parallel", ""}},
		},
		{
			model: model,
			name:  "enableExpertParallel=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{EnableExpertParallel: ptrBool(false)},
			},
			notContains: []string{"--enable-expert-parallel"},
		},
		{
			model: model,
			name:  "full agentic config emits all flags together",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "vllm",
				ModelRef:      "test-model",
				ParallelSlots: ptrInt32(1),
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					AttentionBackend:     "FLASHINFER",
					Dtype:                "bfloat16",
					EnablePrefixCaching:  ptrBool(true),
					EnableChunkedPrefill: ptrBool(true),
					KVCacheDtype:         ptrString("fp8_e5m2"),
					MaxModelLen:          ptrInt32(131072),
					MaxNumBatchedTokens:  ptrInt32(8192),
					Quantization:         "fp8",
					TensorParallelSize:   ptrInt32(2),
					ToolCallParser:       "glm47",
					ReasoningParser:      "glm45",
				},
			},
			contains: []FlagCheck{
				{"--attention-backend", "FLASHINFER"},
				{"--dtype", "bfloat16"},
				{"--enable-prefix-caching", ""},
				{"--enable-chunked-prefill", ""},
				{"--kv-cache-dtype", "fp8_e5m2"},
				{"--max-model-len", "131072"},
				{"--max-num-batched-tokens", "8192"},
				{"--max-num-seqs", "1"},
				{"--quantization", "fp8"},
				{"--tensor-parallel-size", "2"},
				{"--tool-call-parser", "glm47"},
				{"--enable-auto-tool-choice", ""},
				{"--reasoning-parser", "glm45"},
			},
		},
		{
			model: model,
			name:  "toolCallParser emits the auto-tool-choice pairing",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{ToolCallParser: "glm47"},
			},
			contains: []FlagCheck{
				{"--tool-call-parser", "glm47"},
				{"--enable-auto-tool-choice", ""},
			},
			notContains: []string{"--reasoning-parser"},
		},
		{
			model: model,
			name:  "toolCallParser unset emits neither flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
			notContains: []string{"--tool-call-parser", "--enable-auto-tool-choice", "--reasoning-parser"},
		},
		{
			model: model,
			name:  "nil vllmConfig emits no parser flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: nil,
			},
			notContains: []string{"--tool-call-parser", "--enable-auto-tool-choice", "--reasoning-parser"},
		},
		{
			model: model,
			name:  "toolCallParser with --tool-call-parser in extraArgs still pairs",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				ExtraArgs:  []string{"--tool-call-parser", "hermes"},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{ToolCallParser: "glm47"},
			},
			// The user's parser wins, and auto tool choice must still be on or
			// their parser is inert.
			contains:    []FlagCheck{{"--tool-call-parser", "hermes"}, {"--enable-auto-tool-choice", ""}},
			notContains: []string{"glm47"},
			exactlyOnce: []string{"--tool-call-parser", "--enable-auto-tool-choice"},
		},
		{
			model: model,
			name:  "toolCallParser with an inline --tool-call-parser in extraArgs emits once",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				ExtraArgs:  []string{"--tool-call-parser=hermes"},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{ToolCallParser: "glm47"},
			},
			contains:    []FlagCheck{{"--enable-auto-tool-choice", ""}},
			exactlyOnce: []string{"--tool-call-parser"},
		},
		{
			model: model,
			name:  "toolCallParser with --enable-auto-tool-choice in extraArgs emits it once",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				ExtraArgs:  []string{"--enable-auto-tool-choice"},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{ToolCallParser: "glm47"},
			},
			contains:    []FlagCheck{{"--tool-call-parser", "glm47"}},
			exactlyOnce: []string{"--enable-auto-tool-choice"},
		},
		{
			model: model,
			name:  "toolCallParser with an inline --enable-auto-tool-choice in extraArgs emits it once",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				ExtraArgs:  []string{"--enable-auto-tool-choice=true"},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{ToolCallParser: "glm47"},
			},
			contains:    []FlagCheck{{"--tool-call-parser", "glm47"}},
			exactlyOnce: []string{"--enable-auto-tool-choice"},
		},
		{
			model: model,
			name:  "toolCallParser fully covered by extraArgs contributes neither flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:   "vllm",
				ModelRef:  "test-model",
				ExtraArgs: []string{"--tool-call-parser", "hermes", "--enable-auto-tool-choice"},
				// Both flags are already the user's, so the typed field adds
				// nothing and the argv carries exactly the extraArgs pair.
				VLLMConfig: &inferencev1alpha1.VLLMConfig{ToolCallParser: "glm47"},
			},
			contains:    []FlagCheck{{"--tool-call-parser", "hermes"}, {"--enable-auto-tool-choice", ""}},
			notContains: []string{"glm47"},
			exactlyOnce: []string{"--tool-call-parser", "--enable-auto-tool-choice"},
		},
		{
			model: model,
			name:  "reasoningParser emits alone",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{ReasoningParser: "glm45"},
			},
			contains:    []FlagCheck{{"--reasoning-parser", "glm45"}},
			notContains: []string{"--tool-call-parser", "--enable-auto-tool-choice"},
		},
		{
			model: model,
			name:  "reasoningParser with --reasoning-parser in extraArgs emits once",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "vllm",
				ModelRef:   "test-model",
				ExtraArgs:  []string{"--reasoning-parser", "deepseek_v4"},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{ReasoningParser: "glm45"},
			},
			contains:    []FlagCheck{{"--reasoning-parser", "deepseek_v4"}},
			notContains: []string{"glm45"},
			exactlyOnce: []string{"--reasoning-parser"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "isvc-" + strings.ReplaceAll(tc.name, " ", "-"), Namespace: "default"},
				Spec:       *tc.spec,
			}
			args := backend.BuildArgs(isvc, tc.model, modelPath, "", port)
			for _, fc := range tc.contains {
				if !containsArg(args, fc.flag, fc.value) {
					t.Errorf("expected %q %q in args, got: %v", fc.flag, fc.value, args)
				}
			}
			for _, f := range tc.notContains {
				if containsArg(args, f, "") {
					t.Errorf("expected %q NOT in args, got: %v", f, args)
				}
			}
			for _, f := range tc.exactlyOnce {
				if n := countFlagAnyForm(args, f); n != 1 {
					t.Errorf("expected %q exactly once, got %d in args: %v", f, n, args)
				}
			}
		})
	}
}

// TestVLLMBuildArgsDeterministic verifies BuildArgs emits flags in the same
// order across calls — important so Deployment .spec diffs stay quiet and
// snapshot tests do not flake.
func TestVLLMBuildArgsDeterministic(t *testing.T) {
	backend := &VLLMBackend{}
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: "vllm",
			VLLMConfig: &inferencev1alpha1.VLLMConfig{
				TensorParallelSize:   ptrInt32(2),
				KVCacheDtype:         ptrString("fp8_e5m2"),
				EnablePrefixCaching:  ptrBool(true),
				EnableChunkedPrefill: ptrBool(true),
				MaxNumBatchedTokens:  ptrInt32(8192),
				AttentionBackend:     "FLASHINFER",
				ToolCallParser:       "glm47",
				ReasoningParser:      "glm45",
			},
		},
	}

	first := backend.BuildArgs(isvc, model, "/models/x", "", 8000)
	if !containsArg(first, "--tool-call-parser", "glm47") {
		t.Fatalf("fixture does not emit --tool-call-parser glm47: %v", first)
	}
	// The two parser flags follow struct-declaration order (ToolCallParser
	// before ReasoningParser), so the argv reads in the order vllmConfig is
	// documented to render.
	toolIdx := indexOfFlag(first, "--tool-call-parser")
	reasonIdx := indexOfFlag(first, "--reasoning-parser")
	if toolIdx < 0 || reasonIdx < 0 || toolIdx > reasonIdx {
		t.Fatalf("want --tool-call-parser before --reasoning-parser, got: %v", first)
	}
	for i := 0; i < 10; i++ {
		got := backend.BuildArgs(isvc, model, "/models/x", "", 8000)
		if len(got) != len(first) {
			t.Fatalf("iteration %d: length differs: got %d want %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("iteration %d pos %d: %q != %q", i, j, got[j], first[j])
			}
		}
	}
}

// TestVLLMDisableServiceLinks asserts the VLLMBackend opts out of the legacy
// K8s service-link env-var injection. vLLM v0.20+ flags every K8s-injected
// VLLM_<svcname>_* env var as unknown; disabling service links suppresses the
// noise without affecting DNS service discovery.
func TestVLLMDisableServiceLinks(t *testing.T) {
	backend := &VLLMBackend{}
	if !backend.DisableServiceLinks() {
		t.Error("VLLMBackend.DisableServiceLinks() = false, want true (vLLM v0.20+ env validator noise)")
	}
}

// TestVLLMBuildCommand pins the explicit "vllm serve" entrypoint so the runtime
// is image-agnostic (parity with SGLang): the deployment builder sets
// container.Command from this, so BuildArgs' positional model path is served by
// vLLM rather than exec'd as the container process on a non-stock image.
func TestVLLMBuildCommand(t *testing.T) {
	b := &VLLMBackend{}
	want := []string{"vllm", "serve"}
	got := b.BuildCommand()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildCommand() = %v, want %v", got, want)
	}
}

// TestResolveEnableServiceLinks covers the wiring between ServiceLinksOptOut
// and the *bool the deployment builder writes to Pod.Spec.EnableServiceLinks.
// nil = K8s default (links on); explicit *false = links off.
func TestResolveEnableServiceLinks(t *testing.T) {
	cases := []struct {
		name       string
		backend    RuntimeBackend
		wantNil    bool
		wantValue  bool
		hasOptOut  bool
		expectFlag string
	}{
		{
			name:    "VLLMBackend opts out -> *false",
			backend: &VLLMBackend{},
			wantNil: false, wantValue: false,
		},
		{
			name:    "LlamaCppBackend does not implement opt-out -> nil (default)",
			backend: &LlamaCppBackend{},
			wantNil: true,
		},
		{
			name:    "GenericBackend does not implement opt-out -> nil (default)",
			backend: &GenericBackend{},
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEnableServiceLinks(tc.backend)
			if tc.wantNil {
				if got != nil {
					t.Errorf("got %v, want nil (default service links)", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want non-nil (explicit opt-out)")
			}
			if *got != tc.wantValue {
				t.Errorf("got *%v, want *%v", *got, tc.wantValue)
			}
		})
	}
}

// TestResolveKVCacheDtype covers the precedence rules for the custom-vs-standard
// KV cache type field. Direct unit tests on the resolver are easier to debug
// than going through BuildArgs and arg-list scanning.
func TestResolveKVCacheDtype(t *testing.T) {
	cases := []struct {
		name     string
		custom   string
		standard *string
		want     string
	}{
		{name: "both unset returns empty", custom: "", standard: nil, want: ""},
		{name: "standard nil and custom empty returns empty", custom: "", standard: nil, want: ""},
		{name: "standard set, custom empty returns standard", custom: "", standard: ptrString("fp8_e4m3"), want: "fp8_e4m3"},
		{name: "standard auto, custom empty returns auto", custom: "", standard: ptrString("auto"), want: "auto"},
		{name: "custom set, standard nil returns custom", custom: "turbo2", standard: nil, want: "turbo2"},
		{name: "custom set, standard set returns custom (custom wins)", custom: "turbo2", standard: ptrString("fp8_e5m2"), want: "turbo2"},
		{name: "custom set, standard auto returns custom", custom: "turbo2", standard: ptrString("auto"), want: "turbo2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveKVCacheDtype(tc.custom, tc.standard)
			if got != tc.want {
				t.Errorf("resolveKVCacheDtype(%q, %v) = %q, want %q", tc.custom, tc.standard, got, tc.want)
			}
		})
	}
}

// TestValidateVLLMConfig exercises the spec validator that feeds the
// VLLMSpecValid status condition.
func TestValidateVLLMConfig(t *testing.T) {
	cases := []struct {
		name       string
		isvc       *inferencev1alpha1.InferenceService
		wantReason string
	}{
		{
			name:       "nil isvc is valid",
			isvc:       nil,
			wantReason: "",
		},
		{
			name: "nil vllm config is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm"},
			},
			wantReason: "",
		},
		{
			name: "speculative disabled is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						Speculative: &inferencev1alpha1.SpeculativeConfig{Enabled: ptrBool(false)},
					},
				},
			},
			wantReason: "",
		},
		{
			name: "speculative enabled with model is valid",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						Speculative: &inferencev1alpha1.SpeculativeConfig{
							Enabled: ptrBool(true),
							Model:   "draft-model",
						},
					},
				},
			},
			wantReason: "",
		},
		{
			name: "speculative enabled without model reports SpeculativeMissingModel",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						Speculative: &inferencev1alpha1.SpeculativeConfig{Enabled: ptrBool(true)},
					},
				},
			},
			wantReason: "SpeculativeMissingModel",
		},
		{
			name: "cpuOffloadGB set surfaces CPUOffloadUnverified",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						CPUOffloadGB: ptrInt32(4),
					},
				},
			},
			wantReason: "CPUOffloadUnverified",
		},
		{
			name: "cpuOffloadGB zero is not warned",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						CPUOffloadGB: ptrInt32(0),
					},
				},
			},
			wantReason: "",
		},
		{
			name: "speculative-missing outranks cpu-offload (pins precedence)",
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime: "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						CPUOffloadGB: ptrInt32(4),
						Speculative:  &inferencev1alpha1.SpeculativeConfig{Enabled: ptrBool(true)},
					},
				},
			},
			wantReason: "SpeculativeMissingModel",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, message := ValidateVLLMConfig(tc.isvc)
			if reason != tc.wantReason {
				t.Errorf("reason: got %q want %q (message=%q)", reason, tc.wantReason, message)
			}
			if reason != "" && message == "" {
				t.Errorf("expected non-empty message when reason is set, got empty")
			}
		})
	}
}

// TestNeedsCPUOffloadNoopWarning covers the event predicate for #1320. The
// runtime gate matters: the event call site is not runtime-scoped, so a
// llamacpp service with a leftover vllmConfig must not warn.
func TestNeedsCPUOffloadNoopWarning(t *testing.T) {
	mk := func(runtime string, gb *int32) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: runtime},
		}
		if gb != nil {
			isvc.Spec.VLLMConfig = &inferencev1alpha1.VLLMConfig{CPUOffloadGB: gb}
		}
		return isvc
	}
	cases := []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
		want bool
	}{
		{"vllm with offload set", mk("vllm", ptrInt32(4)), true},
		{"llamacpp with vllmConfig leftover", mk("llamacpp", ptrInt32(4)), false},
		{"vllm with zero", mk("vllm", ptrInt32(0)), false},
		{"vllm with nil config", mk("vllm", nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsCPUOffloadNoopWarning(tc.isvc); got != tc.want {
				t.Fatalf("needsCPUOffloadNoopWarning() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVLLMBuildArgsPipelineParallelSize covers vllmConfig.pipelineParallelSize:
// emitted when above 1, silent at 1 (vLLM's default) and when unset.
func TestVLLMBuildArgsPipelineParallelSize(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "org/name"},
	}
	pp := int32(2)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:    "vllm",
			VLLMConfig: &inferencev1alpha1.VLLMConfig{PipelineParallelSize: &pp},
		},
	}
	args := (&VLLMBackend{}).BuildArgs(isvc, model, "/models/m", "", 8000)
	if got := argValue(args, "--pipeline-parallel-size"); got != "2" {
		t.Fatalf("--pipeline-parallel-size = %q, want 2 in %v", got, args)
	}

	one := int32(1)
	isvc.Spec.VLLMConfig.PipelineParallelSize = &one
	args = (&VLLMBackend{}).BuildArgs(isvc, model, "/models/m", "", 8000)
	for _, a := range args {
		if a == "--pipeline-parallel-size" {
			t.Fatalf("pipelineParallelSize 1 must not emit a flag: %v", args)
		}
	}
}

// multiNodeISVCForTest is the two-Spark ring pair: rank 0 on dgx3, rank 1 on
// dgx1, each end of the same leg with its own NIC and HCA names.
func multiNodeISVCForTest() *inferencev1alpha1.InferenceService {
	tp := int32(2)
	gid := int32(3)
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "ring", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:    "vllm",
			VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp},
			MultiNode: &inferencev1alpha1.MultiNodeSpec{
				IBGIDIndex: &gid,
				Members: []inferencev1alpha1.MultiNodeMember{
					{Node: "dgx3", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.10.4.1", SocketInterface: "enp1s0f0np0", IBHCA: "rocep1s0f0"}},
					{Node: "dgx1", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.10.4.2", SocketInterface: "enp1s0f1np1", IBHCA: "rocep1s0f1"}},
				},
			},
		},
	}
}

func TestVLLMBuildMultiNodeArgs(t *testing.T) {
	isvc := multiNodeISVCForTest()
	b := &VLLMBackend{}

	head := b.BuildMultiNodeArgs(isvc, 0)
	for _, want := range [][2]string{
		{"--nnodes", "2"}, {"--node-rank", "0"}, {"--master-addr", "10.10.4.1"},
		{"--master-port", "29500"}, {"--distributed-executor-backend", "mp"},
	} {
		if !containsArg(head, want[0], want[1]) {
			t.Errorf("rank 0 missing %s %s in %v", want[0], want[1], head)
		}
	}
	if containsArg(head, "--headless", "") {
		t.Errorf("rank 0 must not be headless: %v", head)
	}

	worker := b.BuildMultiNodeArgs(isvc, 1)
	if !containsArg(worker, "--node-rank", "1") {
		t.Errorf("rank 1 missing --node-rank 1 in %v", worker)
	}
	if !containsArg(worker, "--headless", "") {
		t.Errorf("rank 1 must be --headless: %v", worker)
	}

	port := int32(31000)
	isvc.Spec.MultiNode.RendezvousPort = &port
	if got := argValue(b.BuildMultiNodeArgs(isvc, 1), "--master-port"); got != "31000" {
		t.Errorf("--master-port = %q, want 31000", got)
	}
	if got := b.BuildMultiNodeArgs(&inferencev1alpha1.InferenceService{}, 0); got != nil {
		t.Errorf("no multiNode block must yield nil args, got %v", got)
	}
}

func TestVLLMBuildMultiNodeEnv(t *testing.T) {
	isvc := multiNodeISVCForTest()
	env := (&VLLMBackend{}).BuildMultiNodeEnv(isvc, 1)
	want := map[string]string{
		"VLLM_HOST_IP":       "10.10.4.2",
		"NCCL_SOCKET_IFNAME": "enp1s0f1np1",
		"GLOO_SOCKET_IFNAME": "enp1s0f1np1",
		"TP_SOCKET_IFNAME":   "enp1s0f1np1",
		"NCCL_IB_HCA":        "rocep1s0f1",
		"NCCL_IB_GID_INDEX":  "3",
	}
	got := envMap(env)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (env %v)", k, got[k], v, env)
		}
	}
	if len(env) != len(want) {
		t.Errorf("unexpected extra env: %v", env)
	}

	// A member with no fabric block and no GID index yields nothing: NCCL
	// enumerates, VLLM_HOST_IP is left to vLLM.
	isvc.Spec.MultiNode.Members[1].Fabric = nil
	isvc.Spec.MultiNode.IBGIDIndex = nil
	if env := (&VLLMBackend{}).BuildMultiNodeEnv(isvc, 1); len(env) != 0 {
		t.Errorf("expected no env for a member without fabric, got %v", env)
	}
}

// envMap indexes a container env by name for assertions.
func envMap(env []corev1.EnvVar) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

// TestVLLMCustomizeProbes pins the stall-detection wiring (#2020): the exec
// liveness probe renders only when the feature is opted into, the window and
// port land in the script, startup and readiness stay on /health, and the
// off cases are byte-identical to the probes BuildProbes renders today.
func TestVLLMCustomizeProbes(t *testing.T) {
	on, off := true, false
	w90 := int32(90)
	cases := []struct {
		name       string
		vllmConfig *inferencev1alpha1.VLLMConfig
		wantWindow int32 // 0 means the liveness probe must stay /health
	}{
		{name: "no vllmConfig", vllmConfig: nil, wantWindow: 0},
		{name: "stallDetection absent", vllmConfig: &inferencev1alpha1.VLLMConfig{}, wantWindow: 0},
		{name: "enabled absent", vllmConfig: &inferencev1alpha1.VLLMConfig{
			StallDetection: &inferencev1alpha1.VLLMStallDetection{},
		}, wantWindow: 0},
		{name: "enabled false", vllmConfig: &inferencev1alpha1.VLLMConfig{
			StallDetection: &inferencev1alpha1.VLLMStallDetection{Enabled: &off},
		}, wantWindow: 0},
		{name: "enabled, default window", vllmConfig: &inferencev1alpha1.VLLMConfig{
			StallDetection: &inferencev1alpha1.VLLMStallDetection{Enabled: &on},
		}, wantWindow: 120},
		{name: "enabled, custom window", vllmConfig: &inferencev1alpha1.VLLMConfig{
			StallDetection: &inferencev1alpha1.VLLMStallDetection{Enabled: &on, WindowSeconds: &w90},
		}, wantWindow: 90},
	}

	const port = int32(8000)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{VLLMConfig: tc.vllmConfig},
			}
			backend := &VLLMBackend{}
			startup, liveness, readiness := backend.BuildProbes(port)
			startupBefore, livenessBefore, readinessBefore := *startup, *liveness, *readiness

			backend.CustomizeProbes(isvc, port, startup, liveness, readiness)

			if tc.wantWindow == 0 {
				if !reflect.DeepEqual(&livenessBefore, liveness) {
					t.Errorf("liveness probe changed with stall detection off: %+v", *liveness)
				}
				if !reflect.DeepEqual(&startupBefore, startup) {
					t.Errorf("startup probe changed with stall detection off: %+v", *startup)
				}
				if !reflect.DeepEqual(&readinessBefore, readiness) {
					t.Errorf("readiness probe changed with stall detection off: %+v", *readiness)
				}
				return
			}

			if liveness.Exec == nil {
				t.Fatalf("liveness probe = %+v, want an exec probe", liveness)
			}
			if liveness.HTTPGet != nil {
				t.Errorf("exec liveness still carries an HTTPGet handler")
			}
			wantCmd := stallProbeCommand(port, tc.wantWindow)
			if !reflect.DeepEqual(liveness.Exec.Command, wantCmd) {
				t.Errorf("exec command = %q, want the rendered stallProbeCommand %q",
					liveness.Exec.Command, wantCmd)
			}
			if got := liveness.Exec.Command[0]; got != "python3" {
				t.Errorf("exec interpreter = %q, want python3", got)
			}
			script := liveness.Exec.Command[2]
			// localhost, not 127.0.0.1: a server bound IPv6-only refuses
			// IPv4 loopback (#2027).
			if !strings.Contains(script, "localhost:8000/metrics") {
				t.Errorf("stall script does not target the local metrics endpoint")
			}
			if !strings.Contains(script, strconv.FormatInt(int64(tc.wantWindow), 10)) {
				t.Errorf("stall script does not carry window %d", tc.wantWindow)
			}
			// Startup and readiness must stay on /health regardless.
			if !reflect.DeepEqual(&startupBefore, startup) || !reflect.DeepEqual(&readinessBefore, readiness) {
				t.Errorf("startup/readiness probes must stay on /health: %+v / %+v", *startup, *readiness)
			}
			if startup.HTTPGet == nil || startup.HTTPGet.Path != "/health" ||
				readiness.HTTPGet == nil || readiness.HTTPGet.Path != "/health" {
				t.Errorf("startup/readiness lost the /health handler: %+v / %+v", startup, readiness)
			}
		})
	}
}
