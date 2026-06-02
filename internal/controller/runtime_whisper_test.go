package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func whisperModel(accelerator, quantization string) *inferencev1alpha1.Model {
	m := &inferencev1alpha1.Model{
		Spec: inferencev1alpha1.ModelSpec{
			Source:       "Systran/faster-whisper-large-v3",
			Quantization: quantization,
		},
	}
	if accelerator != "" {
		m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{Accelerator: accelerator}
	}
	return m
}

func TestWhisperBackendBasics(t *testing.T) {
	b := &WhisperBackend{}

	if b.ContainerName() != "speaches" {
		t.Errorf("ContainerName() = %q, want speaches", b.ContainerName())
	}
	if b.DefaultPort() != 8000 {
		t.Errorf("DefaultPort() = %d, want 8000", b.DefaultPort())
	}
	if b.NeedsModelInit() {
		t.Error("NeedsModelInit() = true, want false (speaches fetches from HF at runtime)")
	}
	if b.DefaultHPAMetric() != "" {
		t.Errorf("DefaultHPAMetric() = %q, want empty (speaches exposes no scrapeable queue metric)", b.DefaultHPAMetric())
	}
	if got := b.DefaultEndpointPath(); got != "/v1/audio/transcriptions" {
		t.Errorf("DefaultEndpointPath() = %q, want /v1/audio/transcriptions", got)
	}
	if img := b.DefaultImage(); img == "" || !containsSubstr(img, "speaches") {
		t.Errorf("DefaultImage() = %q, want a pinned speaches image", img)
	}
}

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestWhisperBuildProbes(t *testing.T) {
	b := &WhisperBackend{}
	startup, liveness, readiness := b.BuildProbes(8000)
	for name, p := range map[string]*corev1.Probe{"startup": startup, "liveness": liveness, "readiness": readiness} {
		if p == nil || p.HTTPGet == nil {
			t.Fatalf("%s probe should be an HTTP GET", name)
			continue
		}
		if p.HTTPGet.Path != "/health" {
			t.Errorf("%s probe path = %q, want /health", name, p.HTTPGet.Path)
		}
		if p.HTTPGet.Port.IntValue() != 8000 {
			t.Errorf("%s probe port = %v, want 8000", name, p.HTTPGet.Port)
		}
	}
}

func TestWhisperBuildEnv(t *testing.T) {
	secretRef := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "hf"},
		Key:                  "token",
	}
	apiRef := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "api"},
		Key:                  "key",
	}

	tests := []struct {
		name          string
		cfg           *inferencev1alpha1.WhisperConfig
		model         *inferencev1alpha1.Model
		wantEnv       map[string]string // name -> exact .Value
		wantAbsent    []string
		wantHFSecret  bool
		wantAPISecret bool
	}{
		{
			name:  "minimal cpu model: HF_HOME, UI off, device cpu, no compute/ttl",
			model: whisperModel("cpu", ""),
			wantEnv: map[string]string{
				"HF_HOME":                   "/home/ubuntu/.cache/huggingface",
				"ENABLE_UI":                 "false",
				"WHISPER__INFERENCE_DEVICE": "cpu",
				"LLMKUBE_WHISPER_MODEL":     "Systran/faster-whisper-large-v3",
			},
			wantAbsent: []string{"WHISPER__COMPUTE_TYPE", "WHISPER__TTL", "HF_TOKEN", "API_KEY"},
		},
		{
			name:    "cuda accelerator maps to cuda device",
			model:   whisperModel("cuda", ""),
			wantEnv: map[string]string{"WHISPER__INFERENCE_DEVICE": "cuda"},
		},
		{
			name:    "metal accelerator maps to cpu",
			model:   whisperModel("metal", ""),
			wantEnv: map[string]string{"WHISPER__INFERENCE_DEVICE": "cpu"},
		},
		{
			name:    "nil hardware defaults device to auto",
			model:   whisperModel("", ""),
			wantEnv: map[string]string{"WHISPER__INFERENCE_DEVICE": "auto"},
		},
		{
			name:    "config device overrides model accelerator",
			cfg:     &inferencev1alpha1.WhisperConfig{InferenceDevice: "auto"},
			model:   whisperModel("cuda", ""),
			wantEnv: map[string]string{"WHISPER__INFERENCE_DEVICE": "auto"},
		},
		{
			name:    "explicit compute type wins",
			cfg:     &inferencev1alpha1.WhisperConfig{ComputeType: "int8_float16"},
			model:   whisperModel("cuda", "float16"),
			wantEnv: map[string]string{"WHISPER__COMPUTE_TYPE": "int8_float16"},
		},
		{
			name:    "recognized model quantization becomes compute type",
			model:   whisperModel("cuda", "float16"),
			wantEnv: map[string]string{"WHISPER__COMPUTE_TYPE": "float16"},
		},
		{
			name:       "unrecognized quantization omits compute type",
			model:      whisperModel("cuda", "Q4_K_M"),
			wantAbsent: []string{"WHISPER__COMPUTE_TYPE"},
		},
		{
			name:    "model ttl -1 keeps loaded",
			cfg:     &inferencev1alpha1.WhisperConfig{ModelTTLSeconds: ptrInt32(-1)},
			model:   whisperModel("cuda", ""),
			wantEnv: map[string]string{"WHISPER__TTL": "-1"},
		},
		{
			name:    "enable UI true",
			cfg:     &inferencev1alpha1.WhisperConfig{EnableUI: ptrBool(true)},
			model:   whisperModel("cuda", ""),
			wantEnv: map[string]string{"ENABLE_UI": "true"},
		},
		{
			name:         "HF token secret ref",
			cfg:          &inferencev1alpha1.WhisperConfig{HFTokenSecretRef: secretRef},
			model:        whisperModel("cuda", ""),
			wantHFSecret: true,
		},
		{
			name:          "API key secret ref",
			cfg:           &inferencev1alpha1.WhisperConfig{APIKeySecretRef: apiRef},
			model:         whisperModel("cuda", ""),
			wantAPISecret: true,
		},
	}

	b := &WhisperBackend{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime:       "whisper",
					WhisperConfig: tc.cfg,
				},
			}
			env := b.BuildEnv(isvc, tc.model)

			for name, want := range tc.wantEnv {
				if !containsEnv(env, name, want) {
					t.Errorf("env %s = %q not found; got %+v", name, want, env)
				}
			}
			for _, name := range tc.wantAbsent {
				if containsEnv(env, name, "") {
					t.Errorf("env %s should be absent; got %+v", name, env)
				}
			}
			if tc.wantHFSecret && envSecretRef(env, "HF_TOKEN") == nil {
				t.Errorf("HF_TOKEN should be backed by a secret ref; got %+v", env)
			}
			if tc.wantAPISecret && envSecretRef(env, "API_KEY") == nil {
				t.Errorf("API_KEY should be backed by a secret ref; got %+v", env)
			}
		})
	}
}

