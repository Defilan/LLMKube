package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// WhisperBackend generates container configuration for speaches
// (https://speaches.ai), the faster-whisper OpenAI-compatible audio
// transcription server. speaches serves /v1/audio/transcriptions on port 8000,
// is configured entirely via environment variables, and lazy-loads CTranslate2
// models from HuggingFace per request (so there is no model-init step and the
// model id clients request comes from the referenced Model's spec.source).
type WhisperBackend struct{}

// whisperImage is the pinned default speaches image. CUDA by default; CPU-only
// deployments should override spec.image with the ...-cpu tag.
const whisperImage = "ghcr.io/speaches-ai/speaches:0.8.3-cuda"

// whisperHFHome is where speaches' underlying huggingface_hub caches models.
// The image runs as the non-root "ubuntu" user with HOME=/home/ubuntu.
const whisperHFHome = "/home/ubuntu/.cache/huggingface"

// whisperComputeTypes is the set of CTranslate2 compute types speaches accepts
// (WHISPER__COMPUTE_TYPE). Used to decide whether a Model's quantization string
// can be passed through as a compute type.
var whisperComputeTypes = map[string]struct{}{
	"int8": {}, "int8_float16": {}, "int8_bfloat16": {}, "int8_float32": {},
	"int16": {}, "float16": {}, "bfloat16": {}, "float32": {}, "default": {},
}

func (b *WhisperBackend) ContainerName() string { return "speaches" }
func (b *WhisperBackend) DefaultImage() string  { return whisperImage }
func (b *WhisperBackend) DefaultPort() int32    { return 8000 }

// NeedsModelInit is false: speaches downloads the CTranslate2 model from
// HuggingFace at request time, so no model-downloader init container is needed.
func (b *WhisperBackend) NeedsModelInit() bool { return false }

// DefaultHPAMetric returns "" because speaches exposes no Prometheus queue
// metric to autoscale on.
func (b *WhisperBackend) DefaultHPAMetric() string { return "" }

// DefaultEndpointPath advertises the OpenAI audio transcription path so the
// status endpoint points clients at the right route.
func (b *WhisperBackend) DefaultEndpointPath() string { return "/v1/audio/transcriptions" }

// BuildArgs returns only the user's extra args: speaches is configured via env
// vars, not CLI flags (see BuildEnv).
func (b *WhisperBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, _ *inferencev1alpha1.Model, _ string, _ int32) []string {
	return isvc.Spec.ExtraArgs
}

func (b *WhisperBackend) BuildProbes(port int32) (startup, liveness, readiness *corev1.Probe) {
	healthGet := func() corev1.ProbeHandler {
		return corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		}
	}
	startup = &corev1.Probe{
		ProbeHandler:     healthGet(),
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 180,
	}
	liveness = &corev1.Probe{
		ProbeHandler:     healthGet(),
		PeriodSeconds:    15,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	readiness = &corev1.Probe{
		ProbeHandler:     healthGet(),
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	return startup, liveness, readiness
}

// BuildEnv translates the Model and WhisperConfig into speaches environment
// variables. Emitted in a stable order so Deployment specs are deterministic.
func (b *WhisperBackend) BuildEnv(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) []corev1.EnvVar {
	cfg := isvc.Spec.WhisperConfig

	env := []corev1.EnvVar{
		{Name: "HF_HOME", Value: whisperHFHome},
		{Name: "ENABLE_UI", Value: whisperEnableUI(cfg)},
		{Name: "WHISPER__INFERENCE_DEVICE", Value: whisperDevice(cfg, model)},
	}

	if ct := whisperComputeType(cfg, model); ct != "" {
		env = append(env, corev1.EnvVar{Name: "WHISPER__COMPUTE_TYPE", Value: ct})
	}
	if cfg != nil && cfg.ModelTTLSeconds != nil {
		env = append(env, corev1.EnvVar{Name: "WHISPER__TTL", Value: fmt.Sprintf("%d", *cfg.ModelTTLSeconds)})
	}
	if cfg != nil && cfg.HFTokenSecretRef != nil {
		env = append(env, corev1.EnvVar{
			Name:      "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.HFTokenSecretRef},
		})
	}
	if cfg != nil && cfg.APIKeySecretRef != nil {
		env = append(env, corev1.EnvVar{
			Name:      "API_KEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.APIKeySecretRef},
		})
	}

	return env
}

func whisperEnableUI(cfg *inferencev1alpha1.WhisperConfig) string {
	if cfg != nil && cfg.EnableUI != nil && *cfg.EnableUI {
		return "true"
	}
	return "false"
}

// whisperDevice resolves the speaches inference device: explicit config wins,
// otherwise it is derived from the Model accelerator, defaulting to "auto".
func whisperDevice(cfg *inferencev1alpha1.WhisperConfig, model *inferencev1alpha1.Model) string {
	if cfg != nil && cfg.InferenceDevice != "" {
		return cfg.InferenceDevice
	}
	if model != nil && model.Spec.Hardware != nil {
		switch strings.ToLower(model.Spec.Hardware.Accelerator) {
		case "cuda":
			return "cuda"
		case "cpu", "metal":
			// CTranslate2 has no Metal backend; fall back to CPU.
			return "cpu"
		}
	}
	return "auto"
}

// whisperComputeType resolves WHISPER__COMPUTE_TYPE: explicit config wins,
// otherwise a Model quantization string is passed through only if speaches
// recognizes it as a compute type. Returns "" to use the speaches default.
func whisperComputeType(cfg *inferencev1alpha1.WhisperConfig, model *inferencev1alpha1.Model) string {
	if cfg != nil && cfg.ComputeType != "" {
		return cfg.ComputeType
	}
	if model != nil {
		q := strings.ToLower(strings.TrimSpace(model.Spec.Quantization))
		if _, ok := whisperComputeTypes[q]; ok {
			return q
		}
	}
	return ""
}
