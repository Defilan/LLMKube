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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Argument builders for the vllm runtime. Each helper takes the current
// args slice plus the relevant CRD field and returns the appended slice (or
// the unchanged slice when the field is unset or not applicable). Kept as
// free functions so they are trivially testable in isolation and can be
// composed in any order from the deployment builder.

func appendAttentionBackend(args []string, attentionBackend string) []string {
	if attentionBackend != "" {
		return append(args, "--attention-backend", attentionBackend)
	}
	return args
}

func appendCPUOffloadGB(args []string, cpuOffloadGB *int32, gpuPresent bool) ([]string, error) {
	if cpuOffloadGB != nil {
		// NOTE(fvoituret): --cpu-offload-gb CLI args is only GPU related so we prevent
		// adding it in a non GPU context.
		if !gpuPresent {
			return args, errors.New(
				"spec.vllmConfig.cpuOffloadGB is defined with no GPU hardware; skipping --cpu-offload-gb flags",
			)
		}
		return append(args, "--cpu-offload-gb", fmt.Sprintf("%d", *cpuOffloadGB)), nil
	}
	return args, nil
}

func appendDtype(args []string, dtype string) []string {
	if dtype != "" {
		return append(args, "--dtype", dtype)
	}
	return args
}

// appendEnableChunkedPrefill only emit when user explicitly opted in (true).
// vLLM's own default handles the nil/false case.
func appendEnableChunkedPrefill(args []string, enableChunkedPrefill *bool) []string {
	if enableChunkedPrefill != nil && *enableChunkedPrefill {
		return append(args, "--enable-chunked-prefill")
	}
	return args
}

func appendEnableExpertParallel(args []string, enableExpertParallel *bool) []string {
	if enableExpertParallel != nil && *enableExpertParallel {
		return append(args, "--enable-expert-parallel")
	}
	return args
}

// appendEnablePrefixCaching only emit when user explicitly opted in (true).
// vLLM's own default handles the nil/false case.
func appendEnablePrefixCaching(args []string, enablePrefixCaching *bool) []string {
	if enablePrefixCaching != nil && *enablePrefixCaching {
		return append(args, "--enable-prefix-caching")
	}
	return args
}

func appendGPUMemoryUtilization(args []string, gpuMemoryUtilization *float64, gpuPresent bool) ([]string, error) {
	if gpuMemoryUtilization != nil {
		// NOTE(fvoituret): --gpu-memory-utilization CLI args is only GPU related so we prevent
		// adding it in a non GPU context.
		if !gpuPresent {
			return args, errors.New(
				"spec.vllmConfig.gpuMemoryUtilization is defined with no GPU hardware; skipping --gpu-memory-utilization flags",
			)
		}
		return append(args, "--gpu-memory-utilization", strconv.FormatFloat(*gpuMemoryUtilization, 'f', -1, 64)), nil
	}
	return args, nil
}

// resolveKVCacheDtype returns the custom vLLM KV cache type when set,
// otherwise the enum-validated standard value (dereferenced; nil → ""). Lets
// users opt into vLLM image-specific cache formats (TurboQuant turbo2 from
// v0.20+, future variants) without expanding the CRD enum on every release,
// while keeping the standard field discoverable for the common case. Mirrors
// resolveCacheType on the llama.cpp side.
func resolveKVCacheDtype(custom string, standard *string) string {
	if custom != "" {
		return custom
	}
	if standard == nil {
		return ""
	}
	return *standard
}

// appendKVCacheDtype emit flag unless unset or explicitly "auto" (vLLM's default).
func appendKVCacheDtype(args []string, kvCacheDtype *string, kvCacheCustomDtype string) []string {
	if resolved := resolveKVCacheDtype(kvCacheCustomDtype, kvCacheDtype); resolved != "" && resolved != "auto" {
		args = append(args, "--kv-cache-dtype", resolved)
	}
	return args
}

func appendQuantization(args []string, quantization string) []string {
	if quantization != "" {
		return append(args, "--quantization", quantization)
	}
	return args
}

// appendToolCallParser emits the parser and the auto tool choice flag that
// makes it take effect. The two are guarded separately so an extraArgs value
// suppresses only its own flag: vLLM renders a parser inert unless auto tool
// choice is on, so pairing a user-supplied parser with the flag is the point.
func appendToolCallParser(args []string, parser string, extraArgs []string) []string {
	if parser == "" {
		return args
	}
	if !hasMatchingExtraArg(extraArgs, "tool-call-parser") {
		args = append(args, "--tool-call-parser", parser)
	}
	if !hasMatchingExtraArg(extraArgs, "enable-auto-tool-choice") {
		args = append(args, "--enable-auto-tool-choice")
	}
	return args
}

func appendReasoningParser(args []string, parser string, extraArgs []string) []string {
	if parser != "" && !hasMatchingExtraArg(extraArgs, "reasoning-parser") {
		return append(args, "--reasoning-parser", parser)
	}
	return args
}

func appendMaxModelLen(args []string, maxModelLen *int32) []string {
	if maxModelLen != nil {
		return append(args, "--max-model-len", fmt.Sprintf("%d", *maxModelLen))
	}
	return args
}

func appendMaxNumBatchedTokens(args []string, maxNumBatchedTokens *int32) []string {
	if maxNumBatchedTokens != nil {
		return append(args, "--max-num-batched-tokens", fmt.Sprintf("%d", *maxNumBatchedTokens))
	}
	return args
}

