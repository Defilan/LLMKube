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

package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestComputeLength(t *testing.T) {
	got := Compute("https://huggingface.co/model.gguf")
	if len(got) != 16 {
		t.Errorf("Compute length = %d, want 16", len(got))
	}
}

func TestComputeHexChars(t *testing.T) {
	got := Compute("https://huggingface.co/model.gguf")
	for i, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("Compute char at %d = %q, want hex", i, string(c))
		}
	}
}

func TestComputeDeterministic(t *testing.T) {
	a := Compute("https://huggingface.co/model.gguf")
	b := Compute("https://huggingface.co/model.gguf")
	if a != b {
		t.Errorf("Compute not deterministic: %q != %q", a, b)
	}
}

func TestComputeMatchesSHA256Prefix(t *testing.T) {
	source := "https://huggingface.co/TheBloke/Llama-2-7B-GGUF/resolve/main/llama-2-7b.Q4_K_M.gguf"
	want := sha256.Sum256([]byte(source))
	wantStr := hex.EncodeToString(want[:])[:16]
	got := Compute(source)
	if got != wantStr {
		t.Errorf("Compute = %q, want %q", got, wantStr)
	}
}

// The separator and scheme-case variants of one repository must land on one
// cache directory per form, since a duplicate key means a second
// multi-gigabyte download and a cache list that disagrees with what
// delete --purge-cache removes.
func TestComputeSameKeyForRepoSpellings(t *testing.T) {
	for want, spellings := range map[string][]string{
		"hf://org/repo": {"hf://org/repo/", "HF://org/repo", "hf://org//repo"},
		"org/repo":      {"org/repo/", "org//repo"},
	} {
		for _, s := range spellings {
			if got := Compute(s); got != Compute(want) {
				t.Errorf("Compute(%q) = %q, want %q (the key Compute(%q) returns)", s, got, Compute(want), want)
			}
		}
	}
}

// A spelling that worked before canonicalization keeps the digest an older
// release computed for it, so an upgrade neither moves nor re-stages its
// cached files.
func TestComputeKeepsWorkingRepoKeys(t *testing.T) {
	for _, source := range []string{"hf://org/repo", "hf://org/repo@v1.0", "org/repo", "org/repo@v1.0"} {
		want := sha256.Sum256([]byte(source))
		if got := Compute(source); got != hex.EncodeToString(want[:])[:16] {
			t.Errorf("Compute(%q) = %q, want the pre-upgrade digest %q", source, got, hex.EncodeToString(want[:])[:16])
		}
	}
}

// A source that already names a URL to download from is hashed verbatim: its
// current key owns a download on disk, and re-keying it would orphan the file.
func TestComputeKeepsHTTPSourceVerbatim(t *testing.T) {
	for _, source := range []string{
		"https://huggingface.co/org/repo/resolve/main/",
		"https://example.com/models/repo/",
		"s3://bucket/prefix/",
		"/models/local/",
		"pvc://claim/path/",
	} {
		want := sha256.Sum256([]byte(source))
		wantStr := hex.EncodeToString(want[:])[:16]
		if got := Compute(source); got != wantStr {
			t.Errorf("Compute(%q) = %q, want the verbatim digest %q", source, got, wantStr)
		}
	}
}

func TestComputeEmptySource(t *testing.T) {
	got := Compute("")
	if len(got) != 16 {
		t.Errorf("Compute empty length = %d, want 16", len(got))
	}
	// Empty string still produces a valid 16-char hex digest.
	want := sha256.Sum256([]byte(""))
	wantStr := hex.EncodeToString(want[:])[:16]
	if got != wantStr {
		t.Errorf("Compute empty = %q, want %q", got, wantStr)
	}
}

func TestEffectiveKeyNilModel(t *testing.T) {
	if got := EffectiveKey(nil); got != "" {
		t.Errorf("EffectiveKey(nil) = %q, want empty", got)
	}
}

func TestEffectiveKeyStatusCacheKeyWins(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://huggingface.co/example/model.gguf",
		},
		Status: inferencev1alpha1.ModelStatus{
			CacheKey: "status-wins",
		},
	}
	got := EffectiveKey(model)
	if got != "status-wins" {
		t.Errorf("EffectiveKey with Status.CacheKey set = %q, want status-wins", got)
	}
}

func TestEffectiveKeyMultiFileNonMetal(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://example/model",
			Files:  []string{"model.gguf", "mmproj.gguf"},
		},
	}
	want := Compute("hf://example/model")
	got := EffectiveKey(model)
	if got != want {
		t.Errorf("EffectiveKey multi-file non-metal = %q, want %q", got, want)
	}
}

// The effective key of a multi-file Model is its repository's key, so the
// trailing-slash spelling a user copies from the Hub directory listing points
// at the same cached files as the plain spelling.
func TestEffectiveKeyTrailingSlashRepo(t *testing.T) {
	plain := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{Source: "hf://example/model", Files: []string{"model.gguf"}},
	}
	slashed := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{Source: "hf://example/model/", Files: []string{"model.gguf"}},
	}
	if a, b := EffectiveKey(plain), EffectiveKey(slashed); a != b {
		t.Errorf("EffectiveKey differs by spelling: %q != %q", a, b)
	}
}

func TestEffectiveKeyMetalMultiFile(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://example/model",
			Files:  []string{"model.gguf"},
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "metal",
			},
		},
	}
	got := EffectiveKey(model)
	if got != "" {
		t.Errorf("EffectiveKey metal multi-file = %q, want empty", got)
	}
}

func TestEffectiveKeySingleFile(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://huggingface.co/example/model.gguf",
		},
	}
	got := EffectiveKey(model)
	if got != "" {
		t.Errorf("EffectiveKey single-file = %q, want empty", got)
	}
}
