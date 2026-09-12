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

package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

func mustTranslate(t *testing.T, req oai.ChatRequest) *request {
	t.Helper()
	out, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	return out
}

func TestTranslate_SystemHoistedAndJoined(t *testing.T) {
	out := mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleSystem, Content: "first"},
			{Role: oai.RoleSystem, Content: "   "},
			{Role: oai.RoleSystem, Content: "second"},
			{Role: oai.RoleUser, Content: "hi"},
		},
	})
	if out.System != "first\n\nsecond" {
		t.Errorf("system: want hoisted+joined with blank line, got %q", out.System)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != roleUser {
		t.Fatalf("want a single user message left, got %+v", out.Messages)
	}
}

func TestTranslate_MaxTokens(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{
		{0, DefaultMaxTokens}, // zero maps to the default, never dropped
		{1, 1},
		{8192, 8192},
	} {
		out := mustTranslate(t, oai.ChatRequest{
			Model:     "m",
			MaxTokens: tc.in,
			Messages:  []oai.Message{{Role: oai.RoleUser, Content: "x"}},
		})
		if out.MaxTokens != tc.want {
			t.Errorf("MaxTokens in=%d: want %d got %d", tc.in, tc.want, out.MaxTokens)
		}
	}
}

func TestTranslate_Temperature(t *testing.T) {
	out := mustTranslate(t, oai.ChatRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: "x"}},
	})
	if out.Temperature != nil {
		t.Errorf("nil temperature must stay nil, got %v", *out.Temperature)
	}

	v := 0.7
	out = mustTranslate(t, oai.ChatRequest{
		Model:       "m",
		Temperature: &v,
		Messages:    []oai.Message{{Role: oai.RoleUser, Content: "x"}},
	})
	if out.Temperature == nil || *out.Temperature != 0.7 {
		t.Errorf("temperature: want 0.7 got %v", out.Temperature)
	}
}

func TestTranslate_Tools(t *testing.T) {
	params := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	out := mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Tools: []oai.Tool{
			{Type: "function", Function: oai.ToolSchemaDef{Name: "read_file", Description: "reads", Parameters: params}},
			{Type: "function", Function: oai.ToolSchemaDef{Name: "submit_result"}},
		},
		Messages: []oai.Message{{Role: oai.RoleUser, Content: "x"}},
	})
	if len(out.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(out.Tools))
	}
	if out.Tools[0].Name != "read_file" || out.Tools[0].Description != "reads" ||
		string(out.Tools[0].InputSchema) != string(params) {
		t.Errorf("tool 0 mismatch: %+v", out.Tools[0])
	}
	if out.Tools[1].Name != "submit_result" || out.Tools[1].InputSchema != nil {
		t.Errorf("tool 1 must drop the empty schema: %+v", out.Tools[1])
	}

	// No tools advertised -> key absent from the wire entirely.
	body := mustJSON(t, mustTranslate(t, oai.ChatRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: "x"}},
	}))
	if strings.Contains(body, `"tools"`) {
		t.Errorf("tools key must be omitted when empty: %s", body)
	}
}

func TestTranslate_StreamAlwaysOn(t *testing.T) {
	out := mustTranslate(t, oai.ChatRequest{
		Model:    "m",
		Stream:   false,
		Messages: []oai.Message{{Role: oai.RoleUser, Content: "x"}},
	})
	if !out.Stream {
		t.Error("stream must be forced true")
	}
}

func TestTranslate_ChatTemplateKwargsIgnored(t *testing.T) {
	body := mustJSON(t, mustTranslate(t, oai.ChatRequest{
		Model:              "m",
		ChatTemplateKwargs: map[string]apiextensionsv1.JSON{"enable_thinking": {Raw: []byte("false")}},
		Messages:           []oai.Message{{Role: oai.RoleUser, Content: "x"}},
	}))
	if strings.Contains(body, "chat_template_kwargs") || strings.Contains(body, "enable_thinking") {
		t.Errorf("chat template kwargs must not leak onto the Anthropic wire: %s", body)
	}
}