func appendMaxNumSeqsArgs(args []string, parallelSlots *int32, extraArgs []string) ([]string, error) {
	// NOTE(#339): extra args has precedence.
	if parallelSlots != nil && *parallelSlots >= 1 {
		if hasMatchingExtraArg(extraArgs, "max-num-seqs") {
			return args, errors.New("spec.parallelSlots is enabled but `--max-num-seqs` is already defined in spec.ExtraArgs, skipping")
		}
		return append(args, "--max-num-seqs", fmt.Sprintf("%d", *parallelSlots)), nil
	}
	return args, nil
}

// appendSpeculativeConfig renders the block as the one argument vLLM accepts
// for speculative decoding. vLLM registers no per-field speculative flags: its
// parser takes --speculative-config (JSON) and builds a dataclass that forbids
// unknown keys (vllm/config/utils.py, extra="forbid"), so a key that does not
// exist in the image you run fails the engine at startup. Method-specific keys
// (draft_sample_method, prompt_lookup_max, ...) therefore belong in
// spec.extraArgs as --speculative-config, which wins over this block, rather
// than in typed fields this operator cannot version-check.
// vllmSpeculativeArgSpellings are every extraArgs spelling that writes the same
// speculative document the typed block renders: vLLM registers these flags as
// descriptions of one SpeculativeConfig and raises when a key appears both in
// --speculative-config and in one of the shorthands, so a user setting any of
// them owns the argument (vllm/engine/arg_utils.py at v0.25.1, the -sc alias at
// 1514 and the mutual-exclusion check at 1724).
var vllmSpeculativeArgSpellings = []string{"speculative-config", "-sc", "spec-method", "spec-model", "spec-tokens"}

// extraArgsSetsVLLMSpeculative reports whether the user configures speculative
// decoding themselves, in any of vLLM's spellings.
func extraArgsSetsVLLMSpeculative(extraArgs []string) bool {
	return hasMatchingExtraArgAny(extraArgs, vllmSpeculativeArgSpellings...)
}

func appendSpeculativeConfig(args []string, cfg *inferencev1alpha1.SpeculativeConfig, draftPath string, extraArgs []string) ([]string, error) {
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled {
		return args, nil
	}
	if extraArgsSetsVLLMSpeculative(extraArgs) {
		return args, nil
	}
	if cfg.Model != "" && draftPath == "" {
		return args, errors.New("spec.vllmConfig.speculative.model is set but nothing staged the draft; skipping --speculative-config")
	}
	if cfg.Model == "" && cfg.Method == "" {
		return args, errors.New("spec.vllmConfig.speculative.enabled is true but neither model nor method is set; skipping --speculative-config")
	}

	payload := vllmSpeculativeConfig{NumSpeculativeTokens: vllmDefaultSpeculativeTokens}
	if cfg.Method != "" {
		payload.Method = cfg.Method
	}
	// Keyed on cfg.Model, not on draftPath: draftPath belongs to whichever
	// Model the resolver picked, and a method that needs no draft must not
	// carry the path of one.
	if cfg.Model != "" {
		// The path, not the CR name: vLLM loads what the pod mounted.
		payload.Model = draftPath
		if payload.Method == "" {
			payload.Method = "draft_model"
		}
	}
	if cfg.NumSpeculativeTokens != nil {
		payload.NumSpeculativeTokens = *cfg.NumSpeculativeTokens
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return args, fmt.Errorf("cannot render spec.vllmConfig.speculative as JSON: %w", err)
	}
	return append(args, "--speculative-config", string(raw)), nil
}

// vllmSpeculativeConfig is the JSON document vLLM's SpeculativeConfig parses.
// Field names and order are the wire contract: reordering only changes the
// rendered argv, but a rename silently becomes an unknown key, which vLLM's
// parser rejects.
type vllmSpeculativeConfig struct {
	Method               string `json:"method,omitempty"`
	Model                string `json:"model,omitempty"`
	NumSpeculativeTokens int32  `json:"num_speculative_tokens"`
}

// vllmDefaultSpeculativeTokens is the CRD default for
// spec.vllmConfig.speculative.numSpeculativeTokens. It is also what the
// apiserver would default the field to; the fallback here keeps the rendered
// JSON valid when the Go value reaches us undefaulted, because vLLM refuses a
// draft model without a token count.
const vllmDefaultSpeculativeTokens int32 = 4

func appendTensorParallelSize(args []string, tensorParallelSize *int32) []string {
	if tensorParallelSize != nil && *tensorParallelSize > 1 {
		return append(args, "--tensor-parallel-size", fmt.Sprintf("%d", *tensorParallelSize))
	}
	return args
}

// appendPipelineParallelSize emits --pipeline-parallel-size when set above 1.
// 1 is vLLM's default and emitting it would only add noise to the argv.
func appendPipelineParallelSize(args []string, pipelineParallelSize *int32) []string {
	if pipelineParallelSize == nil || *pipelineParallelSize <= 1 {
		return args
	}
	return append(args, "--pipeline-parallel-size", fmt.Sprintf("%d", *pipelineParallelSize))
}

// needsCPUOffloadNoopWarning returns true when the vLLM runtime is selected
// and spec.vllmConfig.cpuOffloadGB is set to a positive value. The flag is a
// known silent no-op on current vLLM (vllm-project/vllm#48468), so the
// controller emits a warning event: the field targets VRAM-constrained boxes,
// where the no-op surfaces as an unexplained OOM rather than an error. The
// runtime gate matters because the caller is not runtime-scoped.
func needsCPUOffloadNoopWarning(isvc *inferencev1alpha1.InferenceService) bool {
	return isvc.Spec.Runtime == RuntimeVLLM &&
		isvc.Spec.VLLMConfig != nil &&
		isvc.Spec.VLLMConfig.CPUOffloadGB != nil &&
		*isvc.Spec.VLLMConfig.CPUOffloadGB > 0
}
