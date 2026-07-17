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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// llamaCppRouterLog is a package-level logger used for construction-time warnings from
// BuildArgs.
var llamaCppRouterLog = logf.Log.WithName("runtime.llamacpp-router")

// RuntimeLlamaCppRouter is the InferenceService.Spec.Runtime value that selects the
// llama.cpp router backend. Kept as a named constant so callers can cross-check the
// runtime without duplicating the string literal.
const RuntimeLlamaCppRouter = "llamacpp-router"

// LlamaCppRouterBackend generates container configuration for the llama.cpp inference
// server in router mode, which allows one Pod to host multiple models with dynamic
// loading/unloading.
type LlamaCppRouterBackend struct{}

func (b *LlamaCppRouterBackend) ContainerName() string {
	return "llama-server"
}

func (b *LlamaCppRouterBackend) DefaultImage() string {
	return "ghcr.io/ggml-org/llama.cpp:server"
}

func (b *LlamaCppRouterBackend) DefaultPort() int32 {
	return 8080
}

func (b *LlamaCppRouterBackend) NeedsModelInit() bool {
	// Router mode does not need a model init container because models are loaded
	// dynamically from the models directory at runtime.
	return false
}

func (b *LlamaCppRouterBackend) DefaultHPAMetric() string {
	return "llamacpp:requests_processing"
}

// BuildArgs generates the llama.cpp server CLI arguments for router mode.
// Router mode uses --models-dir instead of --model to enable dynamic multi-model
// loading. Arguments are emitted in a deterministic order:
//
//  1. --models-dir (always, pointing to the shared model cache)
//  2. --host, --port (always)
//  3. GPU configuration flags (when GPU resources are present)
//  4. ExtraArgs (user escape hatch, always last so user flags win)
func (b *LlamaCppRouterBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, port int32) []string {
	// Router mode uses --models-dir to point to the directory containing models.
	// The operator mounts the shared model cache at /models, so we use that path.
	args := []string{
		"--models-dir", "/models",
		// Bind the dual-stack wildcard so pods are reachable on IPv6-only
		// clusters (#972). With the default net.ipv6.bindv6only=0, :: also
		// accepts IPv4, so IPv4-only and dual-stack clusters keep working.
		// On IPv6-disabled nodes, override via extraArgs ("--host", "0.0.0.0");
		// llama.cpp keeps the last occurrence of a repeated flag.
		"--host", "::",
		"--port", fmt.Sprintf("%d", port),
	}

	// GPU configuration: router mode still needs GPU flags when GPU resources
	// are present, but we don't set --n-gpu-layers or sharding since models
	// are loaded dynamically and each model can have its own GPU configuration
	// via INI presets or model-specific settings.
	gpuCount := resolveGPUCount(isvc, model)
	if gpuCount > 0 {
		// In router mode, we don't set --n-gpu-layers or --tensor-split because
		// different models may need different GPU configurations. The user can
		// specify GPU settings via ExtraArgs if they want to apply them globally,
		// or use INI presets for model-specific GPU configuration.
		llamaCppRouterLog.Info(
			"GPU resources present but GPU sharding flags omitted in router mode; "+
				"use ExtraArgs or INI presets for model-specific GPU configuration",
			"inferenceService", isvc.Name,
			"namespace", isvc.Namespace,
			"gpuCount", gpuCount,
		)
	}

	// Enable Prometheus metrics endpoint on llama.cpp
	args = append(args, "--metrics")

	// Append user-provided extra args last so they can override defaults
	if len(isvc.Spec.ExtraArgs) > 0 {
		args = append(args, isvc.Spec.ExtraArgs...)
	}

	return args
}

func (b *LlamaCppRouterBackend) BuildProbes(port int32) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
	startup := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 180,
	}
	liveness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    15,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	readiness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	return startup, liveness, readiness
}

// IdleProbe returns a probe closure that checks llama.cpp /slots endpoint for
// idle status. All slots must report is_processing == false for the probe to
// return true. This is the same logic used by the single-model llamacpp backend.
func (b *LlamaCppRouterBackend) IdleProbe(_ *inferencev1alpha1.InferenceService, client *http.Client) func(ctx context.Context, baseURL string) (bool, error) {
	return func(ctx context.Context, baseURL string) (bool, error) {
		url := fmt.Sprintf("%s/slots", baseURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Errorf("failed to query /slots: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("/slots returned status %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, fmt.Errorf("failed to read /slots response: %w", err)
		}

		var slots []llamaCPUSlot
		if err := json.Unmarshal(body, &slots); err != nil {
			return false, fmt.Errorf("failed to parse /slots response: %w", err)
		}

		for _, slot := range slots {
			if slot.IsProcessing {
				return false, nil
			}
		}

		return true, nil
	}
}
