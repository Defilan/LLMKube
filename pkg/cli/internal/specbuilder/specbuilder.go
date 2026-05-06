/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package specbuilder builds Model + InferenceService CRD objects from a
// minimal user-facing input shape. It exists so the TUI deploy form
// (pkg/cli/tui) and any future programmatic deployer can produce specs
// identical to what `llmkube deploy` writes today, without dragging in the
// full deployOptions flag-parsing surface.
//
// Scope is intentionally narrow: the fields the TUI form collects, and that
// `llmkube deploy` already supports. Adding a new field here should match an
// existing field on `inferencev1alpha1.InferenceServiceSpec` or `ModelSpec`,
// not introduce new CRD surface.
package specbuilder

import (
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Input is the user-facing shape the deploy form collects. Pointer fields
// represent "leave the spec field unset" when nil; non-pointer fields map
// straight through.
type Input struct {
	// Identity
	Name      string
	Namespace string

	// Model
	ModelSource  string // HF URL, hf://, file://, or pvc:// scheme
	ModelFormat  string // gguf, mlx, safetensors, pytorch, custom
	Quantization string // optional, free-form (e.g., "Q5_K_M")
	Accelerator  string // cpu, metal, cuda, rocm

	// Hardware (metal-agent only; ignored for other accelerators)
	MetalMemoryBudget   string   // e.g. "24Gi" — mutually exclusive with MetalMemoryFraction
	MetalMemoryFraction *float64 // 0.0 to 1.0

	// Resource requests
	CPU       string // e.g. "500m"
	Memory    string // e.g. "4Gi"
	GPUMemory string // optional GPU VRAM request

	// GPU (cuda/rocm)
	GPUCount  int32  // 0 for CPU-only deploys
	GPUVendor string // "nvidia", "amd"; empty defaults via accelerator
	GPULayers int32  // model layers offloaded to GPU; 0 = runtime default

	// Inference runtime
	Runtime string // "" = controller default; "llamacpp", "vllm", "vllm-swift", "tgi", "ollama", "generic"

	// Tunables (all pointer-style on the CRD; nil means unset)
	Replicas       int32
	ContextSize    int32 // 0 = unset
	ParallelSlots  int32 // 0 = unset
	FlashAttention *bool // nil = unset; true/false explicit
	Jinja          *bool
	CacheTypeK     string // "" = unset; "f16", "q8_0", "q5_0", "q4_0", etc.
	CacheTypeV     string

	// Image override (optional; controller picks default by runtime)
	Image string
}

// Build produces a Model and InferenceService for the given input. The
// returned objects are unsaved; the caller is responsible for client.Create.
//
// Mirrors the spec construction in pkg/cli/deploy.go so output is byte-for-byte
// identical for equivalent inputs. When that file is refactored to consume
// specbuilder directly, the TestEquivalentTo regression test in this package
// will catch any drift.
func Build(in Input) (*inferencev1alpha1.Model, *inferencev1alpha1.InferenceService) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name,
			Namespace: in.Namespace,
		},
		Spec: inferencev1alpha1.ModelSpec{
			Source:       in.ModelSource,
			Format:       in.ModelFormat,
			Quantization: in.Quantization,
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: in.Accelerator,
			},
			Resources: &inferencev1alpha1.ResourceRequirements{
				CPU:    in.CPU,
				Memory: in.Memory,
			},
		},
	}

	if in.GPUCount > 0 {
		model.Spec.Hardware.GPU = &inferencev1alpha1.GPUSpec{
			Enabled: true,
			Count:   in.GPUCount,
			Vendor:  in.GPUVendor,
		}
		if in.GPULayers != 0 {
			model.Spec.Hardware.GPU.Layers = in.GPULayers
		}
		if in.GPUMemory != "" {
			model.Spec.Hardware.GPU.Memory = in.GPUMemory
		}
	}

	if in.MetalMemoryBudget != "" {
		model.Spec.Hardware.MemoryBudget = in.MetalMemoryBudget
	}
	if in.MetalMemoryFraction != nil {
		model.Spec.Hardware.MemoryFraction = in.MetalMemoryFraction
	}

	replicas := in.Replicas
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name,
			Namespace: in.Namespace,
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: in.Name,
			Replicas: &replicas,
			Image:    in.Image,
			Endpoint: &inferencev1alpha1.EndpointSpec{
				Port: 8080,
				Path: "/v1/chat/completions",
				Type: "ClusterIP",
			},
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				CPU:    in.CPU,
				Memory: in.Memory,
			},
		},
	}

	if in.GPUCount > 0 {
		isvc.Spec.Resources.GPU = in.GPUCount
		if in.GPUMemory != "" {
			isvc.Spec.Resources.GPUMemory = in.GPUMemory
		}
	}

	if in.ContextSize > 0 {
		ctx := in.ContextSize
		isvc.Spec.ContextSize = &ctx
	}
	if in.ParallelSlots > 0 {
		ps := in.ParallelSlots
		isvc.Spec.ParallelSlots = &ps
	}
	if in.FlashAttention != nil {
		isvc.Spec.FlashAttention = in.FlashAttention
	}
	if in.Jinja != nil {
		isvc.Spec.Jinja = in.Jinja
	}
	if in.CacheTypeK != "" {
		isvc.Spec.CacheTypeK = in.CacheTypeK
	}
	if in.CacheTypeV != "" {
		isvc.Spec.CacheTypeV = in.CacheTypeV
	}
	// Mirror deploy.go: only set Runtime when non-default. Empty and "llamacpp"
	// both leave the controller default in place.
	if in.Runtime != "" && in.Runtime != "llamacpp" {
		isvc.Spec.Runtime = in.Runtime
	}

	return model, isvc
}
