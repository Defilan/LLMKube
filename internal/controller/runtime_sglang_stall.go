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
	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sglangStallSignals were measured on SGLang v0.5.15 (Qwen3.8-27B-FP8 on a
// B200) during a ~200K-token prefill and a 600-token decode:
//   - num_running_reqs stays 0 while a request is being prefilled, so it
//     alone cannot mark the engine busy; kv_used_tokens is above zero through
//     prefill and decode and returns to 0 when idle.
//   - prompt_tokens_total and generation_tokens_total only advance when a
//     request finishes.
//   - realtime_tokens_total advances on every prefill chunk (+16384) and
//     every decode step, so it is the heartbeat; kv_used_tokens also changes
//     on every chunk.
//
// realtime_tokens_total and num_running_reqs are required: without the
// heartbeat the detector cannot tell a long prefill from a wedge, so it
// fails open.
var sglangStallSignals = stallSignals{
	Busy:     []string{"sglang:num_running_reqs", "sglang:kv_used_tokens"},
	Counters: []string{"sglang:realtime_tokens_total", "sglang:generation_tokens_total", "sglang:prompt_tokens_total"},
	Gauges:   []string{"sglang:kv_used_tokens"},
	Required: []string{"sglang:num_running_reqs", "sglang:realtime_tokens_total"},
}

// sglangStallProbeCommand is the SGLang stall probe argv.
func sglangStallProbeCommand(port, windowSeconds int32) []string {
	return stallProbeCommandFor(sglangStallSignals, port, windowSeconds)
}

// sglangStallDetectionEnabled resolves spec.sglangConfig.stallDetection: the
// window to use, and whether the feature is opted into.
func sglangStallDetectionEnabled(isvc *inferencev1alpha1.InferenceService) (int32, bool) {
	cfg := isvc.Spec.SGLangConfig
	if cfg == nil || cfg.StallDetection == nil ||
		cfg.StallDetection.Enabled == nil || !*cfg.StallDetection.Enabled {
		return 0, false
	}
	window := vllmStallDefaultWindowSeconds
	if w := cfg.StallDetection.WindowSeconds; w != nil {
		window = *w
	}
	return window, true
}

// CustomizeProbes implements ProbeCustomizer. With stall detection opted
// into, the liveness probe stops asking SGLang /health and execs the stall
// detector instead; startup and readiness are unchanged.
// spec.probeOverrides.liveness still wins because the deployment builder
// applies overrides after this.
func (b *SGLangBackend) CustomizeProbes(
	isvc *inferencev1alpha1.InferenceService,
	port int32,
	startup, liveness, readiness *corev1.Probe,
) {
	window, enabled := sglangStallDetectionEnabled(isvc)
	if !enabled {
		return
	}
	*liveness = stallLivenessProbe(sglangStallProbeCommand(port, window))
}
