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
	"net/http"
	"net/url"
	"path"
	"strings"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// runtimeLlamaServer and runtimeLlamaCPP mirror pkg/agent's unexported
// constants of the same names; that package will import this one, so they
// cannot be referenced directly. "llamacpp" is the canonical value the CRD
// emits and "llama-server" the agent's historical key; both select the same
// llama.cpp executor and therefore share one allowlist.
const (
	runtimeLlamaServer = "llama-server"
	runtimeLlamaCPP    = "llamacpp"
)

// modelsPrefix is the one parameterized route: GET /v1/models/{id} with
// exactly one non-empty segment.
const modelsPrefix = "/v1/models/"

type routeKey struct{ method, path string }

func routes(pairs ...string) map[routeKey]struct{} {
	m := make(map[routeKey]struct{}, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[routeKey{pairs[i], pairs[i+1]}] = struct{}{}
	}
	return m
}

var commonAllowed = routes(
	http.MethodGet, "/health",
	http.MethodGet, "/v1/models",
	http.MethodPost, "/v1/chat/completions",
	http.MethodPost, "/v1/completions",
	http.MethodPost, "/v1/embeddings",
	http.MethodPost, "/v1/responses",
	http.MethodGet, "/metrics",
)

// llamaCPPAllowed is the llama.cpp addition set, shared by both of its
// runtime names.
var llamaCPPAllowed = routes(
	http.MethodPost, "/completion",
	http.MethodPost, "/completions",
	http.MethodPost, "/tokenize",
	http.MethodPost, "/detokenize",
	http.MethodPost, "/apply-template",
	http.MethodPost, "/embedding",
	http.MethodPost, "/embeddings",
	http.MethodPost, "/infill",
	http.MethodPost, "/rerank",
	http.MethodPost, "/reranking",
	http.MethodPost, "/v1/rerank",
	http.MethodPost, "/v1/messages",
	http.MethodPost, "/v1/messages/count_tokens",
	http.MethodGet, "/props",
	http.MethodGet, "/models",
)

// runtimeAllowed pins each runtime's additions to the common set. A runtime
// absent from this map is allowed nothing, including the common routes.
var runtimeAllowed = map[string]map[routeKey]struct{}{
	runtimeLlamaServer: llamaCPPAllowed,
	runtimeLlamaCPP:    llamaCPPAllowed,
	inferencev1alpha1.RuntimeVLLMSwift: routes(
		http.MethodPost, "/tokenize",
		http.MethodPost, "/detokenize",
		http.MethodGet, "/version",
		http.MethodGet, "/ping",
		http.MethodPost, "/v1/messages",
		http.MethodPost, "/pooling",
		http.MethodPost, "/score",
		http.MethodPost, "/v1/score",
		http.MethodPost, "/rerank",
		http.MethodPost, "/v1/rerank",
		http.MethodPost, "/v2/rerank",
	),
	inferencev1alpha1.RuntimeOllama: routes(
		http.MethodGet, "/",
		http.MethodPost, "/api/chat",
		http.MethodPost, "/api/generate",
		http.MethodPost, "/api/embed",
		http.MethodPost, "/api/embeddings",
		http.MethodGet, "/api/tags",
		http.MethodPost, "/api/show",
		http.MethodGet, "/api/version",
		http.MethodGet, "/api/ps",
	),
	inferencev1alpha1.RuntimeMLXServer:  {},
	inferencev1alpha1.RuntimeTensorFold: {},
	inferencev1alpha1.RuntimeOMLX:       {},
}

// Allowed reports whether the ingress may forward method + rawPath to an
// engine of the given runtime. rawPath is the request's URL.EscapedPath();
// any query string is ignored. The path is percent-decoded and must already
// be canonical (equal to path.Clean of itself and absolute): "..", ".", "//"
// and trailing-slash variants are rejected, never cleaned. Matching is exact
// and case-sensitive.
func Allowed(runtime, method, rawPath string) bool {
	extra, ok := runtimeAllowed[runtime]
	if !ok {
		return false
	}
	p, ok := canonicalPath(rawPath)
	if !ok {
		return false
	}
	k := routeKey{method, p}
	if _, ok := commonAllowed[k]; ok {
		return true
	}
	if _, ok := extra[k]; ok {
		return true
	}
	return method == http.MethodGet && isModelIDPath(p)
}

// canonicalPath decodes and checks rawPath. ServeHTTP passes
// URL.EscapedPath, which never contains a query (a literal '?' in the path is
// escaped as %3F); the '?' cut only serves callers that pass a raw
// request-target, and is kept so Allowed's "any query string is ignored"
// contract holds for them.
func canonicalPath(rawPath string) (string, bool) {
	if i := strings.IndexByte(rawPath, '?'); i >= 0 {
		rawPath = rawPath[:i]
	}
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", false
	}
	if !strings.HasPrefix(decoded, "/") || decoded != path.Clean(decoded) {
		return "", false
	}
	return decoded, true
}

// isModelIDPath matches /v1/models/{id}: one non-empty segment with no
// further slash and none of ? # % \ ; or control characters, any of which
// could change the path's meaning to an upstream that decodes or splits it
// differently. p is
// already canonical, so "." and ".." segments cannot reach here.
func isModelIDPath(p string) bool {
	id, ok := strings.CutPrefix(p, modelsPrefix)
	if !ok || id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c == '/' || c == '?' || c == '#' || c == '%' || c == '\\' || c == ';' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