func TestWhisperBuildLifecycle(t *testing.T) {
	b := &WhisperBackend{}

	t.Run("postStart preloads the model", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "whisper"},
		}
		lc := b.BuildLifecycle(isvc, whisperModel("cuda", ""), 8000)
		if lc == nil || lc.PostStart == nil || lc.PostStart.Exec == nil {
			t.Fatal("expected a postStart exec hook")
		}
		cmd := lc.PostStart.Exec.Command
		if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
			t.Fatalf("expected sh -c <script>, got %v", cmd)
		}
		script := cmd[2]
		for _, want := range []string{"/v1/models/$LLMKUBE_WHISPER_MODEL", "localhost:8000/health", "-X POST"} {
			if !containsSubstr(script, want) {
				t.Errorf("postStart script missing %q; got:\n%s", want, script)
			}
		}
	})

	t.Run("nil when no model source", func(t *testing.T) {
		isvc := &inferencev1alpha1.InferenceService{}
		if lc := b.BuildLifecycle(isvc, &inferencev1alpha1.Model{}, 8000); lc != nil {
			t.Errorf("expected nil lifecycle when model source empty, got %+v", lc)
		}
	})
}

// TestConstructEndpointRuntimeAwareDefault verifies the runtime-aware default path:
// whisper resolves to the audio endpoint, other runtimes keep the chat endpoint,
// and an explicit spec.endpoint.path always wins.
func TestConstructEndpointRuntimeAwareDefault(t *testing.T) {
	r := &InferenceServiceReconciler{}
	svc := &corev1.Service{}
	svc.Name = "demo"
	svc.Namespace = "default"

	cases := []struct {
		name     string
		runtime  string
		path     string
		wantEnds string
	}{
		// Port follows the backend DefaultPort: whisper/vllm on 8000, llamacpp on 8080.
		{name: "whisper default", runtime: "whisper", wantEnds: ":8000/v1/audio/transcriptions"},
		{name: "llamacpp default", runtime: "", wantEnds: ":8080/v1/chat/completions"},
		{name: "vllm default", runtime: "vllm", wantEnds: ":8000/v1/chat/completions"},
		{name: "explicit path wins on whisper", runtime: "whisper", path: "/custom", wantEnds: ":8000/custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: tc.runtime},
			}
			if tc.path != "" {
				isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Path: tc.path}
			}
			got := r.constructEndpoint(isvc, svc)
			if !endsWith(got, tc.wantEnds) {
				t.Errorf("constructEndpoint() = %q, want suffix %q", got, tc.wantEnds)
			}
		})
	}
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// TestResolveServicePort verifies the Service/endpoint port follows the backend
// default (so non-8080 runtimes route correctly) while explicit overrides win.
func TestResolveServicePort(t *testing.T) {
	cases := []struct {
		name          string
		runtime       string
		containerPort *int32
		endpointPort  int32
		want          int32
	}{
		{name: "whisper defaults to 8000", runtime: "whisper", want: 8000},
		{name: "llamacpp defaults to 8080", runtime: "", want: 8080},
		{name: "tgi defaults to 80", runtime: "tgi", want: 80},
		{name: "endpoint port overrides backend default", runtime: "whisper", endpointPort: 9000, want: 9000},
		{name: "containerPort wins over endpoint port", runtime: "whisper", containerPort: ptrInt32(7000), endpointPort: 9000, want: 7000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime:       tc.runtime,
					ContainerPort: tc.containerPort,
				},
			}
			if tc.endpointPort > 0 {
				isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: tc.endpointPort}
			}
			if got := resolveServicePort(isvc); got != tc.want {
				t.Errorf("resolveServicePort() = %d, want %d", got, tc.want)
			}
		})
	}
}