func TestTranslate_UserContentShapes(t *testing.T) {
	out := mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: "plain text"},
		},
	})
	if len(out.Messages[0].Content) != 1 || out.Messages[0].Content[0].Type != blockTypeText ||
		out.Messages[0].Content[0].Text != "plain text" {
		t.Errorf("plain user: %+v", out.Messages[0].Content)
	}

	out = mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{
				Role: oai.RoleUser,
				Parts: []oai.ContentPart{
					{Type: oai.ContentPartText, Text: "look at this"},
					{Type: oai.ContentPartImageURL, ImageURL: &oai.ImageURL{URL: "data:image/png;base64,AAA"}},
					{Type: oai.ContentPartImageURL, ImageURL: &oai.ImageURL{URL: "https://example.com/x.png"}},
				},
			},
		},
	})
	blocks := out.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("want 3 parts blocks, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != blockTypeText || blocks[0].Text != "look at this" {
		t.Errorf("text part: %+v", blocks[0])
	}
	if blocks[1].Type != blockTypeImage || blocks[1].Source == nil ||
		blocks[1].Source.Type != imageSourceBase64 || blocks[1].Source.MediaType != "image/png" ||
		blocks[1].Source.Data != "AAA" {
		t.Errorf("data-url image part: %+v", blocks[1])
	}
	if blocks[2].Type != blockTypeImage || blocks[2].Source == nil ||
		blocks[2].Source.Type != imageSourceURL || blocks[2].Source.URL != "https://example.com/x.png" {
		t.Errorf("remote image part: %+v", blocks[2])
	}

	// An empty user message (no content, no parts) drops out of the
	// messages array entirely: an empty Anthropic user message carries
	// no signal. When that empties out the OPENING message, the
	// conversation would start assistant-side, which the API rejects —
	// so the guard turns it into a clean client-side error.
	_, err := translateRequest(oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: ""},
			{Role: oai.RoleAssistant, Content: "re"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "first message must be a user message") {
		t.Fatalf("want the first-user guard to fire, got %v", err)
	}

	// A later empty user (a dropped nudge) just vanishes; the
	// conversation still opens with a user, so no error.
	out = mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: "task"},
			{Role: oai.RoleAssistant, Content: "re"},
			{Role: oai.RoleUser, Content: ""},
		},
	})
	if len(out.Messages) != 2 || out.Messages[1].Role != roleAssistant {
		t.Fatalf("empty later user must drop, got %+v", out.Messages)
	}
}

func TestTranslate_ToolResultRidesInUserMessage(t *testing.T) {
	out := mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: "task"},
			{
				Role: oai.RoleAssistant,
				ToolCalls: []oai.ToolCall{{
					ID: "tu_1", Type: "function",
					Function: oai.ToolCallFunction{Name: "read_file", Arguments: `{"path":"a"}`},
				}},
			},
			{Role: oai.RoleTool, ToolCallID: "tu_1", Content: "file body"},
		},
	})
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(out.Messages), out.Messages)
	}
	got := out.Messages[2].Content
	if len(got) != 1 || got[0].Type != blockTypeToolResult ||
		got[0].ToolUseID != "tu_1" || got[0].Content != "file body" {
		t.Errorf("tool result block: %+v", got)
	}
}

func TestTranslate_MergesConsecutiveSameRole(t *testing.T) {
	// A tool batch followed by a corrective nudge: all of it lands in one
	// user message, preserving block order.
	out := mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: "task"},
			{
				Role: oai.RoleAssistant,
				ToolCalls: []oai.ToolCall{
					{ID: "tu_1", Type: "function", Function: oai.ToolCallFunction{Name: "a", Arguments: `{}`}},
					{ID: "tu_2", Type: "function", Function: oai.ToolCallFunction{Name: "b", Arguments: `{}`}},
				},
			},
			{Role: oai.RoleTool, ToolCallID: "tu_1", Content: "r1"},
			{Role: oai.RoleTool, ToolCallID: "tu_2", Content: "r2"},
			{Role: oai.RoleUser, Content: "nudge"},
		},
	})
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages (merged), got %d: %+v", len(out.Messages), out.Messages)
	}
	last := out.Messages[2]
	if last.Role != roleUser || len(last.Content) != 3 {
		t.Fatalf("merged user message: %+v", last)
	}
	if last.Content[0].Type != blockTypeToolResult || last.Content[0].ToolUseID != "tu_1" ||
		last.Content[1].Type != blockTypeToolResult || last.Content[1].ToolUseID != "tu_2" ||
		last.Content[2].Type != blockTypeText || last.Content[2].Text != "nudge" {
		t.Errorf("merged blocks out of order or wrong: %+v", last.Content)
	}
}

func TestTranslate_AssistantTextAndToolUseOrdering(t *testing.T) {
	out := mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: "task"},
			{
				Role:    oai.RoleAssistant,
				Content: "thinking out loud",
				ToolCalls: []oai.ToolCall{
					{ID: "tu_1", Type: "function", Function: oai.ToolCallFunction{Name: "a", Arguments: `{"x":1}`}},
					{ID: "tu_2", Type: "function", Function: oai.ToolCallFunction{Name: "b", Arguments: ""}},
				},
			},
		},
	})
	blocks := out.Messages[1].Content
	if len(blocks) != 3 {
		t.Fatalf("want text + 2 tool_use blocks, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != blockTypeText || blocks[0].Text != "thinking out loud" {
		t.Errorf("text block: %+v", blocks[0])
	}
	if blocks[1].Type != blockTypeToolUse || blocks[1].ID != "tu_1" || blocks[1].Name != "a" ||
		string(blocks[1].Input) != `{"x":1}` {
		t.Errorf("first tool_use: %+v", blocks[1])
	}
	// Empty arguments become the empty object, mirroring the oai dispatch.
	if blocks[2].Type != blockTypeToolUse || blocks[2].ID != "tu_2" ||
		string(blocks[2].Input) != "{}" {
		t.Errorf("second tool_use (empty args): %+v", blocks[2])
	}
}

