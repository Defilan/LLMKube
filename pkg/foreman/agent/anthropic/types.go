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

// Package anthropic is a minimal client for the Anthropic-native Messages
// API (POST /v1/messages, SSE streaming).
//
// It lives next to the oai package, not inside it, because the two wires
// differ in ways that matter to agent quality, not just plumbing (#1627):
// Messages carries the model's reasoning as a first-class thinking
// channel, carries tool input as structured JSON (tool_use blocks), and
// enforces strict user/assistant alternation in the messages array. An
// OpenAI-compatible translation proxy in front of Anthropic rewrites the
// wire on both sides and degrades or drops the reasoning trace in the
// process; dialing the native endpoint keeps the loop's reasoning-only
// turn and truncation-continuation machinery working against a model that
// actually emits thinking blocks.
//
// As with oai, the official SDK is deliberately not used: Foreman needs
// exactly one HTTP shape, wants provider-specific retry semantics, and
// does not want the SDK's transitive cone.
package anthropic

import "encoding/json"

// APIVersion is the anthropic-version request header value. The Messages
// API is versioned per request; 2023-06-01 is the stable version that
// carries the streaming thinking and tool_use events this client parses.
const APIVersion = "2023-06-01"

// DefaultMaxTokens is the max_tokens value used when the caller leaves
// the field at zero. Unlike the OpenAI surface (where zero means "let the
// server decide"), the Messages API requires the field on every request,
// so zero cannot be dropped and must map to a concrete default. 8192
// leaves room for a thinking-heavy decision turn without pinning current
// model generations at the 8K output ceiling.
const DefaultMaxTokens = 8192

// Role and content-block type values in the request body. Wire literals;
// the request structs are package-private, so these stay private.
const (
	roleUser      = "user"
	roleAssistant = "assistant"

	blockTypeText       = "text"
	blockTypeImage      = "image"
	blockTypeToolUse    = "tool_use"
	blockTypeToolResult = "tool_result"
	blockTypeThinking   = "thinking"

	imageSourceBase64 = "base64"
	imageSourceURL    = "url"
)

// request is the POST /v1/messages body.
type request struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	Messages    []message `json:"messages"`
	System      string    `json:"system,omitempty"`
	Tools       []tool    `json:"tools,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	Stream      bool      `json:"stream"`
}

// message is one entry of the messages array. The schema allows only
// "user" and "assistant" here: system text is hoisted to the top-level
// System field, and tool results ride inside user messages as
// tool_result blocks (there is no tool role).
type message struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// contentBlock is one element of a message's content array. Only the
// fields relevant to Type are populated; the rest stay off the wire.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// imageSource is the source of an image content block: base64 inline
// bytes (from a data: URL) or a remote URL.
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// tool is one entry of the tools array: the Anthropic shape of an
// OpenAI function advertisement. The function's parameters JSONSchema
// maps onto input_schema verbatim.
type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// --- SSE event payloads ---
//
// The Messages stream is standard SSE: each event is an `event:` name
// line plus one or more `data:` lines carrying a JSON object whose
// `type` field duplicates the event name. The parser dispatches on the
// type field; these structs are what each kind decodes into.

// sseMessageStart is the stream's first event: the message header
// carrying the message id (echoed into ChatResponse.ID) and
// usage.input_tokens (the prompt size).
type sseMessageStart struct {
	Type    string     `json:"type"`
	Message sseMessage `json:"message"`
}

// sseMessage is the message object inside message_start.
type sseMessage struct {
	ID    string   `json:"id"`
	Model string   `json:"model"`
	Usage sseUsage `json:"usage"`
}

// sseUsage is the token accounting the API attaches to message_start
// (input) and message_delta (output). The oai-shaped response the
// client returns has no Usage field yet, so the aggregator holds the
// values; they are parsed so a future oai.ChatResponse usage field has
// a ready source.
type sseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// sseContentBlockStart opens one content block of the assistant
// message. The content_block payload names the block kind: text (bytes
// arrive as text_delta), thinking (bytes as thinking_delta), or
// tool_use (id + name up front; the arguments JSON arrives as the
// initial input field and/or input_json_delta pieces).
type sseContentBlockStart struct {
	Type         string   `json:"type"`
	Index        int      `json:"index"`
	ContentBlock sseBlock `json:"content_block"`
}

// sseBlock is the content_block payload of a content_block_start.
type sseBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// sseContentBlockDelta is one incremental piece of an open block. The
// delta's type selects the payload: text_delta (text), thinking_delta
// (thinking), input_json_delta (a fragment of a tool_use arguments
// JSON), signature_delta (the thinking block's signature, ignored).
type sseContentBlockDelta struct {
	Type  string   `json:"type"`
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`
}

// sseDelta is the delta payload of a content_block_delta.
type sseDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

// sseMessageDelta is the stream's terminal metadata event: the final
// stop_reason and the output token count.
type sseMessageDelta struct {
	Type  string   `json:"type"`
	Delta sseStop  `json:"delta"`
	Usage sseUsage `json:"usage"`
}

// sseStop is the delta payload of a message_delta.
type sseStop struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// sseStreamError is the stream-embedded error event: the API's way of
// aborting an in-flight 2xx stream (overloaded upstream, blocked
// generation). It ends the request with an error carrying both fields.
type sseStreamError struct {
	Type  string   `json:"type"`
	Error sseError `json:"error"`
}

// sseError is the error payload of an sseStreamError.
type sseError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// SSE event type literals.
const (
	sseTypePing       = "ping"
	sseTypeMsgStart   = "message_start"
	sseTypeMsgStop    = "message_stop"
	sseTypeMsgDelta   = "message_delta"
	sseTypeBlkStart   = "content_block_start"
	sseTypeBlkDelta   = "content_block_delta"
	sseTypeBlkStop    = "content_block_stop"
	sseTypeErrorEvent = "error"

	deltaTypeText      = "text_delta"
	deltaTypeThinking  = "thinking_delta"
	deltaTypeInputJSON = "input_json_delta"
	deltaTypeSignature = "signature_delta"
)
