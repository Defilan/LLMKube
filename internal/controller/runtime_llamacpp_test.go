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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestLlamaCppBuildArgs is the single table-driven test that covers every new
// agentic-coding flag and the "not emitted when unset/false" counterpart.
// Each row asserts a set of must-contain and must-not-contain flags on the
// generated arg list.
func TestLlamaCppBuildArgs(t *testing.T) {
	backend := &LlamaCppBackend{}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	const modelPath = "/models/test"
	const port = int32(8000)

	cases := []struct {
		// contains is a slice of flag/value pairs ("" value means "flag must be
		// present as a bare toggle").
		contains []FlagCheck
		// notContains is just a list of flags that must NOT appear anywhere in args.
		notContains []string
		model       *inferencev1alpha1.Model
		modelPath   string
		name        string
		spec        *inferencev1alpha1.InferenceServiceSpec
	}{
		{
			model: model,
			name:  "empty config emits only base flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--ctx-size", "--parallel", "--flash-attn", "--jinja", "--cache-type-k", "--cpu-moe", "--n-cpu-moe", "--no-kv-offload", "--override-tensor", "--override-kv", "--batch-size", "--ubatch-size", "--no-warmup", "--reasoning-budget", "--reasoning-budget-message", "--mmproj"},
		},
		{
			// #972: bind the dual-stack wildcard (::), not 0.0.0.0, so pods are
			// reachable on IPv6-only clusters. :: still accepts IPv4 (default
			// bindv6only=0), so IPv4-only clusters are unaffected.
			model: model,
			name:  "binds dual-stack wildcard :: for IPv6-only clusters",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			contains:    []FlagCheck{{"--host", "::"}},
			notContains: []string{"0.0.0.0"},
		},
		{
			model: model,
			name:  "contextSize set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:     "llama",
				ModelRef:    "test-model",
				ContextSize: ptrInt32(8192),
			},
			contains: []FlagCheck{{"--ctx-size", "8192"}},
		},
		{
			model: model,
			name:  "contextSize nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--ctx-size"},
		},
		{
			model: model,
			name:  "ropeScaling set emits rope flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				RopeScaling: &inferencev1alpha1.RopeScalingSpec{
					Type:            "yarn",
					Factor:          "2.0",
					OriginalContext: ptrInt32(131072),
				},
			},
			contains: []FlagCheck{{"--rope-scaling", "yarn"}, {"--rope-scale", "2.0"}, {"--yarn-orig-ctx", "131072"}},
		},
		{
			model: model,
			name:  "ropeScaling nil does not emit rope flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--rope-scaling", "--rope-scale", "--yarn-orig-ctx"},
		},
		{
			model: model,
			name:  "ropeScaling skipped when extraArgs already sets --rope-scaling",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:     "llama",
				ModelRef:    "test-model",
				ExtraArgs:   []string{"--rope-scaling", "linear"},
				RopeScaling: &inferencev1alpha1.RopeScalingSpec{Type: "yarn", Factor: "2.0"},
			},
			// extraArgs wins: the whole rope block is skipped, so --rope-scale
			// is absent and the only --rope-scaling is the extraArgs value.
			contains:    []FlagCheck{{"--rope-scaling", "linear"}},
			notContains: []string{"--rope-scale", "--yarn-orig-ctx"},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (without extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				ParallelSlots: ptrInt32(8192),
			},
			contains: []FlagCheck{{"--parallel", "8192"}},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (with extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				ExtraArgs:     []string{"--parallel", "8"},
				ParallelSlots: ptrInt32(4),
			},
			// NOTE: Since extraArgs are always last in position but still have priority
			// and containsArgs helper function always validate first occurrence, having
			// --parallel 8 case true mean that no duplicate due to parallelSlots was
			// found along the way.
			contains: []FlagCheck{{"--parallel", "8"}},
		},
		{
			model: model,
			name:  "parallelSlots set emits flag (with extraArgs inline precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				ExtraArgs:     []string{"--parallel=8"},
				ParallelSlots: ptrInt32(4),
			},
			// NOTE: Since extraArgs are always last in position but still have priority
			// and containsArgs helper function always validate first occurrence, having
			// --parallel 8 case true mean that no duplicate due to parallelSlots was
			// found along the way.
			contains: []FlagCheck{{"--parallel=8", ""}},
		},
		{
			model: model,
			name:  "parallelSlots nil does not emit flag (without extraArgs precedence)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--parallel"},
		},
		{
			model: model,
			name:  "batchSize set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:   "llama",
				ModelRef:  "test-model",
				BatchSize: ptrInt32(8192),
			},
			contains: []FlagCheck{{"--batch-size", "8192"}},
		},
		{
			model: model,
			name:  "batchSize nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--batch-size"},
		},
		{
			model: model,
			name:  "uBatchSize set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "llama",
				ModelRef:   "test-model",
				UBatchSize: ptrInt32(8192),
			},
			contains: []FlagCheck{{"--ubatch-size", "8192"}},
		},
		{
			model: model,
			name:  "uBatchSize nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--ubatch-size"},
		},
		{
			model: model,
			name:  "moeCPULayers set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:      "llama",
				ModelRef:     "test-model",
				MoeCPULayers: ptrInt32(8192),
			},
			contains: []FlagCheck{{"--n-cpu-moe", "8192"}},
		},
		{
			model: model,
			name:  "moeCPULayers nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--n-cpu-moe"},
		},
		{
			model: model,
			name:  "flashAttention=true does not emit flag without GPU",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:        "llama",
				ModelRef:       "test-model",
				FlashAttention: ptrBool(true),
			},
			notContains: []string{"--flash-attn"},
		},
		{
			model: model,
			name:  "flashAttention=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:        "llama",
				ModelRef:       "test-model",
				FlashAttention: ptrBool(false),
			},
			notContains: []string{"--flash-attn"},
		},
		{
			model: model,
			name:  "jinja=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				Jinja:    ptrBool(true),
			},
			contains: []FlagCheck{{"--jinja", ""}},
		},
		{
			model: model,
			name:  "jinja=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				Jinja:    ptrBool(false),
			},
			notContains: []string{"--jinja"},
		},
		{
			model: model,
			name:  "moeCPUOffload=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				MoeCPUOffload: ptrBool(true),
			},
			contains: []FlagCheck{{"--cpu-moe", ""}},
		},
		{
			model: model,
			name:  "moeCPUOffload=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:       "llama",
				ModelRef:      "test-model",
				MoeCPUOffload: ptrBool(false),
			},
			notContains: []string{"--cpu-moe"},
		},
		{
			model: model,
			name:  "noKVOffload=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:     "llama",
				ModelRef:    "test-model",
				NoKvOffload: ptrBool(true),
			},
			contains: []FlagCheck{{"--no-kv-offload", ""}},
		},
		{
			model: model,
			name:  "noKVOffload=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:     "llama",
				ModelRef:    "test-model",
				NoKvOffload: ptrBool(false),
			},
			notContains: []string{"--no-kv-offload"},
		},
		{
			model: model,
			name:  "noWarmup=true emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				NoWarmup: ptrBool(true),
			},
			contains: []FlagCheck{{"--no-warmup", ""}},
		},
		{
			model: model,
			name:  "noWarmup=false does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				NoWarmup: ptrBool(false),
			},
			notContains: []string{"--no-warmup"},
		},
		{
			model: model,
			name:  "reasoningBudget set emits flag (without message)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:         "llama",
				ModelRef:        "test-model",
				ReasoningBudget: ptrInt32(8192),
			},
			contains: []FlagCheck{{"--reasoning-budget", "8192"}},
		},
		{
			model: model,
			name:  "reasoningBudget set emits flag (with message)",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:                "llama",
				ModelRef:               "test-model",
				ReasoningBudget:        ptrInt32(8192),
				ReasoningBudgetMessage: "message",
			},
			contains: []FlagCheck{
				{"--reasoning-budget", "8192"},
				{"--reasoning-budget-message", "message"},
			},
		},
		{
			model: model,
			name:  "cacheTypeK set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "llama",
				ModelRef:   "test-model",
				CacheTypeK: "key",
			},
			contains: []FlagCheck{{"--cache-type-k", "key"}},
		},
		{
			model: model,
			name:  "cacheTypeK nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--cache-type-k"},
		},
		{
			model: model,
			name:  "cacheTypeV set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:    "llama",
				ModelRef:   "test-model",
				CacheTypeV: "value",
			},
			contains: []FlagCheck{{"--cache-type-v", "value"}},
		},
		{
			model: model,
			name:  "cacheTypeV nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--cache-type-v"},
		},
		{
			model: model,
			name:  "tensorOverride set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:         "llama",
				ModelRef:        "test-model",
				TensorOverrides: []string{"value1", "value2"},
			},
			contains: []FlagCheck{
				{"--override-tensor", "value1"},
				{"--override-tensor", "value2"},
			},
		},
		{
			model: model,
			name:  "tensorOverrides nil does not emit flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--override-tensor"},
		},
		{
			model: model,
			name:  "metadataOverride set emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:           "llama",
				ModelRef:          "test-model",
				MetadataOverrides: []string{"value1", "value2"},
			},
			contains: []FlagCheck{
				{"--override-kv", "value1"},
				{"--override-kv", "value2"},
			},
		},
		{
			model: model,
			name:  "metadataOverride nil does not emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--override-kv"},
		},
		{
			model: model,
			name:  "full agentic config emits all flags together",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:                "llama",
				ModelRef:               "test-model",
				BatchSize:              ptrInt32(2),
				CacheTypeK:             "key",
				CacheTypeV:             "value",
				ContextSize:            ptrInt32(2),
				FlashAttention:         ptrBool(true),
				Jinja:                  ptrBool(true),
				MetadataOverrides:      []string{"value1", "value2"},
				MoeCPUOffload:          ptrBool(true),
				MoeCPULayers:           ptrInt32(2),
				NoKvOffload:            ptrBool(true),
				NoWarmup:               ptrBool(true),
				ParallelSlots:          ptrInt32(2),
				ReasoningBudget:        ptrInt32(2),
				ReasoningBudgetMessage: "message",
				TensorOverrides:        []string{"value1", "value2"},
				UBatchSize:             ptrInt32(2),
			},
			contains: []FlagCheck{
				{"--batch-size", "2"},
				{"--cache-type-k", "key"},
				{"--cache-type-v", "value"},
				{"--ctx-size", "2"},
				{"--jinja", ""},
				{"--override-kv", "value1"},
				{"--override-kv", "value2"},
				{"--cpu-moe", ""},
				{"--n-cpu-moe", "2"},
				{"--n-cpu-moe", "2"},
				{"--no-kv-offload", ""},
				{"--no-warmup", ""},
				{"--parallel", "2"},
				{"--reasoning-budget", "2"},
				{"--reasoning-budget-message", "message"},
				{"--override-tensor", "value1"},
				{"--override-tensor", "value2"},
				{"--ubatch-size", "2"},
			},
		},
		{
			model: model,
			name:  "speculativeDecoding mtp emits --spec-type draft-mtp",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type: "mtp",
				},
			},
			contains: []FlagCheck{{"--spec-type", "draft-mtp"}},
		},
		{
			model: model,
			name:  "speculativeDecoding draft emits --spec-type draft-simple",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type: "draft",
				},
			},
			contains: []FlagCheck{{"--spec-type", "draft-simple"}},
		},
		{
			model: model,
			name:  "speculativeDecoding mtp with nDraftMax emits both flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type:      "mtp",
					NDraftMax: ptrInt32(5),
				},
			},
			contains: []FlagCheck{
				{"--spec-type", "draft-mtp"},
				{"--spec-draft-n-max", "5"},
			},
		},
		{
			model: model,
			name:  "speculativeDecoding disabled does not emit flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type: "disabled",
				},
			},
			notContains: []string{"--spec-type", "--spec-draft-n-max"},
		},
		{
			model: model,
			name:  "speculativeDecoding nil does not emit flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--spec-type", "--spec-draft-n-max"},
		},
		{
			model: model,
			name:  "speculativeDecoding empty type does not emit flags",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type: "",
				},
			},
			notContains: []string{"--spec-type", "--spec-draft-n-max"},
		},
		{
			model: model,
			name:  "speculativeDecoding mtp without nDraftMax does not emit --spec-draft-n-max",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type: "mtp",
				},
			},
			contains:    []FlagCheck{{"--spec-type", "draft-mtp"}},
			notContains: []string{"--spec-draft-n-max"},
		},
		{
			model: &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Mmproj: "mmproj-F16.gguf",
				},
			},
			name: "mmproj without files does not emit managed flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--mmproj", "/models/mmproj-F16.gguf"},
		},
		{
			model: &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Files:  []string{"model.gguf"},
					Mmproj: "mmproj-F16.gguf",
				},
			},
			name: "mmproj with files emits flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			contains: []FlagCheck{{"--mmproj", "/models/mmproj-F16.gguf"}},
		},
		{
			model: &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Mmproj: "mmproj-F16.gguf",
				},
			},
			name: "mmproj skipped when extraArgs already sets flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:   "llama",
				ModelRef:  "test-model",
				ExtraArgs: []string{"--mmproj", "/custom/mmproj.gguf"},
			},
			contains:    []FlagCheck{{"--mmproj", "/custom/mmproj.gguf"}},
			notContains: []string{"/models/mmproj-F16.gguf"},
		},
		{
			model: &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Mmproj: "mmproj-F16.gguf",
				},
			},
			name: "mmproj skipped when extraArgs sets inline flag",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:   "llama",
				ModelRef:  "test-model",
				ExtraArgs: []string{"--mmproj=/custom/mmproj.gguf"},
			},
			contains:    []FlagCheck{{"--mmproj=/custom/mmproj.gguf", ""}},
			notContains: []string{"/models/mmproj-F16.gguf"},
		},
		{
			model: &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Files:  []string{"subdir/model.gguf"},
					Mmproj: "mmproj-F16.gguf",
				},
			},
			modelPath: "/models/cache/subdir/model.gguf",
			name:      "mmproj path uses cache root when model in subdirectory",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			contains: []FlagCheck{{"--mmproj", "/models/cache/mmproj-F16.gguf"}},
		},
		{
			model: &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Files:  []string{"../escape.gguf"},
					Mmproj: "mmproj-F16.gguf",
				},
			},
			name: "mmproj not emitted when ResolveFileSet errors due to invalid files",
			spec: &inferencev1alpha1.InferenceServiceSpec{
				Runtime:  "llama",
				ModelRef: "test-model",
			},
			notContains: []string{"--mmproj"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "isvc-" + strings.ReplaceAll(tc.name, " ", "-"), Namespace: "default"},
				Spec:       *tc.spec,
			}
			mp := tc.modelPath
			if mp == "" {
				mp = modelPath
			}
			args := backend.BuildArgs(isvc, tc.model, mp, "", port)
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
		})
	}
}

