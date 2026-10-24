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
	"encoding/json"
	"os"
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// renderedSpeculativeConfig returns the JSON document appendSpeculativeConfig
// emitted, decoded, so a case asserts on keys rather than on key order.
func renderedSpeculativeConfig(t *testing.T, args []string) (map[string]interface{}, bool) {
	t.Helper()
	for i, a := range args {
		var raw string
		switch {
		case a == "--speculative-config" && i+1 < len(args):
			raw = args[i+1]
		case strings.HasPrefix(a, "--speculative-config="):
			raw = strings.TrimPrefix(a, "--speculative-config=")
		default:
			continue
		}
		out := map[string]interface{}{}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("rendered JSON is not an object: %s (%v)", raw, err)
		}
		return out, true
	}
	return nil, false
}

func TestAppendSpeculativeConfig(t *testing.T) {
	cases := []struct {
		name      string
		cfg       *inferencev1alpha1.SpeculativeConfig
		draftPath string
		extraArgs []string
		// want is the exact decoded document; nil means "emit nothing".
		want    map[string]interface{}
		wantErr bool
	}{
		{
			name: "disabled emits nothing even with a draft",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled: ptrBool(false),
				Method:  "dspark",
			},
			draftPath: "/models/draft",
		},
		{
			name: "nil config emits nothing",
		},
		{
			name: "method only (drafter in the checkpoint) emits method and tokens",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled:              ptrBool(true),
				Method:               "dspark",
				NumSpeculativeTokens: ptrInt32(5),
			},
			want: map[string]interface{}{
				"method":                 "dspark",
				"num_speculative_tokens": float64(5),
			},
		},
		{
			name: "method without tokens renders the CRD default",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled: ptrBool(true),
				Method:  "mtp",
			},
			want: map[string]interface{}{
				"method":                 "mtp",
				"num_speculative_tokens": float64(4),
			},
		},
		{
			name: "draft model emits the staged path and infers draft_model",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled:              ptrBool(true),
				Model:                "my-draft-cr",
				NumSpeculativeTokens: ptrInt32(3),
			},
			draftPath: "/models/abc123",
			want: map[string]interface{}{
				"method":                 "draft_model",
				"model":                  "/models/abc123",
				"num_speculative_tokens": float64(3),
			},
		},
		{
			name: "draft model without tokens renders the CRD default",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled: ptrBool(true),
				Model:   "my-draft-cr",
			},
			draftPath: "/models/abc123",
			want: map[string]interface{}{
				"method":                 "draft_model",
				"model":                  "/models/abc123",
				"num_speculative_tokens": float64(4),
			},
		},
		{
			name: "explicit method is kept alongside a draft path",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled:              ptrBool(true),
				Method:               "eagle3",
				Model:                "my-draft-cr",
				NumSpeculativeTokens: ptrInt32(3),
			},
			draftPath: "/models/abc123",
			want: map[string]interface{}{
				"method":                 "eagle3",
				"model":                  "/models/abc123",
				"num_speculative_tokens": float64(3),
			},
		},
		{
			name: "an unknown method passes through, because the registry is per image",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled:              ptrBool(true),
				Method:               "some_future_method",
				NumSpeculativeTokens: ptrInt32(2),
			},
			want: map[string]interface{}{
				"method":                 "some_future_method",
				"num_speculative_tokens": float64(2),
			},
		},
		{
			name: "a method that needs no draft ignores a path the resolver picked",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled:              ptrBool(true),
				Method:               "mtp",
				NumSpeculativeTokens: ptrInt32(2),
			},
			draftPath: "/models/stray",
			want: map[string]interface{}{
				"method":                 "mtp",
				"num_speculative_tokens": float64(2),
			},
		},
		{
			name: "extraArgs --speculative-config wins",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled:              ptrBool(true),
				Method:               "mtp",
				NumSpeculativeTokens: ptrInt32(2),
			},
			extraArgs: []string{"--speculative-config", `{"method":"dspark"}`},
		},
		{
			name: "extraArgs inline --speculative-config=... wins",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled: ptrBool(true),
				Method:  "mtp",
			},
			extraArgs: []string{`--speculative-config={"method":"dspark"}`},
		},
		{
			name: "a draft that nothing staged is an error, not an empty model",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled: ptrBool(true),
				Model:   "my-draft-cr",
			},
			wantErr: true,
		},
		{
			name: "neither model nor method is an error",
			cfg: &inferencev1alpha1.SpeculativeConfig{
				Enabled:              ptrBool(true),
				NumSpeculativeTokens: ptrInt32(4),
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := appendSpeculativeConfig(nil, tc.cfg, tc.draftPath, tc.extraArgs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got args %v", got)
				}
				if len(got) != 0 {
					t.Fatalf("a rejected config must emit no args, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			doc, emitted := renderedSpeculativeConfig(t, got)
			if tc.want == nil {
				if emitted {
					t.Fatalf("expected no --speculative-config, got %v", got)
				}
				return
			}
			if !emitted {
				t.Fatalf("expected --speculative-config, got %v", got)
			}
			if len(doc) != len(tc.want) {
				t.Fatalf("rendered %v, want %v", doc, tc.want)
			}
			for k, v := range tc.want {
				if doc[k] != v {
					t.Fatalf("key %q: got %v, want %v (document %v)", k, doc[k], v, doc)
				}
			}
		})
	}
}

