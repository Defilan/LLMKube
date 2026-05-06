/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseQuantFromName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Llama-3.1-8B-Instruct-Q5_K_M", "Q5_K_M"},
		{"Qwen3.6-35B-A3B-Q8_0", "Q8_0"},
		{"DeepSeek-R1-Distill-Qwen-32B-Q4_K_M.gguf", "Q4_K_M"},
		{"Mistral-7B-IQ4_NL", "IQ4_NL"},
		{"Qwen3.6-35B-A3B-8bit", "8BIT"},
		{"mlx-community-Qwen3-4bit", "4BIT"},
		{"some-fp16-model", "FP16"},
		{"unrelated-filename", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQuantFromName(tc.name)
			if got != tc.want {
				t.Fatalf("parseQuantFromName(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestScanLocal_GGUFRoot(t *testing.T) {
	root := t.TempDir()
	// Simulate ~/llmkube-models/qwen36-35b-a3b/Qwen3.6-35B-A3B-Q8_0.gguf
	modelDir := filepath.Join(root, "qwen36-35b-a3b")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ggufPath := filepath.Join(modelDir, "Qwen3.6-35B-A3B-Q8_0.gguf")
	mustWrite(t, ggufPath, []byte("fake gguf content"))

	results := scanRoot(root)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	r := results[0]
	if r.Format != "gguf" {
		t.Errorf("Format = %q, want gguf", r.Format)
	}
	if r.Quant != "Q8_0" {
		t.Errorf("Quant = %q, want Q8_0", r.Quant)
	}
	if !strings.HasSuffix(r.Path, ".gguf") {
		t.Errorf("Path = %q, want .gguf suffix", r.Path)
	}
	if r.SizeBytes != int64(len("fake gguf content")) {
		t.Errorf("SizeBytes = %d, want %d", r.SizeBytes, len("fake gguf content"))
	}
}

func TestScanLocal_MLXLayout(t *testing.T) {
	root := t.TempDir()
	// Simulate ~/models/Qwen3.6-35B-A3B-8bit/ with MLX-style files
	modelDir := filepath.Join(root, "Qwen3.6-35B-A3B-8bit")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(modelDir, "config.json"), []byte("{}"))
	mustWrite(t, filepath.Join(modelDir, "model.safetensors.index.json"), []byte("{}"))
	mustWrite(t, filepath.Join(modelDir, "mlx_lm.json"), []byte("{}"))
	mustWrite(t, filepath.Join(modelDir, "weights.npz"), make([]byte, 4096))

	results := scanRoot(root)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	r := results[0]
	if r.Format != "mlx" {
		t.Errorf("Format = %q, want mlx", r.Format)
	}
	if r.Quant != "8BIT" {
		t.Errorf("Quant = %q, want 8BIT", r.Quant)
	}
	if r.Path != modelDir {
		t.Errorf("Path = %q, want %q", r.Path, modelDir)
	}
	// 4 files: config + index + mlx_lm.json (each 2 bytes) + 4096-byte npz
	if r.SizeBytes < 4096 {
		t.Errorf("SizeBytes = %d, want >= 4096", r.SizeBytes)
	}
}

func TestScanLocal_SafetensorsLayout(t *testing.T) {
	root := t.TempDir()
	modelDir := filepath.Join(root, "Llama-3.1-8B-Instruct-FP16")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(modelDir, "config.json"), []byte("{}"))
	mustWrite(t, filepath.Join(modelDir, "model-00001-of-00002.safetensors"), []byte("data"))
	mustWrite(t, filepath.Join(modelDir, "model.safetensors.index.json"), []byte("{}"))

	results := scanRoot(root)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	r := results[0]
	if r.Format != "safetensors" {
		t.Errorf("Format = %q, want safetensors", r.Format)
	}
	if r.Quant != "FP16" {
		t.Errorf("Quant = %q, want FP16", r.Quant)
	}
}

func TestScanLocal_HFCache(t *testing.T) {
	hubRoot := t.TempDir()
	// Simulate ~/.cache/huggingface/hub/models--meta-llama--Llama-3.1-8B/snapshots/abc123/
	repoDir := filepath.Join(hubRoot, "models--meta-llama--Llama-3.1-8B")
	snapDir := filepath.Join(repoDir, "snapshots", "abc123")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(snapDir, "config.json"), []byte("{}"))
	mustWrite(t, filepath.Join(snapDir, "model.safetensors"), []byte("payload"))

	// Force scanRoot to take the HF path by giving the dir a name ending in "hub"
	// inside an "huggingface" path.
	wrapper := filepath.Join(t.TempDir(), "huggingface", "hub")
	if err := os.MkdirAll(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	// Symlink the repo into the wrapper so the HF dispatcher fires.
	if err := os.Symlink(repoDir, filepath.Join(wrapper, "models--meta-llama--Llama-3.1-8B")); err != nil {
		t.Fatal(err)
	}

	results := scanRoot(wrapper)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	r := results[0]
	if r.Format != "safetensors" {
		t.Errorf("Format = %q, want safetensors", r.Format)
	}
	if r.DisplayName != "meta-llama/Llama-3.1-8B" {
		t.Errorf("DisplayName = %q, want meta-llama/Llama-3.1-8B", r.DisplayName)
	}
}

func TestScanLocal_MissingDirsAreSilent(t *testing.T) {
	results := scanRoot("/path/that/definitely/does/not/exist/llmkube")
	if results != nil {
		t.Fatalf("expected nil for missing root, got %+v", results)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
