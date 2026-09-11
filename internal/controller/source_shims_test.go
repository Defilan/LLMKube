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

import "testing"

// The HF source-resolution family moved to pkg/hfsource in #1759; the
// package-local names are now delegation shims. These pin each shim to one
// representative answer so a wrong delegation (a swapped or misdirected
// wrapper) fails in this package, at the call sites that use it.
func TestHFSourceShimsDelegate(t *testing.T) {
	t.Run("hasSchemeFold", func(t *testing.T) {
		if !hasSchemeFold("HF://org/repo", "hf://") {
			t.Errorf(`hasSchemeFold("HF://org/repo", "hf://") = false, want true`)
		}
		if hasSchemeFold("s3://org/repo", "hf://") {
			t.Errorf(`hasSchemeFold("s3://org/repo", "hf://") = true, want false`)
		}
	})
	t.Run("isHuggingFaceURL", func(t *testing.T) {
		if !isHuggingFaceURL("https://www.huggingface.co/Qwen/Qwen3-8B") {
			t.Errorf(`isHuggingFaceURL("https://www.huggingface.co/Qwen/Qwen3-8B") = false, want true`)
		}
		if isHuggingFaceURL("https://example.com/model.gguf") {
			t.Errorf(`isHuggingFaceURL("https://example.com/model.gguf") = true, want false`)
		}
	})
	t.Run("isHFAuthSource", func(t *testing.T) {
		if !isHFAuthSource("hf://meta-llama/Llama-Guard-4-12B") {
			t.Errorf(`isHFAuthSource("hf://meta-llama/Llama-Guard-4-12B") = false, want true`)
		}
		if isHFAuthSource("https://cdn.example.com/model.gguf") {
			t.Errorf(`isHFAuthSource("https://cdn.example.com/model.gguf") = true, want false`)
		}
	})
	t.Run("isHuggingFaceFileURL", func(t *testing.T) {
		if !isHuggingFaceFileURL("https://huggingface.co/org/repo/blob/v1.0/config.json") {
			t.Errorf(`isHuggingFaceFileURL(".../blob/v1.0/config.json") = false, want true`)
		}
		if isHuggingFaceFileURL("https://huggingface.co/org/repo/tree/main") {
			t.Errorf(`isHuggingFaceFileURL(".../tree/main") = true, want false`)
		}
	})
	t.Run("extractHFRepoFromURL", func(t *testing.T) {
		repoID, revision, ok := extractHFRepoFromURL("https://huggingface.co/Qwen/Qwen3-8B/tree/v1.0")
		if !ok || repoID != "Qwen/Qwen3-8B" || revision != "v1.0" {
			t.Errorf("extractHFRepoFromURL(tree url) = (%q, %q, %v), want (%q, %q, true)",
				repoID, revision, ok, "Qwen/Qwen3-8B", "v1.0")
		}
		if _, _, ok := extractHFRepoFromURL("https://huggingface.co/datasets/org/repo"); ok {
			t.Error(`extractHFRepoFromURL(datasets url) ok = true, want false`)
		}
	})
	t.Run("parseHFSource", func(t *testing.T) {
		repoID, revision, err := parseHFSource("hf://org/repo@v1.0")
		if err != nil || repoID != "org/repo" || revision != "v1.0" {
			t.Errorf("parseHFSource(%q) = (%q, %q, %v), want (%q, %q, nil)",
				"hf://org/repo@v1.0", repoID, revision, err, "org/repo", "v1.0")
		}
		if _, _, err := parseHFSource("hf://"); err == nil {
			t.Error(`parseHFSource("hf://") err = nil, want error`)
		}
	})
	t.Run("normalizeHFSource", func(t *testing.T) {
		if got := normalizeHFSource("hf://org/repo"); got != "https://huggingface.co/org/repo/resolve/main/" {
			t.Errorf("normalizeHFSource(%q) = %q, want %q", "hf://org/repo", got, "https://huggingface.co/org/repo/resolve/main/")
		}
		if got := normalizeHFSource("https://example.com/model.gguf"); got != "https://example.com/model.gguf" {
			t.Errorf("normalizeHFSource(%q) = %q, want it unchanged", "https://example.com/model.gguf", got)
		}
	})
}
