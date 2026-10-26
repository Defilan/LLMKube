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
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// fakeEngineNames returns the model ids an OpenAI-compatible engine started
// with args answers to, the way vLLM and SGLang parse them: every name after
// --served-model-name (or its =value form), else the positional model (vLLM)
// or --model-path (SGLang). It is written apart from the controller's own
// parser so that a parser bug cannot agree with itself.
func fakeEngineNames(args []string) []string {
	var names []string
	for i := 0; i < len(args); i++ {
		if v, ok := strings.CutPrefix(args[i], "--served-model-name="); ok {
			names = append(names, v)
			continue
		}
		if args[i] != "--served-model-name" {
			continue
		}
		for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			names = append(names, args[i])
		}
	}
	if len(names) > 0 {
		return names
	}
	for i, a := range args {
		if a == "--model-path" && i+1 < len(args) {
			return []string{args[i+1]}
		}
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return []string{args[0]}
	}
	return nil
}

// fakeEngine answers a chat completion with 200 only for a model it serves,
// and 404 "The model X does not exist" otherwise, as vLLM and SGLang do.
func fakeEngine(t *testing.T, names []string) (*httptest.Server, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		got = append(got, req.Model)
		if !slices.Contains(names, req.Model) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"The model `+req.Model+` does not exist."}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// TestRouterReachesTheNameTheEngineServes is the #1988 contract end to end: a
// ModelRouter in front of a vLLM or SGLang InferenceService whose modelRef is
// not its name. The engine is started with the args the controller builds
// for its container; the proxy runs on the config the controller compiles. A
// request the router rewrites must be one of the names that engine serves.
func TestRouterReachesTheNameTheEngineServes(t *testing.T) {
	const modelPath = "/models/a2df004d073e3716"
	cases := []struct {
		name      string
		backend   RuntimeBackend
		spec      inferencev1alpha1.InferenceServiceSpec
		wantModel string
	}{
		{
			name:      "vllm serves its modelRef",
			backend:   &VLLMBackend{},
			spec:      inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeVLLM, ModelRef: "glm-flash"},
			wantModel: "glm-flash",
		},
		{
			name:    "vllm serves the user's --served-model-name",
			backend: &VLLMBackend{},
			spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: RuntimeVLLM, ModelRef: "glm-flash",
				ExtraArgs: []string{"--served-model-name", "org/glm", "glm"},
			},
			wantModel: "org/glm",
		},
		{
			name:    "vllm with spec.command serves the name in spec.args",
			backend: &VLLMBackend{},
			spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: RuntimeVLLM, ModelRef: "glm-flash",
				Command: []string{"vllm", "serve"},
				Args:    []string{modelPath, "--served-model-name=glm-custom"},
			},
			wantModel: "glm-custom",
		},
		{
			name:      "sglang serves its modelRef",
			backend:   &SGLangBackend{},
			spec:      inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeSGLANG, ModelRef: "glm-flash"},
			wantModel: "glm-flash",
		},
		{
			name:    "sglang serves the user's --served-model-name",
			backend: &SGLangBackend{},
			spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: RuntimeSGLANG, ModelRef: "glm-flash",
				ExtraArgs: []string{"--served-model-name=glm-sg"},
			},
			wantModel: "glm-sg",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-svc", Namespace: testBuilderNs},
				Spec:       tc.spec,
			}
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "glm-flash", Namespace: testBuilderNs},
				Spec:       inferencev1alpha1.ModelSpec{Source: "hf://org/glm-flash"},
			}
			container := &corev1.Container{}
			setCommandAndArgs(container, isvc, tc.backend, tc.backend.BuildArgs(isvc, model, modelPath, "", 8000))
			engine, got := fakeEngine(t, fakeEngineNames(container.Args))

			mr := &inferencev1alpha1.ModelRouter{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-router", Namespace: testBuilderNs},
				Spec: inferencev1alpha1.ModelRouterSpec{
					Backends: []inferencev1alpha1.RouterBackend{{
						Name:                "chat",
						InferenceServiceRef: &corev1.LocalObjectReference{Name: "chat-svc"},
						Tier:                "local",
					}},
					DefaultRoute: "chat",
				},
			}
			r := newRouterReconcilerForTest(t, mr, isvc, model)
			compiled, err := r.compileRouterConfig(context.Background(), mr)
			if err != nil {
				t.Fatalf("compileRouterConfig: %v", err)
			}
			var cfg router.Config
			if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
				t.Fatalf("unmarshal compiled JSON: %v", err)
			}
			cfg.Backends[0].Address = engine.URL
			cfg.Backends[0].Endpoints = nil
			cfg.Backends[0].EndpointSource = nil
			if err := cfg.Validate(); err != nil {
				t.Fatalf("compiled config: %v", err)
			}
			mux := http.NewServeMux()
			router.NewProxy(&cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Mount(mux)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK || len(*got) == 0 || (*got)[len(*got)-1] != tc.wantModel {
				t.Fatalf("request for %q through the router: status %d, engine saw models %q; "+
					"the engine serves %q (args %q), want it to see %q",
					"chat", rec.Code, *got, fakeEngineNames(container.Args), container.Args, tc.wantModel)
			}
		})
	}
}

// TestVLLMServedModelNameKeepsTheSource pins the compatibility half of #1988:
// vLLM answered to its positional source before it was given a served name,
// so clients may have saved that id. It stays a second name, and is not
// repeated when it equals the modelRef.
func TestVLLMServedModelNameKeepsTheSource(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "glm-flash", Namespace: testBuilderNs},
		Spec:       inferencev1alpha1.ModelSpec{Source: "hf://org/glm-flash"},
	}
	cases := []struct {
		name, modelRef, modelPath string
		want                      []string
	}{
		{"cached path", "glm-flash", "/models/a2df004d073e3716", []string{"glm-flash", "/models/a2df004d073e3716"}},
		{"served from the hub", "glm-flash", "", []string{"glm-flash", "org/glm-flash"}},
		{"source equals the modelRef", "org/glm-flash", "", []string{"org/glm-flash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-svc", Namespace: testBuilderNs},
				Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeVLLM, ModelRef: tc.modelRef},
			}
			args := (&VLLMBackend{}).BuildArgs(isvc, model, tc.modelPath, "", 8000)
			if got := fakeEngineNames(args); !slices.Equal(got, tc.want) {
				t.Errorf("vLLM serves %q, want %q (args %q)", got, tc.want, args)
			}
		})
	}
}
