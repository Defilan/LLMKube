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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent/anthropic"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// The loop is provider-agnostic: it drives a ChatCompleter, and
// anthropic.Client is one implementation of that seam (#1627). These
// tests run the REAL loop against a real anthropic.Client pointed at an
// httptest server that speaks the Anthropic SSE wire, and assert both
// sides of the contract: what the loop does with the parsed response
// (transcript shape, tool dispatch, terminal) and what the client puts
// on the wire (x-api-key header, system hoisted, tool_result riding in
// a user message). The oai-based loop tests (loop_test.go) prove the
// same loop logic over the other wire; this proves the seam holds.

// anthReq mirrors the anthropic request body the client sends, just
// enough to assert the wire shape. The real structs live unexported in
// the anthropic package.
type anthReq struct {
	Model     string     `json:"model"`
	MaxTokens int        `json:"max_tokens"`
	System    string     `json:"system"`
	Messages  []anthMsg  `json:"messages"`
	Tools     []anthTool `json:"tools"`
	Stream    bool       `json:"stream"`
	Temp      *float64   `json:"temperature"`
}

type anthMsg struct {
	Role    string      `json:"role"`
	Content []anthBlock `json:"content"`
}

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type anthTool struct {
	Name string `json:"name"`
}

// anthServer is an httptest server that records every request it
// receives and answers each POST /v1/messages with a pre-baked
// Anthropic SSE stream in sequence.
type anthServer struct {
	srv   *httptest.Server
	t     *testing.T
	mu    sync.Mutex
	reqs  []anthReq
	hdrs  []http.Header
	turns int
	// scripts are the SSE bodies for turns 1..N; the last one repeats.
	scripts []string
}

func newAnthServer(t *testing.T, scripts ...string) *anthServer {
	t.Helper()
	s := &anthServer{t: t, scripts: scripts}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: want /v1/messages got %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req anthReq
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("request body is not a parseable anthropic request: %v\n%s", err, body)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.turns++
		s.reqs = append(s.reqs, req)
		s.hdrs = append(s.hdrs, r.Header.Clone())
		i := s.turns - 1
		if i >= len(s.scripts) {
			i = len(s.scripts) - 1
		}
		script := s.scripts[i]
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, script)
	}))
	t.Cleanup(srv.Close)
	s.srv = srv
	return s
}

const (
	// sseReadFile: a tool_use turn whose input arrives as TWO
	// input_json_delta fragments — the loop must see the full args
	// after the client concatenates them.
	sseReadFile = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_rf","name":"read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"README"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":".md\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`
	// sseSubmitGo: a turn with a text block followed by a tool_use
	// block whose input is seeded from content_block_start (no
	// input_json deltas) — the other way tool args can arrive.
	sseSubmitGo = `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","usage":{"input_tokens":20,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"looks good"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
` +
		// Split across lines for the linter's line budget; the seeded
		// input object is what this turn's assertion is about.
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use",` +
		`"id":"tu_sr","name":"submit_result","input":{"verdict":"GO","summary":"ok"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}
`
)