// TestAppendSpeculativeConfigEmitsNoLegacyFlags pins the regression the issue
// reported: --speculative-model and --num-speculative-tokens are not flags in
// any vLLM release, and passing them made the container exit at startup.
func TestAppendSpeculativeConfigEmitsNoLegacyFlags(t *testing.T) {
	args, err := appendSpeculativeConfig(nil,
		&inferencev1alpha1.SpeculativeConfig{
			Enabled:              ptrBool(true),
			Method:               "draft_model",
			Model:                "my-draft-cr",
			NumSpeculativeTokens: ptrInt32(4),
		},
		"/models/abc123", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, legacy := range []string{"--speculative-model", "--num-speculative-tokens", "--spec-method", "--spec-model", "--spec-tokens"} {
		for _, a := range args {
			if a == legacy || strings.HasPrefix(a, legacy+"=") {
				t.Fatalf("%s is not a vLLM flag and must never be emitted, got %v", legacy, args)
			}
		}
	}
}

// TestDraftWeightsRefForPrefersTheVLLMBlock covers a service that carries both
// speculative blocks, which is what a llama.cpp service looks like after it is
// switched to vLLM. Only the vLLM block's path reaches the vLLM argv, so
// resolving the llama block there would mount the wrong weights and hand vLLM
// their path. The llama block still resolves when no enabled vLLM block names a
// model, so a switched-over service that never set one keeps its old behavior.
func TestDraftWeightsRefForPrefersTheVLLMBlock(t *testing.T) {
	both := func(runtime string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: runtime,
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type: "draft-dspark", DraftModelRef: "llama-draft",
				},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled: ptrBool(true),
						Model:   "vllm-draft",
					},
				},
			},
		}
	}

	cases := []struct {
		name      string
		isvc      *inferencev1alpha1.InferenceService
		wantRef   string
		wantField string
	}{
		{
			name:      "both blocks on a vllm service resolves the vllm field",
			isvc:      both("vllm"),
			wantRef:   "vllm-draft",
			wantField: "spec.vllmConfig.speculative.model",
		},
		{
			name:      "both blocks on a llama.cpp service resolves the llama field",
			isvc:      both("llamacpp"),
			wantRef:   "llama-draft",
			wantField: "spec.speculativeDecoding.draftModelRef",
		},
		{
			name: "a disabled vllm block leaves the llama field",
			isvc: func() *inferencev1alpha1.InferenceService {
				i := both("vllm")
				i.Spec.VLLMConfig.Speculative.Enabled = ptrBool(false)
				return i
			}(),
			wantRef:   "llama-draft",
			wantField: "spec.speculativeDecoding.draftModelRef",
		},
		{
			name: "a method-only vllm block leaves the llama field",
			isvc: func() *inferencev1alpha1.InferenceService {
				i := both("vllm")
				i.Spec.VLLMConfig.Speculative.Model = ""
				i.Spec.VLLMConfig.Speculative.Method = "mtp"
				return i
			}(),
			wantRef:   "llama-draft",
			wantField: "spec.speculativeDecoding.draftModelRef",
		},
		{
			name:      "nil service resolves nothing",
			isvc:      nil,
			wantRef:   "",
			wantField: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, field := draftWeightsRefFor(tc.isvc)
			if ref != tc.wantRef || field != tc.wantField {
				t.Fatalf("got (%q, %q), want (%q, %q)", ref, field, tc.wantRef, tc.wantField)
			}
		})
	}
}

