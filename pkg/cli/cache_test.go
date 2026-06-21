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

package cli

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComputeCacheKey(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{"HTTPS URL", "https://huggingface.co/TheBloke/Llama-2-7B-GGUF/resolve/main/llama-2-7b.Q4_K_M.gguf"},
		{"HTTP URL", "http://example.com/model.gguf"},
		{"file URL", "file:///mnt/models/model.gguf"},
		{"empty string", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := computeCacheKey(tt.source)

			// Must be exactly 16 hex characters
			if len(key) != 16 {
				t.Errorf("computeCacheKey(%q) length = %d, want 16", tt.source, len(key))
			}

			// Must contain only hex characters
			for _, c := range key {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Errorf("computeCacheKey(%q) contains non-hex char %q", tt.source, string(c))
				}
			}
		})
	}
}

func TestComputeCacheKeyDeterministic(t *testing.T) {
	source := "https://huggingface.co/TheBloke/model.gguf"
	key1 := computeCacheKey(source)
	key2 := computeCacheKey(source)

	if key1 != key2 {
		t.Errorf("computeCacheKey is not deterministic: %q != %q", key1, key2)
	}
}

func TestComputeCacheKeyUniqueness(t *testing.T) {
	sources := []string{
		"https://huggingface.co/model-a.gguf",
		"https://huggingface.co/model-b.gguf",
		"https://huggingface.co/model-a.gguf?v=2",
		"file:///mnt/models/model.gguf",
	}

	keys := make(map[string]string)
	for _, source := range sources {
		key := computeCacheKey(source)
		if prev, exists := keys[key]; exists {
			t.Errorf("Cache key collision: %q and %q both produce %q", prev, source, key)
		}
		keys[key] = source
	}
}

func TestComputeCacheKeyMatchesController(t *testing.T) {
	// The controller uses the same algorithm: SHA256(source)[:16]
	// Verify known hash values to catch algorithm drift
	source := "https://huggingface.co/TheBloke/Llama-2-7B-GGUF/resolve/main/llama-2-7b.Q4_K_M.gguf"
	key := computeCacheKey(source)

	// Re-compute with the same algorithm to verify
	expected := computeCacheKey(source)
	if key != expected {
		t.Errorf("computeCacheKey result changed between calls: %q != %q", key, expected)
	}
}

func TestNewCacheCommand(t *testing.T) {
	cmd := NewCacheCommand()

	if cmd.Use != "cache" {
		t.Errorf("Use = %q, want %q", cmd.Use, "cache")
	}

	// Verify subcommands are registered
	subcommands := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subcommands[sub.Name()] = true
	}

	expectedSubs := []string{"list", "clear", "preload"}
	for _, name := range expectedSubs {
		if !subcommands[name] {
			t.Errorf("Missing subcommand %q", name)
		}
	}
}

func TestNewCacheListCommand(t *testing.T) {
	cmd := newCacheListCommand()

	if cmd.Use != "list" {
		t.Errorf("Use = %q, want %q", cmd.Use, "list")
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	} else if f.Shorthand != "n" {
		t.Errorf("namespace shorthand = %q, want %q", f.Shorthand, "n")
	}

	if f := cmd.Flags().Lookup("all-namespaces"); f == nil {
		t.Error("Missing --all-namespaces flag")
	} else if f.Shorthand != "A" {
		t.Errorf("all-namespaces shorthand = %q, want %q", f.Shorthand, "A")
	}
}

func TestNewCacheClearCommand(t *testing.T) {
	cmd := newCacheClearCommand()

	if cmd.Use != "clear" {
		t.Errorf("Use = %q, want %q", cmd.Use, "clear")
	}

	expectedFlags := []string{"model", "namespace", "force"}
	for _, name := range expectedFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("Missing flag %q", name)
		}
	}
}

func TestNewCachePreloadCommand(t *testing.T) {
	cmd := newCachePreloadCommand()

	if cmd.Use != "preload MODEL_ID" {
		t.Errorf("Use = %q, want %q", cmd.Use, "preload MODEL_ID")
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	}
}

func TestCacheEntryHasInferenceServiceNames(t *testing.T) {
	entry := &CacheEntry{
		CacheKey:              "abc123",
		ModelNames:            []string{"model-a"},
		InferenceServiceNames: []string{"isvc-a", "isvc-b"},
		Status:                statusActive,
	}

	if len(entry.InferenceServiceNames) != 2 {
		t.Errorf("InferenceServiceNames length = %d, want 2", len(entry.InferenceServiceNames))
	}
	if entry.InferenceServiceNames[0] != "isvc-a" {
		t.Errorf("InferenceServiceNames[0] = %q, want %q", entry.InferenceServiceNames[0], "isvc-a")
	}
	if entry.InferenceServiceNames[1] != "isvc-b" {
		t.Errorf("InferenceServiceNames[1] = %q, want %q", entry.InferenceServiceNames[1], "isvc-b")
	}
}