// TestLoop_AnthropicProvider_E2E runs the native agent loop end to end
// over the Anthropic wire: turn 1 calls read_file (args split across
// two stream fragments), turn 2 terminates with submit_result (args
// seeded from content_block_start). Asserts the loop's transcript
// handling plus the exact wire shape the client produced on the second
// request — that's where the tool_result rides in a user message and
// the x-api-key / anthropic-version headers live.
func TestLoop_AnthropicProvider_E2E(t *testing.T) {
	srv := newAnthServer(t, sseReadFile, sseSubmitGo)
	reg := &fakeRegistry{
		schemas: []oai.Tool{
			{Type: "function", Function: oai.ToolSchemaDef{Name: "read_file", Description: "read a file"}},
			{Type: "function", Function: oai.ToolSchemaDef{Name: "submit_result", Description: "terminal"}},
		},
		results: map[string]*ToolResult{
			"read_file":     {Output: map[string]any{"content": "# README\n"}},
			"submit_result": {Terminal: true, Verdict: "GO", Summary: "ok"},
		},
	}
	client := anthropic.New(srv.srv.URL+"/v1", time.Second, 0, anthropic.WithAPIKey("sk-test-123"))
	loop := NewLoop(client, reg, nil)

	res, err := loop.Run(context.Background(), LoopConfig{
		Model: "claude-sonnet-4-6", SystemPrompt: "be brief", UserPrompt: "do the thing", MaxTurns: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Terminal == nil || res.Terminal.Verdict != "GO" {
		t.Fatalf("expected a GO terminal, got %+v", res.Terminal)
	}
	if res.Turns != 2 {
		t.Errorf("turns: want 2 got %d", res.Turns)
	}
	if srv.turns != 2 {
		t.Errorf("server turns: want 2 got %d", srv.turns)
	}

	assertAnthE2ETranscript(t, res)
	assertAnthE2EWireShape(t, srv)
	assertAnthE2EHeaders(t, srv)
}

// assertAnthE2ETranscript checks the loop-side view: message roles, the
// fragmented tool args assembled by the client, and the tool results
// threaded back with their tool_use ids.
func assertAnthE2ETranscript(t *testing.T, res *LoopResult) {
	t.Helper()

	// Transcript: system, user, assistant#1 (tool_use), tool#1,
	// assistant#2 (text + tool_use), tool#2 = 6 messages.
	tr := res.Transcript
	if len(tr) != 6 {
		t.Fatalf("transcript len: want 6 got %d: %+v", len(tr), tr)
	}
	roles := make([]oai.Role, len(tr))
	for i, m := range tr {
		roles[i] = m.Role
	}
	want := []oai.Role{oai.RoleSystem, oai.RoleUser, oai.RoleAssistant, oai.RoleTool, oai.RoleAssistant, oai.RoleTool}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("transcript roles: want %v got %v", want, roles)
		}
	}

	// Turn 1 assistant: the two input_json_delta fragments must have
	// concatenated into complete, valid JSON args.
	a1 := tr[2]
	if len(a1.ToolCalls) != 1 || a1.ToolCalls[0].ID != "tu_rf" || a1.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("assistant#1 tool call wrong: %+v", a1.ToolCalls)
	}
	if a1.ToolCalls[0].Function.Arguments != `{"path":"README.md"}` {
		t.Errorf("fragmented args: want %q got %q", `{"path":"README.md"}`, a1.ToolCalls[0].Function.Arguments)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(a1.ToolCalls[0].Function.Arguments), &args); err != nil {
		t.Errorf("args are not valid JSON after concatenation: %v", err)
	}

	// The tool result must carry the assistant's tool_use id, and its
	// content is the marshaled ToolResult.Output.
	if tr[3].ToolCallID != "tu_rf" || tr[3].Name != "read_file" {
		t.Errorf("tool#1: want tu_rf/read_file got %s/%s", tr[3].ToolCallID, tr[3].Name)
	}
	if !strings.Contains(tr[3].Content, "# README") {
		t.Errorf("tool#1 content: want it to carry the output, got %q", tr[3].Content)
	}

	// Turn 2 assistant: text block + tool_use seeded from
	// content_block_start (no input_json deltas on the wire).
	a2 := tr[4]
	if !strings.Contains(a2.Content, "looks good") {
		t.Errorf("assistant#2 text: want it to carry %q, got %q", "looks good", a2.Content)
	}
	if len(a2.ToolCalls) != 1 || a2.ToolCalls[0].ID != "tu_sr" || a2.ToolCalls[0].Function.Name != "submit_result" {
		t.Fatalf("assistant#2 tool call wrong: %+v", a2.ToolCalls)
	}
	if a2.ToolCalls[0].Function.Arguments != `{"verdict":"GO","summary":"ok"}` {
		t.Errorf("seeded args: want %q got %q", `{"verdict":"GO","summary":"ok"}`, a2.ToolCalls[0].Function.Arguments)
	}
	if tr[5].ToolCallID != "tu_sr" {
		t.Errorf("tool#2: want tu_sr got %s", tr[5].ToolCallID)
	}
}

