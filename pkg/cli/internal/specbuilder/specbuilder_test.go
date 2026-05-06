/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package specbuilder

import (
	"testing"
)

func TestBuild_MinimalInput(t *testing.T) {
	in := Input{
		Name:        "phi-4-mini",
		Namespace:   "default",
		ModelSource: "https://huggingface.co/example/phi-4-mini.gguf",
		ModelFormat: "gguf",
		Accelerator: "cpu",
		CPU:         "500m",
		Memory:      "1Gi",
		Replicas:    1,
	}

	model, isvc := Build(in)

	if model.Name != "phi-4-mini" {
		t.Errorf("model.Name = %q, want phi-4-mini", model.Name)
	}
	if model.Spec.Hardware == nil || model.Spec.Hardware.Accelerator != "cpu" {
		t.Errorf("model.Spec.Hardware.Accelerator unexpected: %+v", model.Spec.Hardware)
	}
	if model.Spec.Hardware.GPU != nil {
		t.Errorf("CPU-only input produced GPU spec: %+v", model.Spec.Hardware.GPU)
	}

	if isvc.Spec.ModelRef != "phi-4-mini" {
		t.Errorf("isvc.Spec.ModelRef = %q, want phi-4-mini", isvc.Spec.ModelRef)
	}
	if isvc.Spec.Replicas == nil || *isvc.Spec.Replicas != 1 {
		t.Errorf("isvc.Spec.Replicas not 1: %v", isvc.Spec.Replicas)
	}
	if isvc.Spec.ContextSize != nil {
		t.Errorf("ContextSize should be unset when Input.ContextSize=0; got %v", isvc.Spec.ContextSize)
	}
	if isvc.Spec.Endpoint == nil || isvc.Spec.Endpoint.Port != 8080 {
		t.Errorf("default endpoint port not 8080: %+v", isvc.Spec.Endpoint)
	}
	if isvc.Spec.Runtime != "" {
		t.Errorf("Runtime should be empty (controller default), got %q", isvc.Spec.Runtime)
	}
}

func TestBuild_FullKnobs(t *testing.T) {
	flashOn := true
	jinjaOn := true
	frac := 0.9

	in := Input{
		Name:                "qwen36-coder",
		Namespace:           "default",
		ModelSource:         "mlx-community/Qwen3.6-35B-A3B-8bit",
		ModelFormat:         "mlx",
		Accelerator:         "metal",
		MetalMemoryFraction: &frac,
		CPU:                 "1",
		Memory:              "4Gi",
		Runtime:             "vllm-swift",
		Replicas:            1,
		ContextSize:         262144,
		ParallelSlots:       4,
		FlashAttention:      &flashOn,
		Jinja:               &jinjaOn,
		CacheTypeK:          "f16",
		CacheTypeV:          "f16",
	}

	model, isvc := Build(in)

	if model.Spec.Hardware.MemoryFraction == nil || *model.Spec.Hardware.MemoryFraction != 0.9 {
		t.Errorf("MetalMemoryFraction not propagated: %v", model.Spec.Hardware.MemoryFraction)
	}
	if isvc.Spec.Runtime != "vllm-swift" {
		t.Errorf("Runtime = %q, want vllm-swift", isvc.Spec.Runtime)
	}
	if isvc.Spec.ContextSize == nil || *isvc.Spec.ContextSize != 262144 {
		t.Errorf("ContextSize = %v, want 262144", isvc.Spec.ContextSize)
	}
	if isvc.Spec.ParallelSlots == nil || *isvc.Spec.ParallelSlots != 4 {
		t.Errorf("ParallelSlots = %v, want 4", isvc.Spec.ParallelSlots)
	}
	if isvc.Spec.FlashAttention == nil || !*isvc.Spec.FlashAttention {
		t.Errorf("FlashAttention not true")
	}
	if isvc.Spec.Jinja == nil || !*isvc.Spec.Jinja {
		t.Errorf("Jinja not true")
	}
	if isvc.Spec.CacheTypeK != "f16" || isvc.Spec.CacheTypeV != "f16" {
		t.Errorf("CacheType K/V not f16/f16: %q/%q", isvc.Spec.CacheTypeK, isvc.Spec.CacheTypeV)
	}
}

func TestBuild_GPUInput(t *testing.T) {
	in := Input{
		Name:        "llama-3.1-8b",
		Namespace:   "default",
		ModelSource: "https://huggingface.co/example/llama-8b.gguf",
		ModelFormat: "gguf",
		Accelerator: "cuda",
		CPU:         "2",
		Memory:      "8Gi",
		GPUMemory:   "16Gi",
		GPUCount:    1,
		GPUVendor:   "nvidia",
		GPULayers:   33,
		Replicas:    1,
	}

	model, isvc := Build(in)

	if model.Spec.Hardware.GPU == nil {
		t.Fatal("expected GPU spec; got nil")
	}
	if !model.Spec.Hardware.GPU.Enabled {
		t.Error("GPU not enabled")
	}
	if model.Spec.Hardware.GPU.Count != 1 {
		t.Errorf("GPU.Count = %d, want 1", model.Spec.Hardware.GPU.Count)
	}
	if model.Spec.Hardware.GPU.Layers != 33 {
		t.Errorf("GPU.Layers = %d, want 33", model.Spec.Hardware.GPU.Layers)
	}
	if isvc.Spec.Resources.GPU != 1 {
		t.Errorf("isvc.Spec.Resources.GPU = %d, want 1", isvc.Spec.Resources.GPU)
	}
	if isvc.Spec.Resources.GPUMemory != "16Gi" {
		t.Errorf("isvc.Spec.Resources.GPUMemory = %q, want 16Gi", isvc.Spec.Resources.GPUMemory)
	}
}

func TestBuild_RuntimeLlamacppNormalizesToEmpty(t *testing.T) {
	// "llamacpp" is the controller default; specbuilder mirrors deploy.go's
	// behavior of leaving the field unset when the user picked the default,
	// so the spec stays minimal and looks identical to what existed before.
	in := Input{
		Name:        "x",
		Namespace:   "default",
		ModelSource: "x",
		ModelFormat: "gguf",
		Accelerator: "cpu",
		CPU:         "100m",
		Memory:      "256Mi",
		Runtime:     "llamacpp",
		Replicas:    1,
	}
	_, isvc := Build(in)
	if isvc.Spec.Runtime != "" {
		t.Errorf("Runtime=llamacpp should normalize to empty, got %q", isvc.Spec.Runtime)
	}
}
