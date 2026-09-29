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

package ingress

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type route struct{ method, path string }

var commonRoutes = []route{
	{"GET", "/health"},
	{"GET", "/v1/models"},
	{"GET", "/v1/models/my-model"},
	{"GET", "/v1/models/llama3:8b"},
	{"POST", "/v1/chat/completions"},
	{"POST", "/v1/completions"},
	{"POST", "/v1/embeddings"},
	{"POST", "/v1/responses"},
	{"GET", "/metrics"},
}

var llamaCPPRoutes = []route{
	{"POST", "/completion"},
	{"POST", "/completions"},
	{"POST", "/tokenize"},
	{"POST", "/detokenize"},
	{"POST", "/apply-template"},
	{"POST", "/embedding"},
	{"POST", "/embeddings"},
	{"POST", "/infill"},
	{"POST", "/rerank"},
	{"POST", "/reranking"},
	{"POST", "/v1/rerank"},
	{"POST", "/v1/messages"},
	{"POST", "/v1/messages/count_tokens"},
	{"GET", "/props"},
	{"GET", "/models"},
}

var additions = map[string][]route{
	"llama-server": llamaCPPRoutes,
	"llamacpp":     llamaCPPRoutes,
	inferencev1alpha1.RuntimeVLLMSwift: {
		{"POST", "/tokenize"},
		{"POST", "/detokenize"},
		{"GET", "/version"},
		{"GET", "/ping"},
		{"POST", "/v1/messages"},
		{"POST", "/pooling"},
		{"POST", "/score"},
		{"POST", "/v1/score"},
		{"POST", "/rerank"},
		{"POST", "/v1/rerank"},
		{"POST", "/v2/rerank"},
	},
	inferencev1alpha1.RuntimeOllama: {
		{"GET", "/"},
		{"POST", "/api/chat"},
		{"POST", "/api/generate"},
		{"POST", "/api/embed"},
		{"POST", "/api/embeddings"},
		{"GET", "/api/tags"},
		{"POST", "/api/show"},
		{"GET", "/api/version"},
		{"GET", "/api/ps"},
	},
	inferencev1alpha1.RuntimeMLXServer:  nil,
	inferencev1alpha1.RuntimeTensorFold: nil,
	inferencev1alpha1.RuntimeOMLX:       nil,
}

func TestAllowedPositive(t *testing.T) {
	for rt, extra := range additions {
		for _, r := range append(append([]route{}, commonRoutes...), extra...) {
			if !Allowed(rt, r.method, r.path) {
				t.Errorf("Allowed(%q, %q, %q) = false, want true", rt, r.method, r.path)
			}
		}
	}
}

func TestAllowedQueryIgnored(t *testing.T) {
	if !Allowed("llama-server", "GET", "/health?x=1") {
		t.Error("query string should be ignored")
	}
	if !Allowed(inferencev1alpha1.RuntimeOllama, "GET", "/api/tags?") {
		t.Error("empty query string should be ignored")
	}
}

func TestAllowedAdditionsAreRuntimeScoped(t *testing.T) {
	// Every addition must be denied for runtimes that do not list it.
	for rt, extra := range additions {
		for _, r := range extra {
			for other, otherExtra := range additions {
				if other == rt || containsRoute(otherExtra, r) || containsRoute(commonRoutes, r) {
					continue
				}
				if Allowed(other, r.method, r.path) {
					t.Errorf("Allowed(%q, %q, %q) = true; route belongs only to %q", other, r.method, r.path, rt)
				}
			}
		}
	}
}

func containsRoute(rs []route, r route) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

func TestAllowedNegative(t *testing.T) {
	for _, llama := range []string{"llama-server", "llamacpp"} {
		for _, tc := range []struct{ method, path string }{
			{"GET", "/slots"},
			{"POST", "/slots/0?action=save"},
			{"POST", "/props"},
			{"POST", "/lora-adapters"},
			{"GET", "/v1/../slots"},
			{"GET", "//slots"},
			{"GET", "/SLOTS"},
			{"GET", "/%2e%2e/slots"},
			{"GET", "/v1/models/a/b"},
			{"GET", "/health/"},
			{"GET", "/./health"},
			{"GET", "health"},
			{"GET", ""},
			{"GET", "/v1/models/"},
			{"GET", "/v1/models/a%2Fb"},
			{"GET", "/v1/models/%2e%2e"},
			{"GET", "/v1/models/.."},
			{"GET", "/v1/models/."},
			{"GET", "/v1/models/a%25b"},
			{"GET", "/v1/models/a%5Cb"},
			{"GET", "/v1/models/a\\b"},
			{"GET", "/v1/models/a;b"},
			{"GET", "/v1/models/a%3Bb"},
			{"GET", "/v1/models/a%3Fb"},
			{"GET", "/%zz"},
			{"GET", "/Health"},
			{"get", "/health"},
			{"HEAD", "/health"},
		} {
			if Allowed(llama, tc.method, tc.path) {
				t.Errorf("Allowed(%q, %q, %q) = true, want false", llama, tc.method, tc.path)
			}
		}
	}
	vllm := inferencev1alpha1.RuntimeVLLMSwift
	ollama := inferencev1alpha1.RuntimeOllama
	cases := []struct{ rt, method, path string }{
		{vllm, "POST", "/sleep"},
		{vllm, "POST", "/wake_up"},
		{vllm, "POST", "/reset_prefix_cache"},
		{vllm, "POST", "/collective_rpc"},
		{vllm, "POST", "/v1/load_lora_adapter"},
		{vllm, "POST", "/start_profile"},
		{ollama, "POST", "/api/pull"},
		{ollama, "POST", "/api/push"},
		{ollama, "DELETE", "/api/delete"},
		{ollama, "POST", "/api/copy"},
		{ollama, "POST", "/api/create"},
		{ollama, "POST", "/api/blobs/sha256:x"},
		{ollama, "HEAD", "/api/blobs/sha256:x"},
		{inferencev1alpha1.RuntimeOMLX, "POST", "/v1/models/x/unload"},
		{"unknown-runtime", "GET", "/health"},
		{"", "GET", "/health"},
		{ollama, "GET", "//"},
	}
	for _, tc := range cases {
		if Allowed(tc.rt, tc.method, tc.path) {
			t.Errorf("Allowed(%q, %q, %q) = true, want false", tc.rt, tc.method, tc.path)
		}
	}
	// Wrong method on every common POST route, for every known runtime.
	for rt := range additions {
		if Allowed(rt, "GET", "/v1/chat/completions") {
			t.Errorf("Allowed(%q, GET, /v1/chat/completions) = true, want false", rt)
		}
	}
}