// countArg returns how many times flag appears verbatim in args.
func countArg(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

// TestLlamaCppMetricsNotDuplicated covers #1384: the operator appended
// --metrics unconditionally while ExtraArgs was appended verbatim, so a user
// who set --metrics themselves got it twice. Both llama.cpp backends are
// checked because they append ExtraArgs on opposite sides of the operator's
// own flags, so a positional fix would only have corrected one of them.
func TestLlamaCppMetricsNotDuplicated(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Format: "gguf"},
	}

	cases := []struct {
		name      string
		extraArgs []string
		want      int
	}{
		{name: "operator supplies it when the user does not", extraArgs: nil, want: 1},
		{name: "bare user flag is not duplicated", extraArgs: []string{"--metrics"}, want: 1},
		{name: "inline user flag is not duplicated", extraArgs: []string{"--metrics=true"}, want: 0},
	}

	for _, tc := range cases {
		for _, backend := range []struct {
			name string
			args func(*inferencev1alpha1.InferenceService) []string
		}{
			{"single-model", func(isvc *inferencev1alpha1.InferenceService) []string {
				return (&LlamaCppBackend{}).BuildArgs(isvc, model, "/models/m.gguf", "", 8080)
			}},
			{"router", func(isvc *inferencev1alpha1.InferenceService) []string {
				return (&LlamaCppRouterBackend{}).BuildArgs(isvc, model, "/models/m.gguf", "", 8080)
			}},
		} {
			t.Run(backend.name+": "+tc.name, func(t *testing.T) {
				isvc := &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: "isvc", Namespace: "default"},
					Spec: inferencev1alpha1.InferenceServiceSpec{
						Runtime:   "llama",
						ModelRef:  "test-model",
						ExtraArgs: tc.extraArgs,
					},
				}
				args := backend.args(isvc)
				// The inline form means the operator must not add its own bare
				// --metrics; the user's --metrics=true is the only copy.
				if got := countArg(args, "--metrics"); got != tc.want {
					t.Errorf("bare --metrics appeared %d times, want %d; args: %v", got, tc.want, args)
				}
			})
		}
	}
}