// assertAnthE2EWireShape checks what the client put on the wire.
func assertAnthE2EWireShape(t *testing.T, srv *anthServer) {
	t.Helper()
	// Wire contract, turn 2 request: system hoisted to the top level,
	// the tool result riding in a USER message as a tool_result block,
	// tools forwarded, streaming on, and the anthropic-specific headers
	// (x-api-key, not Authorization: Bearer; anthropic-version set).
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.reqs) != 2 || len(srv.hdrs) != 2 {
		t.Fatalf("want 2 recorded requests, got %d/%d", len(srv.reqs), len(srv.hdrs))
	}
	req1, req2 := srv.reqs[0], srv.reqs[1]

	if req1.System != "be brief" || req2.System != "be brief" {
		t.Errorf("system must be hoisted to the top level: %q / %q", req1.System, req2.System)
	}
	if req1.Model != "claude-sonnet-4-6" || req2.Model != "claude-sonnet-4-6" {
		t.Errorf("model: %q / %q", req1.Model, req2.Model)
	}
	if req1.MaxTokens <= 0 || req2.MaxTokens <= 0 {
		t.Errorf("max_tokens must be set (required by the Messages API): %d / %d", req1.MaxTokens, req2.MaxTokens)
	}
	if !req1.Stream || !req2.Stream {
		t.Errorf("stream must be true on every request: %v / %v", req1.Stream, req2.Stream)
	}
	for i, name := range []string{"read_file", "submit_result"} {
		if len(req2.Tools) != 2 || req2.Tools[i].Name != name {
			t.Errorf("tools on turn 2: want [read_file submit_result] got %+v", req2.Tools)
		}
	}

	// Turn 1 request: a single user message with the task text.
	if len(req1.Messages) != 1 || req1.Messages[0].Role != "user" ||
		len(req1.Messages[0].Content) != 1 || req1.Messages[0].Content[0].Type != "text" ||
		req1.Messages[0].Content[0].Text != "do the thing" {
		t.Errorf("turn 1 request messages wrong: %+v", req1.Messages)
	}

	// Turn 2 request: user(task), assistant(tool_use), user(tool_result)
	// in strict alternation — no "tool" role anywhere on the wire.
	if len(req2.Messages) != 3 {
		t.Fatalf("turn 2 request messages: want 3 (user, assistant, user) got %d: %+v", len(req2.Messages), req2.Messages)
	}
	if req2.Messages[0].Role != "user" || req2.Messages[1].Role != "assistant" || req2.Messages[2].Role != "user" {
		t.Errorf("turn 2 roles: want [user assistant user] got [%s %s %s]",
			req2.Messages[0].Role, req2.Messages[1].Role, req2.Messages[2].Role)
	}
	asstBlocks := req2.Messages[1].Content
	if len(asstBlocks) != 1 || asstBlocks[0].Type != "tool_use" || asstBlocks[0].ID != "tu_rf" {
		t.Errorf("turn 2 assistant must carry the tool_use block: %+v", asstBlocks)
	}
	trBlocks := req2.Messages[2].Content
	if len(trBlocks) != 1 || trBlocks[0].Type != "tool_result" || trBlocks[0].ToolUseID != "tu_rf" {
		t.Fatalf("turn 2 user must carry a tool_result block: %+v", trBlocks)
	}
	if !strings.Contains(string(trBlocks[0].Content), "# README") {
		t.Errorf("tool_result content: want it to carry the tool output, got %s", trBlocks[0].Content)
	}
}

// assertAnthE2EHeaders checks the Messages API auth contract: x-api-key
// carries the credential, Authorization stays empty, and the version
// header is present.
func assertAnthE2EHeaders(t *testing.T, srv *anthServer) {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	h := srv.hdrs[0]
	if h.Get("x-api-key") != "sk-test-123" {
		t.Errorf("x-api-key: want sk-test-123 got %q", h.Get("x-api-key"))
	}
	if h.Get("Authorization") != "" {
		t.Errorf("anthropic client must never send Authorization, got %q", h.Get("Authorization"))
	}
	if h.Get("anthropic-version") == "" {
		t.Errorf("anthropic-version header missing: %v", h)
	}
}
