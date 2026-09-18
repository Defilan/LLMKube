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

package hfsource

import (
	"strings"
	"testing"
)

func TestNormalizeHFSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"hf scheme", "hf://org/repo", "https://huggingface.co/org/repo/resolve/main/"},
		{"hf scheme with rev", "hf://org/repo@v1.0", "https://huggingface.co/org/repo/resolve/v1.0/"},
		{"bare repo id passes through", "org/repo", "org/repo"},
		{"non-hf url passes through", "https://example.com/model.gguf", "https://example.com/model.gguf"},
		{"hf landing page", "https://huggingface.co/org/repo", "https://huggingface.co/org/repo/resolve/main/"},
		{"hf landing page with slash", "https://huggingface.co/org/repo/", "https://huggingface.co/org/repo/resolve/main/"},
		{"hf tree url", "https://huggingface.co/org/repo/tree/main", "https://huggingface.co/org/repo/resolve/main/"},
		{"hf blob file url passes through", "https://huggingface.co/org/repo/blob/v1.0/config.json",
			"https://huggingface.co/org/repo/blob/v1.0/config.json"},
		{"hf resolve file url passes through", "https://huggingface.co/org/repo/resolve/main/model.gguf",
			"https://huggingface.co/org/repo/resolve/main/model.gguf"},
		{"http host folded to https", "http://huggingface.co/org/repo", "https://huggingface.co/org/repo/resolve/main/"},
		{"parse error passes through", "hf://", "hf://"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeHFSource(tc.source); got != tc.want {
				t.Errorf("NormalizeHFSource(%q) = %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

func TestIsHFAuthSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   bool
	}{
		{"hf scheme", "hf://meta-llama/Llama-Guard-4-12B", true},
		{"hf scheme upper", "HF://meta-llama/Llama-Guard-4-12B", true},
		{"resolved https url", "https://huggingface.co/org/repo/resolve/main/model.gguf", true},
		{"host case folded", "https://HuggingFace.CO/org/repo/resolve/main/model.gguf", true},
		{"www prefix", "https://www.huggingface.co/org/repo/resolve/main/model.gguf", true},
		{"www prefix upper", "https://WWW.HuggingFace.co/org/repo/resolve/main/model.gguf", true},
		{"lookalike host", "https://huggingface.co.evil.example/org/repo/model.gguf", false},
		{"substring host", "https://nothuggingface.co/org/repo/model.gguf", false},
		{"other host", "https://cdn.example.com/model.gguf", false},
		{"s3 source", "s3://models/org/repo/model.gguf", false},
		{"local source", "/host-model/model.gguf", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHFAuthSource(tc.source); got != tc.want {
				t.Errorf("IsHFAuthSource(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

func TestIsHuggingFaceURL(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   bool
	}{
		{"https url", "https://huggingface.co/Qwen/Qwen3-8B", true},
		{"http url", "http://huggingface.co/Qwen/Qwen3-8B", true},
		{"www prefix", "https://www.huggingface.co/Qwen/Qwen3-8B", true},
		{"scheme upper", "HTTPS://huggingface.co/Qwen/Qwen3-8B", true},
		{"other host", "https://example.com/model.gguf", false},
		{"bare repo id", "Qwen/Qwen3-8B", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHuggingFaceURL(tc.source); got != tc.want {
				t.Errorf("IsHuggingFaceURL(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

func TestParseHFSource(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		wantRepo string
		wantRev  string
		wantErr  bool
	}{
		{"hf scheme", "hf://org/repo", "org/repo", "", false},
		{"hf scheme with rev", "hf://org/repo@main", "org/repo", "main", false},
		{"bare form", "org/repo@v1.0", "org/repo", "v1.0", false},
		{"https url", "https://huggingface.co/Qwen/Qwen3-8B", "Qwen/Qwen3-8B", "", false},
		{"https url with tree rev", "https://huggingface.co/Qwen/Qwen3-8B/tree/main", "Qwen/Qwen3-8B", "main", false},
		{"file url rejected", "https://huggingface.co/org/repo/resolve/main/model.gguf", "", "", true},
		{"empty scheme", "hf://", "", "", true},
		{"empty rev", "hf://org/repo@", "", "", true},
		{"rev whitespace", "hf://org/repo@main branch", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoID, revision, err := ParseHFSource(tc.source)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseHFSource(%q) = (%q, %q, nil), want error", tc.source, repoID, revision)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHFSource(%q): %v", tc.source, err)
			}
			if repoID != tc.wantRepo || revision != tc.wantRev {
				t.Errorf("ParseHFSource(%q) = (%q, %q), want (%q, %q)", tc.source, repoID, revision, tc.wantRepo, tc.wantRev)
			}
		})
	}
}

func TestExtractHFRepoFromURL(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		wantRepo string
		wantRev  string
		wantOK   bool
	}{
		{"landing page", "https://huggingface.co/Qwen/Qwen3-8B", "Qwen/Qwen3-8B", "", true},
		{"trailing slash", "https://huggingface.co/Qwen/Qwen3-8B/", "Qwen/Qwen3-8B", "", true},
		{"tree rev", "https://huggingface.co/Qwen/Qwen3-8B/tree/main", "Qwen/Qwen3-8B", "main", true},
		{"resolve root rev", "https://huggingface.co/Qwen/Qwen3-8B/resolve/v1.0", "Qwen/Qwen3-8B", "v1.0", true},
		{"file url not a repo", "https://huggingface.co/Qwen/Qwen3-8B/resolve/main/model.gguf", "", "", false},
		{"datasets not a repo", "https://huggingface.co/datasets/org/repo", "", "", false},
		{"other host", "https://example.com/model.gguf", "", "", false},
		{"host only", "https://huggingface.co/Qwen", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoID, revision, ok := ExtractHFRepoFromURL(tc.source)
			if ok != tc.wantOK {
				t.Fatalf("ExtractHFRepoFromURL(%q) ok = %v, want %v", tc.source, ok, tc.wantOK)
			}
			if repoID != tc.wantRepo || revision != tc.wantRev {
				t.Errorf("ExtractHFRepoFromURL(%q) = (%q, %q), want (%q, %q)", tc.source, repoID, revision, tc.wantRepo, tc.wantRev)
			}
		})
	}
}

// IsHuggingFaceURL and HFURLPathSegments must agree on every input, because
// each HF branch gates on the first and calls through to the second. When
// they disagreed, an uppercase host was an HF URL to one and nothing to the
// other, leaving a source no classifier branch handled.
func TestIsHuggingFaceURLAgreesWithSegments(t *testing.T) {
	sources := []string{
		"https://huggingface.co/Qwen/Qwen3-8B/resolve/main/model.gguf",
		"HTTP://WWW.huggingface.co/org/repo",
		"https://huggingface.co.evil.example/x/y",
		"https://example.com/model.gguf",
		"hf://org/repo",
		"",
	}
	for _, src := range sources {
		isURL := IsHuggingFaceURL(src)
		_, ok := HFURLPathSegments(src)
		if isURL != ok {
			t.Errorf("%q: IsHuggingFaceURL=%v but HFURLPathSegments ok=%v; the two must agree", src, isURL, ok)
		}
	}
}

func TestHasSchemeFold(t *testing.T) {
	tests := []struct {
		name   string
		source string
		prefix string
		want   bool
	}{
		{"exact prefix", "hf://org/repo", "hf://", true},
		{"scheme upper", "HF://org/repo", "hf://", true},
		{"mixed case scheme", "Hf://org/repo", "hf://", true},
		{"wrong scheme", "s3://org/repo", "hf://", false},
		{"shorter than prefix", "hf:/", "hf://", false},
		{"empty source", "", "hf://", false},
		{"https upper", "HTTPS://huggingface.co/x", "https://", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasSchemeFold(tc.source, tc.prefix); got != tc.want {
				t.Errorf("HasSchemeFold(%q, %q) = %v, want %v", tc.source, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestIsHuggingFaceFileURL(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   bool
	}{
		{"resolve file", "https://huggingface.co/org/repo/resolve/main/model.gguf", true},
		{"blob file", "https://huggingface.co/org/repo/blob/v1.0/config.json", true},
		{"tree is not a file", "https://huggingface.co/org/repo/tree/main", false},
		{"landing page", "https://huggingface.co/org/repo", false},
		{"resolve root", "https://huggingface.co/org/repo/resolve/main", false},
		{"file before index 2", "https://huggingface.co/org/resolve/main/m.gguf", false},
		{"other host", "https://example.com/org/repo/resolve/main/model.gguf", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHuggingFaceFileURL(tc.source); got != tc.want {
				t.Errorf("IsHuggingFaceFileURL(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

// The full chain, end to end: whatever spelling an operator writes in
// Model.spec.source must normalize to a URL whose path is a resolve path
// (or pass through untouched for a single-file download).
func TestNormalizeProducesResolveURL(t *testing.T) {
	for _, src := range []string{"hf://org/repo", "hf://org/repo@v2", "https://huggingface.co/org/repo/tree/main"} {
		got := NormalizeHFSource(src)
		if !strings.HasPrefix(got, "https://huggingface.co/") || !strings.Contains(got, "/resolve/") {
			t.Errorf("NormalizeHFSource(%q) = %q, want an https://huggingface.co resolve URL", src, got)
		}
	}
}