func TestAppendSpeculativeDecodingArgs(t *testing.T) {
	n := func(i int32) *int32 { return &i }
	p := func(f float64) *float64 { return &f }

	cases := []struct {
		name      string
		spec      *inferencev1alpha1.SpeculativeDecodingSpec
		draftPath string
		want      []string
	}{
		{name: "nil spec emits nothing", spec: nil, want: nil},
		{
			name: "disabled alias emits nothing",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{Type: "disabled"},
			want: nil,
		},
		{
			name: "none emits nothing",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{Type: "none"},
			want: nil,
		},
		{
			name: "mtp alias resolves to draft-mtp and takes no -md",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{Type: "mtp"},
			want: []string{"--spec-type", "draft-mtp"},
		},
		{
			name: "draft alias resolves to draft-simple and takes -md",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type: "draft", DraftModelRef: "d",
			},
			draftPath: "/models/d/model.gguf",
			want:      []string{"--spec-type", "draft-simple", "-md", "/models/d/model.gguf"},
		},
		{
			name: "literal draft-dspark passes through with every knob",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type: "draft-dspark", DraftModelRef: "d", NDraftMax: n(3), PMin: p(0),
			},
			draftPath: "/models/d/model.gguf",
			want: []string{
				"--spec-type", "draft-dspark",
				"-md", "/models/d/model.gguf",
				"--spec-draft-n-max", "3",
				"--spec-draft-p-min", "0",
			},
		},
		{
			name: "ngram type passes through and takes no -md even if a path is present",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type: "ngram-cache", NDraftMax: n(2),
			},
			draftPath: "/models/stale/model.gguf",
			want:      []string{"--spec-type", "ngram-cache", "--spec-draft-n-max", "2"},
		},
		{
			name:      "mtp alias with no draft takes no -md",
			spec:      &inferencev1alpha1.SpeculativeDecodingSpec{Type: "mtp"},
			draftPath: "",
			want:      []string{"--spec-type", "draft-mtp"},
		},
		{
			name:      "literal draft-mtp with no draft takes no -md",
			spec:      &inferencev1alpha1.SpeculativeDecodingSpec{Type: "draft-mtp"},
			draftPath: "",
			want:      []string{"--spec-type", "draft-mtp"},
		},
		{
			name: "literal draft-mtp with a resolved draft emits -md",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type: "draft-mtp", DraftModelRef: "d",
			},
			draftPath: "/models/d/model.gguf",
			want:      []string{"--spec-type", "draft-mtp", "-md", "/models/d/model.gguf"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendSpeculativeDecodingArgs(nil, tc.spec, tc.draftPath)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("args = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLlamaCppBuildArgs_EmitsDraftModelPath(t *testing.T) {
	n := func(i int32) *int32 { return &i }
	isvc := &inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: "llamacpp",
			SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type: "draft-dspark", DraftModelRef: "dspark", NDraftMax: n(3),
			},
		},
	}
	model := &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Format: "gguf"}}

	got := (&LlamaCppBackend{}).BuildArgs(
		isvc, model, "/models/target/model.gguf", "/models/dspark/model.gguf", 8080)

	joined := strings.Join(got, " ")
	for _, want := range []string{
		"--spec-type draft-dspark",
		"-md /models/dspark/model.gguf",
		"--spec-draft-n-max 3",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got %s", want, joined)
		}
	}
}