// TestMethodDocClaimsMatchThePinnedRelease guards the field doc that ships into
// the CRD and `kubectl explain`. A claim that a method is newer than the pinned
// image is a fact about the engine, and the first draft of this doc got it
// backwards; the doc is checked against the same oracle the renderer is.
func TestMethodDocClaimsMatchThePinnedRelease(t *testing.T) {
	crds := []string{
		"../../config/crd/bases/inference.llmkube.dev_inferenceservices.yaml",
		"../../charts/llmkube/templates/crds/inferenceservices.yaml",
	}
	for _, path := range crds {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "arrived after") || strings.Contains(line, "does not yet support") {
				t.Errorf("%s states when vLLM added a method; the pinned release is the only source for that and "+
					"the claim is easy to get wrong:\n  %s", path, strings.TrimSpace(line))
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		_ = f.Close()
	}
}

// TestRenderedSpeculativeConfigUsesPinnedVLLMKeys asserts the rendered keys
// against the field list the pinned image's own parser accepts, not against a
// list this test wrote: vLLM builds SpeculativeConfig from the JSON with
// extra="forbid", so an unknown key is a container that exits on startup.
// See testdata/vllm-speculative-config-fields-v0.25.1.txt for provenance.
func TestRenderedSpeculativeConfigUsesPinnedVLLMKeys(t *testing.T) {
	f, err := os.Open("testdata/vllm-speculative-config-fields-v0.25.1.txt")
	if err != nil {
		t.Fatalf("open oracle: %v", err)
	}
	defer func() { _ = f.Close() }()

	accepted := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		accepted[line] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read oracle: %v", err)
	}

	// Guard the oracle itself: if the extraction ever produced nothing, every
	// key below would look unknown and the test would fail for the wrong
	// reason, which reads as a code bug rather than a fixture bug.
	for _, must := range []string{"method", "model", "num_speculative_tokens"} {
		if !accepted[must] {
			t.Fatalf("oracle fixture is missing %q; it is not a valid extraction of vllm/config/speculative.py", must)
		}
	}
	if len(accepted) < 20 {
		t.Fatalf("oracle fixture has only %d keys; expected the full SpeculativeConfig field list", len(accepted))
	}

	// Every shape the operator can render, one per branch of
	// appendSpeculativeConfig.
	shapes := []*inferencev1alpha1.SpeculativeConfig{
		{Enabled: ptrBool(true), Method: "mtp"},
		{Enabled: ptrBool(true), Method: "dspark", NumSpeculativeTokens: ptrInt32(5)},
		{Enabled: ptrBool(true), Model: "draft", NumSpeculativeTokens: ptrInt32(3)},
		{Enabled: ptrBool(true), Method: "eagle3", Model: "draft"},
	}
	for i, cfg := range shapes {
		args, err := appendSpeculativeConfig(nil, cfg, "/models/x", nil)
		if err != nil {
			t.Fatalf("shape %d: unexpected error: %v", i, err)
		}
		doc, emitted := renderedSpeculativeConfig(t, args)
		if !emitted {
			t.Fatalf("shape %d: no --speculative-config in %v", i, args)
		}
		for key := range doc {
			if !accepted[key] {
				t.Errorf("shape %d renders key %q, which the pinned vLLM (v0.25.1) SpeculativeConfig does not accept; "+
					"its parser forbids unknown keys, so the container would exit at startup", i, key)
			}
		}
	}
}