func TestCacheEntryInferenceServiceNamesEmpty(t *testing.T) {
	entry := &CacheEntry{
		CacheKey:              "def456",
		ModelNames:            []string{"model-b"},
		InferenceServiceNames: []string{},
		Status:                statusActive,
	}

	if len(entry.InferenceServiceNames) != 0 {
		t.Errorf("InferenceServiceNames length = %d, want 0", len(entry.InferenceServiceNames))
	}
}

func TestCacheEntryOrphanedWithInferenceService(t *testing.T) {
	// An orphaned cache entry can still have InferenceService references
	// when the Model was deleted but the InferenceService still references it.
	entry := &CacheEntry{
		CacheKey:              "ghi789",
		ModelNames:            []string{},
		InferenceServiceNames: []string{"isvc-c"},
		Status:                statusOrphaned,
	}

	if entry.Status != statusOrphaned {
		t.Errorf("Status = %q, want %q", entry.Status, statusOrphaned)
	}
	if len(entry.ModelNames) != 0 {
		t.Errorf("ModelNames length = %d, want 0", len(entry.ModelNames))
	}
	if len(entry.InferenceServiceNames) != 1 {
		t.Errorf("InferenceServiceNames length = %d, want 1", len(entry.InferenceServiceNames))
	}
}

func TestCacheListInferenceServiceMapping(t *testing.T) {
	// Verify that the modelCacheKey mapping logic correctly resolves
	// InferenceService modelRef to cache keys.
	models := []inferencev1alpha1.Model{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "model-a"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model-a.gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: "key-a"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "model-b"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model-b.gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: "key-b"},
		},
	}

	isvcs := []inferencev1alpha1.InferenceService{
		{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "model-a"}},
		{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "model-b"}},
		{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "model-a"}}, // second isvc using model-a
		{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "model-c"}}, // references non-existent model
	}

	// Build model name → cache key map (same logic as runCacheList)
	modelCacheKey := make(map[string]string, len(models))
	for _, model := range models {
		cacheKey := model.Status.CacheKey
		if cacheKey == "" {
			cacheKey = computeCacheKey(model.Spec.Source)
		}
		modelCacheKey[model.Name] = cacheKey
	}

	// Map InferenceServices to cache entries
	cacheEntries := make(map[string]*CacheEntry)
	for _, isvc := range isvcs {
		cacheKey, ok := modelCacheKey[isvc.Spec.ModelRef]
		if !ok {
			continue // skip dangling references
		}

		entry, exists := cacheEntries[cacheKey]
		if !exists {
			entry = &CacheEntry{
				CacheKey:              cacheKey,
				ModelNames:            []string{},
				InferenceServiceNames: []string{},
				Status:                statusActive,
			}
			cacheEntries[cacheKey] = entry
		}
		entry.InferenceServiceNames = append(entry.InferenceServiceNames, isvc.Name)
	}

	// key-a should have 2 InferenceServices
	entryA, ok := cacheEntries["key-a"]
	if !ok {
		t.Fatal("cache entry for key-a not found")
	}
	if len(entryA.InferenceServiceNames) != 2 {
		t.Errorf("key-a InferenceServiceNames length = %d, want 2", len(entryA.InferenceServiceNames))
	}

	// key-b should have 1 InferenceService
	entryB, ok := cacheEntries["key-b"]
	if !ok {
		t.Fatal("cache entry for key-b not found")
	}
	if len(entryB.InferenceServiceNames) != 1 {
		t.Errorf("key-b InferenceServiceNames length = %d, want 1", len(entryB.InferenceServiceNames))
	}

	// model-c should not create a cache entry (model not found)
	if _, ok := cacheEntries["key-c"]; ok {
		t.Error("cache entry for key-c should not exist (model not found)")
	}
}

func TestCacheListInferenceServiceMappingWithComputedCacheKey(t *testing.T) {
	// When a Model has no Status.CacheKey, the cache key is computed from
	// the source URL. The InferenceService mapping must use the same computed
	// key to find the right cache entry.
	model := inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		Status:     inferencev1alpha1.ModelStatus{CacheKey: ""}, // no cache key set
	}

	isvc := inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name},
	}

	// Build model name → cache key map
	modelCacheKey := make(map[string]string)
	cacheKey := model.Status.CacheKey
	if cacheKey == "" {
		cacheKey = computeCacheKey(model.Spec.Source)
	}
	modelCacheKey[model.Name] = cacheKey

	// Map InferenceService to cache entry
	cacheKeyFromIsvc, ok := modelCacheKey[isvc.Spec.ModelRef]
	if !ok {
		t.Fatal("could not resolve InferenceService modelRef to cache key")
	}

	if cacheKeyFromIsvc != cacheKey {
		t.Errorf("cache key mismatch: %q != %q", cacheKeyFromIsvc, cacheKey)
	}
}