func TestTranslate_ReasoningDropped(t *testing.T) {
	out := mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: "task"},
			{Role: oai.RoleAssistant, Content: "answer", ReasoningContent: "deep thoughts"},
		},
	})
	blocks := out.Messages[1].Content
	if len(blocks) != 1 || blocks[0].Type != blockTypeText || blocks[0].Text != "answer" {
		t.Errorf("reasoning must not reach the wire: %+v", blocks)
	}

	// A reasoning-only assistant message (a paused turn) carries no
	// Anthropic content at all and drops out of the messages array,
	// which also merges the user messages on either side into one —
	// the strict-alternation pass sees two consecutive users.
	out = mustTranslate(t, oai.ChatRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: "task"},
			{Role: oai.RoleAssistant, ReasoningContent: "deep thoughts"},
			{Role: oai.RoleUser, Content: "nudge"},
		},
	})
	if len(out.Messages) != 1 || out.Messages[0].Role != roleUser || len(out.Messages[0].Content) != 2 {
		t.Fatalf("expected the two users to merge, got %+v", out.Messages)
	}
}

func TestTranslate_Errors(t *testing.T) {
	cases := []struct {
		name    string
		req     oai.ChatRequest
		wantSub string
	}{
		{
			name: "malformed tool args",
			req: oai.ChatRequest{
				Messages: []oai.Message{
					{Role: oai.RoleUser, Content: "x"},
					{
						Role: oai.RoleAssistant,
						ToolCalls: []oai.ToolCall{{
							ID: "t", Type: "function",
							Function: oai.ToolCallFunction{Name: "a", Arguments: `{"x":}`},
						}},
					},
				},
			},
			wantSub: "not a JSON object",
		},
		{
			name: "non-object tool args",
			req: oai.ChatRequest{
				Messages: []oai.Message{
					{Role: oai.RoleUser, Content: "x"},
					{
						Role: oai.RoleAssistant,
						ToolCalls: []oai.ToolCall{{
							ID: "t", Type: "function",
							Function: oai.ToolCallFunction{Name: "a", Arguments: `[1,2]`},
						}},
					},
				},
			},
			wantSub: "not a JSON object",
		},
		{
			name:    "unknown role",
			req:     oai.ChatRequest{Messages: []oai.Message{{Role: oai.Role("gpt"), Content: "x"}}},
			wantSub: "unsupported message role",
		},
		{
			name: "assistant opens the conversation",
			req: oai.ChatRequest{
				Messages: []oai.Message{
					{Role: oai.RoleSystem, Content: "sys"},
					{Role: oai.RoleAssistant, Content: "re"},
				},
			},
			wantSub: "first message must be a user message",
		},
		{
			name:    "only system messages",
			req:     oai.ChatRequest{Messages: []oai.Message{{Role: oai.RoleSystem, Content: "sys"}}},
			wantSub: "no user or assistant messages",
		},
		{
			name:    "nothing at all",
			req:     oai.ChatRequest{Messages: []oai.Message{}},
			wantSub: "no user or assistant messages",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := translateRequest(tc.req)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q: want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestParseDataURL(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantType string
		wantData string
		wantOK   bool
	}{
		{"data:image/png;base64,AAA", "image/png", "AAA", true},
		{"data:;base64,AAA", "", "AAA", true},
		{"data:text/plain,hello", "", "", false}, // no base64 marker
		{"data:", "", "", false},
		{"https://example.com/x.png", "", "", false},
	} {
		gotType, gotData, gotOK := parseDataURL(tc.in)
		if gotType != tc.wantType || gotData != tc.wantData || gotOK != tc.wantOK {
			t.Errorf("parseDataURL(%q) = (%q, %q, %v); want (%q, %q, %v)",
				tc.in, gotType, gotData, gotOK, tc.wantType, tc.wantData, tc.wantOK)
		}
	}
}

func TestMaxTokensFor(t *testing.T) {
	if got := maxTokensFor(0); got != DefaultMaxTokens {
		t.Errorf("zero: want %d got %d", DefaultMaxTokens, got)
	}
	if got := maxTokensFor(1234); got != 1234 {
		t.Errorf("passthrough: want 1234 got %d", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
