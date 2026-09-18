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
	"errors"
	"fmt"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// translateRequest maps an oai.ChatRequest to the Anthropic Messages
// wire shape. The mapping is lossy in exactly the ways the Anthropic
// schema forces:
//
//   - Every system message is hoisted to the top-level system field
//     (the messages array carries only user/assistant), joined with a
//     blank line.
//   - tool messages become tool_result blocks inside a *user* message
//     (there is no tool role).
//   - An assistant message's ReasoningContent is dropped (see
//     assistantContentBlocks).
//   - ChatTemplateKwargs is ignored: it is an OpenAI/llama.cpp
//     chat-template knob (e.g. toggling think blocks on llama.cpp
//     serves). Anthropic has no counterpart — thinking is server-side
//     per model — so forwarding the field would be a lie.
//
// The merge pass at the end folds consecutive same-role messages into
// one. That is required, not cosmetic: the loop naturally produces
// same-role runs (a batch of tool results, or a tool batch followed by
// a corrective nudge), and Anthropic's strict alternation would 400 on
// them.
func translateRequest(req oai.ChatRequest) (*request, error) {
	out := &request{
		Model:       req.Model,
		MaxTokens:   maxTokensFor(req.MaxTokens),
		Temperature: req.Temperature,
		Stream:      true,
	}

	var systems []string
	var msgs []message
	for _, m := range req.Messages {
		switch m.Role {
		case oai.RoleSystem:
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			systems = append(systems, m.Content)
		case oai.RoleUser:
			if blocks := userContentBlocks(m); len(blocks) > 0 {
				msgs = append(msgs, message{Role: roleUser, Content: blocks})
			}
		case oai.RoleTool:
			// No tool role on this wire: the result rides in a user
			// message as a tool_result block that threads the tool_use
			// id back to the assistant's call.
			msgs = append(msgs, message{
				Role: roleUser,
				Content: []contentBlock{{
					Type:      blockTypeToolResult,
					ToolUseID: m.ToolCallID,
					Content:   m.Content,
				}},
			})
		case oai.RoleAssistant:
			blocks, err := assistantContentBlocks(m)
			if err != nil {
				return nil, err
			}
			if len(blocks) > 0 {
				msgs = append(msgs, message{Role: roleAssistant, Content: blocks})
			}
		default:
			return nil, fmt.Errorf("anthropic: translate: unsupported message role %q", m.Role)
		}
	}
	out.System = strings.Join(systems, "\n\n")

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}

	msgs = mergeConsecutiveRoles(msgs)
	if len(msgs) == 0 {
		return nil, errors.New("anthropic: translate: no user or assistant messages to send")
	}
	// The Messages API requires the conversation to open with a user
	// message. The loop's transcript always does (system hoists out and
	// the first remaining message is the task prompt); this check turns
	// a protocol violation into a clean client-side error instead of a
	// 400 deep in the provider.
	if msgs[0].Role != roleUser {
		return nil, errors.New("anthropic: translate: first message must be a user message")
	}
	out.Messages = msgs
	return out, nil
}

// maxTokensFor maps the caller's MaxTokens to the wire value. The
// Messages API requires max_tokens on every request, so zero (the oai
// convention for "let the server decide") maps to DefaultMaxTokens
// instead of being dropped.
func maxTokensFor(n int) int {
	if n <= 0 {
		return DefaultMaxTokens
	}
	return n
}

// userContentBlocks maps a user message to content blocks. A message
// with Parts emits one block per part (text and image_url); otherwise
// its Content is a single text block. A user message with neither is
// dropped by the caller (an empty message carries no signal on either
// wire).
func userContentBlocks(m oai.Message) []contentBlock {
	if len(m.Parts) > 0 {
		blocks := make([]contentBlock, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch p.Type {
			case oai.ContentPartText:
				blocks = append(blocks, contentBlock{Type: blockTypeText, Text: p.Text})
			case oai.ContentPartImageURL:
				if p.ImageURL != nil {
					blocks = append(blocks, contentBlock{
						Type:   blockTypeImage,
						Source: imageSourceFor(p.ImageURL.URL),
					})
				}
			}
		}
		return blocks
	}
	if strings.TrimSpace(m.Content) == "" {
		return nil
	}
	return []contentBlock{{Type: blockTypeText, Text: m.Content}}
}

// imageSourceFor maps an OpenAI image_url value to an Anthropic image
// source. A data: URL is inlined as base64 (the form the loop's image
// tool emits — self-contained, no provider fetch); any other URL is
// passed through as a remote URL source.
func imageSourceFor(u string) *imageSource {
	if mediaType, data, ok := parseDataURL(u); ok {
		return &imageSource{Type: imageSourceBase64, MediaType: mediaType, Data: data}
	}
	return &imageSource{Type: imageSourceURL, URL: u}
}

// parseDataURL splits "data:<mediatype>;base64,<payload>" into its
// media type and base64 payload. ok is false for anything that is not
// a base64 data URL (plain URLs, or data URLs without a base64
// marker).
func parseDataURL(u string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(u, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(u, prefix)
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	data = rest[comma+1:]
	// meta is "<mediatype>;base64" (the mediatype may be empty).
	if i := strings.LastIndexByte(meta, ';'); i >= 0 {
		if meta[i+1:] != "base64" {
			return "", "", false
		}
		return meta[:i], data, true
	}
	return "", "", false
}

// assistantContentBlocks maps an assistant message to content blocks: a
// text block (when Content is non-empty) followed by one tool_use block
// per ToolCall, in slice order.
//
// ReasoningContent is dropped on purpose. Anthropic's thinking blocks
// must be echoed back carrying the provider's signature to be accepted,
// and neither a third-party Anthropic-compatible endpoint nor the
// loop's own placeholder text can produce one — replaying unsigned
// thinking is a guaranteed 400. The loop already strips reasoning from
// history on the OpenAI wire (stripReasoningForWire); dropping it here
// keeps both paths honest instead of poisoning every later turn.
func assistantContentBlocks(m oai.Message) ([]contentBlock, error) {
	var blocks []contentBlock
	if strings.TrimSpace(m.Content) != "" {
		blocks = append(blocks, contentBlock{Type: blockTypeText, Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		input, err := toolUseInput(tc.Function.Arguments)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, contentBlock{
			Type:  blockTypeToolUse,
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}
	return blocks, nil
}

// toolUseInput parses a tool_call's Arguments JSON string into the
// object the Anthropic input field expects. An empty string becomes the
// empty object (the loop dispatches "" as {}); an unparseable or
// non-object value is a hard error. Unlike the stream-side validation
// (which wraps oai.ErrTruncatedToolCallArguments for retry), this one
// is deterministic in the transcript — retrying the same request cannot
// fix it — so it is deliberately not retryable.
func toolUseInput(args string) (json.RawMessage, error) {
	if args == "" {
		return json.RawMessage("{}"), nil
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(args), &probe); err != nil {
		return nil, fmt.Errorf("anthropic: tool_use arguments are not a JSON object: %w", err)
	}
	return json.RawMessage(args), nil
}

// mergeConsecutiveRoles folds adjacent same-role messages into one by
// concatenating their content blocks, and drops messages left with no
// blocks. See the translateRequest doc for why Anthropic requires this
// (strict user/assistant alternation).
func mergeConsecutiveRoles(msgs []message) []message {
	out := make([]message, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Content) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Content = append(out[n-1].Content, m.Content...)
			continue
		}
		out = append(out, m)
	}
	return out
}
