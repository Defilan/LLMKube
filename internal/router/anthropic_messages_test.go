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

package router

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const anthropicBody = `{"model":"any","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

func TestProxyForwardsAnthropicMessages(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			_, mux, back := budgetTestProxy(t, nil)
			r := budgetPostPath(t, mux, path, anthropicBody, nil)
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", r.StatusCode)
			}
			if got := back.LastPath(); got != path {
				t.Errorf("upstream path = %q, want %q", got, path)
			}
		})
	}
}

// A budgeted Anthropic stream is charged from its own usage events, with the
// cached input added back, and its body is never given OpenAI's
// stream_options.
func TestProxyAnthropicStreamChargedWithoutStreamOptions(t *testing.T) {
	proxy, mux, back := budgetTestProxy(t, []Budget{
		{Name: "router-cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1000000},
	})
	back.stream.Store(true)
	script := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":50,\"output_tokens\":40,\"cache_read_input_tokens\":10}}",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}",
	}
	back.sseScript.Store(&script)

	resp := budgetStreamPostPath(t, mux, "/v1/messages", `{"model":"any","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(back.LastBody(), "stream_options") {
		t.Errorf("upstream body = %s, want no stream_options on an Anthropic request", back.LastBody())
	}
	if snap := proxy.budgets.Snapshot(); len(snap) != 1 || snap[0].UsedTokens != 100 {
		t.Fatalf("snapshot = %+v, want UsedTokens 100 (50 input + 10 cached + 40 output)", snap)
	}
}

func TestProxyAnthropicMessageCharged(t *testing.T) {
	proxy, mux, back := budgetTestProxy(t, []Budget{
		{Name: "router-cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1000000},
	})
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":55,"output_tokens":40,"cache_creation_input_tokens":5}}`
	back.body.Store(&body)
	r := budgetPostPath(t, mux, "/v1/messages", anthropicBody, nil)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	if snap := proxy.budgets.Snapshot(); len(snap) != 1 || snap[0].UsedTokens != 100 {
		t.Fatalf("snapshot = %+v, want UsedTokens 100 (55 input + 5 cache write + 40 output)", snap)
	}
}

// The Anthropic API reports input in message_start and output in
// message_delta; vLLM puts both in the final message_delta. Either adds up.
func TestAnthropicUsageFromSSE(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":20,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":30}}` + "\n\n"
	prompt, completion, ok := AnthropicUsageTokensFromSSE([]byte(sse))
	if !ok || prompt != 120 || completion != 30 {
		t.Errorf("usage = %d/%d ok=%v, want 120/30 true", prompt, completion, ok)
	}
	if _, _, ok := AnthropicUsageTokensFromSSE([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")); ok {
		t.Error("a stream with no usage reported ok")
	}
}

// Errors the router writes itself on an Anthropic route use Anthropic's shape;
// the OpenAI routes keep theirs.
func TestProxyAnthropicRouteErrorShape(t *testing.T) {
	cfg := &Config{
		Backends:     []Backend{{Name: "local", Tier: "local", Address: "http://127.0.0.1:1"}},
		DefaultRoute: "local",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Mount(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	post := func(path string) (int, map[string]any) {
		resp, err := http.Post(front.URL+path, "application/json", strings.NewReader(anthropicBody))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return resp.StatusCode, got
	}

	status, got := post("/v1/messages")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	errObj, _ := got["error"].(map[string]any)
	if got["type"] != "error" || errObj["type"] != "api_error" || errObj["message"] == "" {
		t.Errorf("body = %v, want {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":...}}", got)
	}

	_, got = post("/v1/chat/completions")
	if errObj, _ := got["error"].(map[string]any); errObj["code"] == nil {
		t.Errorf("chat completions error = %v, want the OpenAI-style code field kept", got)
	}
}

// A backend that does not serve the Messages API answers 404: the client gets
// that 404 as is, and the backend stays healthy for its other routes.
func TestProxyAnthropicBackend404PassesThrough(t *testing.T) {
	proxy, mux, back := budgetTestProxy(t, nil)
	back.status.Store(http.StatusNotFound)
	notFound := `{"detail":"Not Found"}`
	back.body.Store(&notFound)
	r := budgetPostPath(t, mux, "/v1/messages", anthropicBody, nil)
	got, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound || string(got) != notFound {
		t.Fatalf("got %d %s, want the backend's own 404", r.StatusCode, got)
	}
	if !proxy.disp.IsHealthy("local-qwen") {
		t.Error("a 404 from the backend marked it unhealthy")
	}
}
