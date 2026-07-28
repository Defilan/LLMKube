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
	"strings"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestHFURLSourceClassification guards #1322: a pasted huggingface.co URL must
// be classified so that a landing/tree/repo URL is a runtime-resolved HF repo
// while a URL that names a specific FILE stays a single-file HTTP download (the
// regression the first fix introduced by collapsing resolve/blob file URLs to
// repos). Genuine non-HF sources are unchanged.
func TestHFURLSourceClassification(t *testing.T) {
	cases := []struct {
		name           string
		source         string
		wantHFRepo     bool // isHFRepoSource
		wantRemoteHTTP bool // isRemoteHTTPSource
	}{
		{"landing page", "https://huggingface.co/Qwen/Qwen3.6-35B-A3B", true, false},
		{"tree with rev", "https://huggingface.co/Qwen/Qwen3.6-35B-A3B/tree/main", true, false},
		{"resolve rev root (no file)", "https://huggingface.co/Qwen/Qwen3.6-35B-A3B/resolve/main", true, false},
		// The regression case: a resolve URL naming a file is a single-file
		// download, NOT a repo.
		{"resolve file url", "https://huggingface.co/TheBloke/Model-GGUF/resolve/main/model.Q4_K_M.gguf", false, true},
		{"blob file url", "https://huggingface.co/TheBloke/Model-GGUF/blob/main/model.Q4_K_M.gguf", false, true},
		// Genuine non-HF remote files keep their classification (no regression).
		{"direct gguf url", "https://example.com/model.gguf", false, true},
		{"s3 source", "s3://bucket/model.gguf", false, false},
		// Case-insensitive host.
		{"capitalized host", "https://HuggingFace.co/Qwen/Qwen3.6-35B-A3B", true, false},
		// Query/fragment must not break classification.
		{"query string", "https://huggingface.co/Qwen/Qwen3.6-35B-A3B?library=vllm", true, false},
		// datasets/spaces are not model repos.
		{"datasets", "https://huggingface.co/datasets/foo/bar", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHFRepoSource(tc.source); got != tc.wantHFRepo {
				t.Errorf("isHFRepoSource(%q) = %v, want %v", tc.source, got, tc.wantHFRepo)
			}
			if got := isRemoteHTTPSource(tc.source); got != tc.wantRemoteHTTP {
				t.Errorf("isRemoteHTTPSource(%q) = %v, want %v", tc.source, got, tc.wantRemoteHTTP)
			}
		})
	}
}

// TestHFServeArg guards the serve-path half of #1322: the runtime must receive
// the bare "org/name[@rev]" repo id, never a resolve URL. The URL form must
// resolve to the same argument the bare form yields.
func TestHFServeArg(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"Qwen/Qwen3.6-35B-A3B", "Qwen/Qwen3.6-35B-A3B"},
		{"Qwen/Qwen3.6-35B-A3B@abc123", "Qwen/Qwen3.6-35B-A3B@abc123"},
		{"hf://Qwen/Qwen3.6-35B-A3B", "Qwen/Qwen3.6-35B-A3B"},
		{"https://huggingface.co/Qwen/Qwen3.6-35B-A3B", "Qwen/Qwen3.6-35B-A3B"},
		{"https://huggingface.co/Qwen/Qwen3.6-35B-A3B/tree/abc123", "Qwen/Qwen3.6-35B-A3B@abc123"},
		{"https://HuggingFace.co/Qwen/Qwen3.6-35B-A3B?library=vllm", "Qwen/Qwen3.6-35B-A3B"},
		{"s3://bucket/model.gguf", "s3://bucket/model.gguf"}, // non-HF passthrough
	}
	for _, tc := range cases {
		got := hfServeArg(tc.source)
		if got != tc.want {
			t.Errorf("hfServeArg(%q) = %q, want %q", tc.source, got, tc.want)
		}
		if strings.Contains(got, "resolve/") || strings.HasPrefix(got, "https://huggingface.co/") {
			t.Errorf("hfServeArg(%q) returned a URL %q; runtimes reject that", tc.source, got)
		}
	}
}

// TestVLLMBuildArgsHFURLServesBareRepoID is the end-to-end assertion the review
// asked for: what does the runtime actually receive? For a landing-URL source
// with nothing cached (modelPath==""), vLLM's model argument must be the bare
// repo id, identical to the bare form, not a resolve URL.
func TestVLLMBuildArgsHFURLServesBareRepoID(t *testing.T) {
	bare := &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Source: "Qwen/Qwen3.6-35B-A3B"}}
	url := &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Source: "https://huggingface.co/Qwen/Qwen3.6-35B-A3B"}}
	isvc := &inferencev1alpha1.InferenceService{}
	b := &VLLMBackend{}

	bareArgs := b.BuildArgs(isvc, bare, "", 8000)
	urlArgs := b.BuildArgs(isvc, url, "", 8000)
	if len(bareArgs) == 0 || len(urlArgs) == 0 {
		t.Fatalf("BuildArgs returned no args (bare=%v url=%v)", bareArgs, urlArgs)
	}
	if urlArgs[0] != "Qwen/Qwen3.6-35B-A3B" {
		t.Errorf("vLLM model arg for the URL form = %q, want the bare repo id %q", urlArgs[0], "Qwen/Qwen3.6-35B-A3B")
	}
	if urlArgs[0] != bareArgs[0] {
		t.Errorf("URL form (%q) and bare form (%q) must serve the same model arg", urlArgs[0], bareArgs[0])
	}
	if strings.Contains(urlArgs[0], "resolve/") {
		t.Errorf("vLLM would receive a resolve URL %q and crashloop", urlArgs[0])
	}
}

// TestPrefetchEligibleHFForms confirms the classifier change did not silently
// flip hf:// (and huggingface.co repo URL) prefetch eligibility.
func TestPrefetchEligibleHFForms(t *testing.T) {
	cases := []struct {
		source string
		want   bool
	}{
		{"hf://Qwen/Qwen3.6-35B-A3B", true},
		{"https://huggingface.co/Qwen/Qwen3.6-35B-A3B", true},
		{"https://example.com/model.gguf", true},
		{"Qwen/Qwen3.6-35B-A3B", false}, // bare repo id is runtime-resolved, not prefetched
		{"pvc://claim/path", false},
		{"s3://bucket/model.gguf", false},
	}
	for _, tc := range cases {
		m := &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{Source: tc.source, Prefetch: true}}
		if got := prefetchEligible(m); got != tc.want {
			t.Errorf("prefetchEligible(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}