// TestLlamaCppBuildArgs_FileBasedDraftModel covers the file-based draft path
// (#1495): spec.speculativeDecoding.draftModel names a companion file in
// Model.files (e.g. a dflash drafter) and the operator resolves its staged
// in-container path the way it does for Model.mmproj, so the manifest never
// names a content-hash cache directory.
func TestLlamaCppBuildArgs_FileBasedDraftModel(t *testing.T) {
	n := func(i int32) *int32 { return &i }
	backend := &LlamaCppBackend{}

	cases := []struct {
		name        string
		modelSpec   inferencev1alpha1.ModelSpec
		modelPath   string
		spec        *inferencev1alpha1.SpeculativeDecodingSpec
		contains    []FlagCheck
		notContains []string
	}{
		{
			name: "draft-dflash companion file emits resolved -md",
			modelSpec: inferencev1alpha1.ModelSpec{
				Format: "gguf",
				Files:  []string{"main.gguf", "dflash-kquant.gguf"},
			},
			modelPath: "/models/main.gguf",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type:       "draft-dflash",
				DraftModel: "dflash-kquant.gguf",
				NDraftMax:  n(3),
			},
			contains: []FlagCheck{
				{"--spec-type", "draft-dflash"},
				{"-md", "/models/dflash-kquant.gguf"},
				{"--spec-draft-n-max", "3"},
			},
		},
		{
			name: "draft file in subdirectory resolves against cache root",
			modelSpec: inferencev1alpha1.ModelSpec{
				Format: "gguf",
				Files:  []string{"subdir/main.gguf", "subdir/dflash.gguf"},
			},
			modelPath: "/models/cache/subdir/main.gguf",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type:       "draft-dflash",
				DraftModel: "subdir/dflash.gguf",
			},
			contains: []FlagCheck{
				{"--spec-type", "draft-dflash"},
				{"-md", "/models/cache/subdir/dflash.gguf"},
			},
		},
		{
			name: "draftModel without files does not emit -md (cannot resolve)",
			modelSpec: inferencev1alpha1.ModelSpec{
				Format: "gguf",
			},
			modelPath: "/models/main.gguf",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type:       "draft-dflash",
				DraftModel: "dflash.gguf",
			},
			contains:    []FlagCheck{{"--spec-type", "draft-dflash"}},
			notContains: []string{"-md"},
		},
		{
			name: "draftModel not emitted for ngram type (needs no weights)",
			modelSpec: inferencev1alpha1.ModelSpec{
				Format: "gguf",
				Files:  []string{"main.gguf", "dflash.gguf"},
			},
			modelPath: "/models/main.gguf",
			spec: &inferencev1alpha1.SpeculativeDecodingSpec{
				Type:       "ngram-cache",
				DraftModel: "dflash.gguf",
				NDraftMax:  n(2),
			},
			contains: []FlagCheck{
				{"--spec-type", "ngram-cache"},
				{"--spec-draft-n-max", "2"},
			},
			notContains: []string{"-md"},
		},
		{
			name: "draftModel not emitted for mtp (self-speculation)",
			modelSpec: inferencev1alpha1.ModelSpec{
				Format: "gguf",
				Files:  []string{"main.gguf"},
			},
			modelPath: "/models/main.gguf",
			spec:      &inferencev1alpha1.SpeculativeDecodingSpec{Type: "mtp"},
			contains:  []FlagCheck{{"--spec-type", "draft-mtp"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "isvc-" + strings.ReplaceAll(tc.name, " ", "-"), Namespace: "default"},
				Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: "llamacpp", SpeculativeDecoding: tc.spec},
			}
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
				Spec:       tc.modelSpec,
			}
			args := backend.BuildArgs(isvc, model, tc.modelPath, "", 8080)
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
		})
	}
}
