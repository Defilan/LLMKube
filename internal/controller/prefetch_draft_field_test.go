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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestPrefetchDraftRefusalNamesTheField covers the prefetch gate's new input:
// a vLLM draft Model cannot be covered by a prefetch either (its downloader
// runs on the serving node), and the refusal has to name the field the user
// actually set. The llama.cpp message is asserted verbatim because it is the
// pre-existing behavior for that input class and must not drift.
func TestPrefetchDraftRefusalNamesTheField(t *testing.T) {
	// A cache key makes the Model otherwise prefetchable, so the draft branch
	// is the one that decides.
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/target.gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: "target-key"},
	}

	cases := []struct {
		name    string
		spec    inferencev1alpha1.InferenceServiceSpec
		want    prefetchApplicability
		wantMsg string
	}{
		{
			name: "llama.cpp draftModelRef keeps its message",
			spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "llamacpp",
				SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
					Type: "draft-dspark", DraftModelRef: "draft-cr",
				},
			},
			want:    prefetchUnsupported,
			wantMsg: `speculativeDecoding.draftModelRef "draft-cr" is not prefetched and its downloader would run on the serving node`,
		},
		{
			name: "vllm draft names vllmConfig.speculative.model",
			spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "vllm",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled: ptrBool(true),
						Model:   "draft-cr",
					},
				},
			},
			want:    prefetchUnsupported,
			wantMsg: `spec.vllmConfig.speculative.model "draft-cr" is not prefetched and its downloader would run on the serving node`,
		},
		{
			name: "vllm method-only speculative needs no draft and still prefetches",
			spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "vllm",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled: ptrBool(true),
						Method:  "dspark",
					},
				},
			},
			want: prefetchApplies,
		},
		{
			name: "a disabled vllm speculative block needs no draft",
			spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "vllm",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					Speculative: &inferencev1alpha1.SpeculativeConfig{
						Enabled: ptrBool(false),
						Model:   "draft-cr",
					},
				},
			},
			want: prefetchApplies,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				Spec:       tc.spec,
			}
			isvc.Spec.ModelRef = model.Name
			got, msg := classifyPrefetchRequest(isvc, model, "/models")
			if got != tc.want {
				t.Fatalf("applicability: got %v want %v (message %q)", got, tc.want, msg)
			}
			if tc.wantMsg == "" {
				return
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("message: got %q want it to contain %q", msg, tc.wantMsg)
			}
		})
	}
}
