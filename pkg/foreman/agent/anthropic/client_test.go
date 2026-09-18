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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// scriptedResponse is one canned HTTP response. status 0 means 200.
type scriptedResponse struct {
	status int
	body   string
}

// recordedRequest is one captured inbound request, for wire assertions.
type recordedRequest struct {
	path    string
	headers http.Header
	body    []byte
}

// scriptedAnthropicServer replays canned responses in sequence (the last
// one repeats once the script is exhausted) and records every request so
// tests can assert on the exact wire shape.
func scriptedAnthropicServer(t *testing.T, responses ...scriptedResponse) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	rec := &[]recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*rec = append(*rec, recordedRequest{path: r.URL.Path, headers: r.Header.Clone(), body: body})

		idx := len(*rec) - 1
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		resp := responses[idx]
		if resp.status == 0 {
			resp.status = http.StatusOK
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func decodeBody(t *testing.T, req *recordedRequest) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(req.body, &m); err != nil {
		t.Fatalf("recorded request body is not JSON: %v\n%s", err, req.body)
	}
	return m
}

const sseHappyText = `event: ping
data: {"type":"ping"}

event: message_start
data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4-6","usage":{"input_tokens":10}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`

func TestChat_HappyText(t *testing.T) {
	srv, reqs := scriptedAnthropicServer(t, scriptedResponse{body: sseHappyText})
	client := New(srv.URL+"/v1", time.Second, 0, WithAPIKey("sk-test"))

	resp, err := client.Chat(context.Background(), oai.ChatRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.ID != "msg_01" {
		t.Errorf("id: want msg_01 got %q", resp.ID)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	ch := resp.Choices[0]
	if ch.Message.Role != oai.RoleAssistant || ch.Message.Content != "Hello" {
		t.Errorf("message: %+v", ch.Message)
	}
	if ch.Message.ReasoningContent != "" || len(ch.Message.ToolCalls) != 0 {
		t.Errorf("unexpected reasoning/tool calls: %+v", ch.Message)
	}
	if ch.FinishReason != "stop" {
		t.Errorf("finish: want stop got %q", ch.FinishReason)
	}

	if len(*reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(*reqs))
	}
	r := (*reqs)[0]
	if r.path != "/v1/messages" {
		t.Errorf("path: want /v1/messages got %q", r.path)
	}
	for hdr, want := range map[string]string{
		"x-api-key":         "sk-test",
		"anthropic-version": APIVersion,
		"Content-Type":      "application/json",
		"Accept":            "text/event-stream",
	} {
		if got := r.headers.Get(hdr); got != want {
			t.Errorf("header %s: want %q got %q", hdr, want, got)
		}
	}
	body := decodeBody(t, &r)
	if body["model"] != "claude-sonnet-4-6" {
		t.Errorf("model: %v", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream must be true: %v", body["stream"])
	}
	if body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens: want %d got %v", DefaultMaxTokens, body["max_tokens"])
	}
	if _, ok := body["system"]; ok {
		t.Error("system must be omitted when no system messages exist")
	}
	if _, ok := body["temperature"]; ok {
		t.Error("temperature must be omitted when nil")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "user" {
		t.Errorf("role: %v", m0["role"])
	}
	blocks := m0["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("want 1 content block, got %d", len(blocks))
	}
	if b0 := blocks[0].(map[string]any); b0["type"] != "text" || b0["text"] != "hello" {
		t.Errorf("block: %v", b0)
	}
}

func TestChat_NoAPIKeyOmitsHeader(t *testing.T) {
	srv, reqs := scriptedAnthropicServer(t, scriptedResponse{body: sseHappyText})
	client := New(srv.URL+"/v1", time.Second, 0)
	if _, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := (*reqs)[0].headers.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key must be omitted without WithAPIKey, got %q", got)
	}
}

const sseThinkingThenText = `event: message_start
data: {"type":"message_start","message":{"id":"msg_02","usage":{"input_tokens":3}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}
`

// The thinking trace and the answer must land in separate fields: the
// loop's reasoning-only-turn and truncation-continuation machinery keys
// off ReasoningContent, and a client that concatenates the two would
// poison both.
func TestChat_ThinkingSeparated(t *testing.T) {
	srv, _ := scriptedAnthropicServer(t, scriptedResponse{body: sseThinkingThenText})
	client := New(srv.URL+"/v1", time.Second, 0)
	resp, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	msg := resp.Choices[0].Message
	if msg.ReasoningContent != "let me think" {
		t.Errorf("reasoning: want %q got %q", "let me think", msg.ReasoningContent)
	}
	if msg.Content != "answer" {
		t.Errorf("content: want %q got %q (reasoning must not leak in)", "answer", msg.Content)
	}
}

const sseToolUseFragmented = `event: message_start
data: {"type":"message_start","message":{"id":"msg_03","usage":{"input_tokens":7}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"README"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":".md\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_2","name":"submit_result"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"verdict\":\"GO\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`

func TestChat_ToolUseFragmented(t *testing.T) {
	srv, _ := scriptedAnthropicServer(t, scriptedResponse{body: sseToolUseFragmented})
	client := New(srv.URL+"/v1", time.Second, 0)
	resp, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	ch := resp.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish: want tool_calls got %q", ch.FinishReason)
	}
	tcs := ch.Message.ToolCalls
	if len(tcs) != 2 {
		t.Fatalf("want 2 tool calls, got %d: %+v", len(tcs), tcs)
	}
	if tcs[0].ID != "tu_1" || tcs[0].Function.Name != "read_file" ||
		tcs[0].Function.Arguments != `{"path":"README.md"}` {
		t.Errorf("first tool call (fragments must concatenate): %+v", tcs[0])
	}
	if tcs[1].ID != "tu_2" || tcs[1].Function.Name != "submit_result" ||
		tcs[1].Function.Arguments != `{"verdict":"GO"}` {
		t.Errorf("second tool call: %+v", tcs[1])
	}
	if ch.Message.Content != "" {
		t.Errorf("content must be empty on a tool turn, got %q", ch.Message.Content)
	}
}

// Third-party Anthropic-compatible endpoints may send the full tool
// input on content_block_start and no input_json_delta at all. The
// aggregator must fall back to the start payload's input field.
const sseToolUseStartInput = `event: message_start
data: {"type":"message_start","message":{"id":"msg_04","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t9","name":"a","input":{"x":1}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}
`

func TestChat_ToolUseStartInputFallback(t *testing.T) {
	srv, _ := scriptedAnthropicServer(t, scriptedResponse{body: sseToolUseStartInput})
	client := New(srv.URL+"/v1", time.Second, 0)
	resp, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 || tcs[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("start-payload input must be used when no deltas arrive: %+v", tcs)
	}
}

// A tool_use block with no input anywhere (parameter-less tool) must
// surface as an empty arguments string; the loop's dispatch converts
// that to "{}", same as the oai path.
const sseToolUseNoInput = `event: message_start
data: {"type":"message_start","message":{"id":"msg_05","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_0","name":"no_args"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}
`

func TestChat_ToolUseNoInput(t *testing.T) {
	srv, _ := scriptedAnthropicServer(t, scriptedResponse{body: sseToolUseNoInput})
	client := New(srv.URL+"/v1", time.Second, 0)
	resp, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 || tcs[0].Function.Arguments != "" {
		t.Fatalf("want empty arguments, got %+v", tcs)
	}
}

// Some Anthropic-compatible endpoints close the stream at the last
// content_block_stop without sending message_stop (the same habit
// llama.cpp has with [DONE]). A clean EOF must still be a success.
const sseNoMessageStop = `event: message_start
data: {"type":"message_start","message":{"id":"msg_06","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}
`

func TestChat_CleanEOFWithoutMessageStop(t *testing.T) {
	srv, _ := scriptedAnthropicServer(t, scriptedResponse{body: sseNoMessageStop})
	client := New(srv.URL+"/v1", time.Second, 0)
	resp, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "done" || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("aggregation after clean EOF: %+v", resp.Choices[0])
	}
}

const sseErrorEvent = `event: message_start
data: {"type":"message_start","message":{"id":"msg_07","usage":{"input_tokens":1}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"upstream is saturated"}}
`

// A stream-embedded error event aborts the request with an error
// carrying both fields, and is never retried: the API has said what
// failed, and re-sending the same prompt cannot fix an overloaded
// upstream any faster than the loop-level budget could.
func TestChat_StreamEmbeddedError(t *testing.T) {
	srv, reqs := scriptedAnthropicServer(t, scriptedResponse{body: sseErrorEvent})
	client := New(srv.URL+"/v1", time.Second, 3, WithBackoffs([]time.Duration{0, 0}))
	_, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error from stream-embedded error event")
	}
	if !strings.Contains(err.Error(), "overloaded_error") ||
		!strings.Contains(err.Error(), "upstream is saturated") {
		t.Errorf("error must carry type and message: %v", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("stream-embedded errors are not retried: want 1 request got %d", len(*reqs))
	}
}

// A 2xx stream that carries no text, thinking, or tool content at all is
// a misconfigured upstream (wrong model name, empty generation) and maps
// to oai.ErrNoChoices: non-retryable, and the loop surfaces it as a task
// error instead of a silent no-op.
func TestChat_EmptyStreamIsNoChoices(t *testing.T) {
	const empty = `event: ping
data: {"type":"ping"}

event: message_start
data: {"type":"message_start","message":{"id":"msg_09","usage":{"input_tokens":1}}}

event: message_stop
data: {"type":"message_stop"}
`
	srv, reqs := scriptedAnthropicServer(t, scriptedResponse{body: empty})
	client := New(srv.URL+"/v1", time.Second, 3, WithBackoffs([]time.Duration{0, 0}))
	_, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, oai.ErrNoChoices) {
		t.Fatalf("want ErrNoChoices, got %v", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("ErrNoChoices is not retried: want 1 request got %d", len(*reqs))
	}
}

// A truncated tool-call stream (200, but the arguments JSON never
// completes) is the retryable case, mirroring llama.cpp #22072 on the
// oai path: the same prompt usually succeeds on the retry.
const sseToolUseTruncated = `event: message_start
data: {"type":"message_start","message":{"id":"msg_10","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_t","name":"read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"README"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`

func TestChat_TruncatedToolCallArgsRetries(t *testing.T) {
	srv, reqs := scriptedAnthropicServer(t,
		scriptedResponse{body: sseToolUseTruncated},
		scriptedResponse{body: sseToolUseFragmented},
	)
	client := New(srv.URL+"/v1", time.Second, 2, WithBackoffs([]time.Duration{0, 0}))
	resp, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("want success after retry, got %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(*reqs))
	}
	if got := resp.Choices[0].Message.ToolCalls[0].Function.Arguments; got != `{"path":"README.md"}` {
		t.Errorf("recovered args: %q", got)
	}
}

func TestChat_TruncatedToolCallArgsExhausts(t *testing.T) {
	srv, reqs := scriptedAnthropicServer(t, scriptedResponse{body: sseToolUseTruncated})
	client := New(srv.URL+"/v1", time.Second, 1, WithBackoffs([]time.Duration{0, 0}))
	_, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, oai.ErrTruncatedToolCallArguments) {
		t.Fatalf("want ErrTruncatedToolCallArguments after retries, got %v", err)
	}
	if !strings.Contains(err.Error(), "after 1 retries") {
		t.Errorf("exhausted error should name the retry count: %v", err)
	}
	if len(*reqs) != 2 {
		t.Errorf("want 1 initial + 1 retry, got %d", len(*reqs))
	}
}

// A non-2xx response is never retried, and the error must expose an
// *oai.StatusError so the loop's rejectsTemperature check works
// identically against both native endpoints.
func TestChat_Non2xx(t *testing.T) {
	longBody := `{"type":"error","error":{"type":"invalid_request_error",` +
		`"message":"temperature is not supported for this model. ` + strings.Repeat("x", 600) + `"}}`
	srv, reqs := scriptedAnthropicServer(t, scriptedResponse{status: 400, body: longBody})
	client := New(srv.URL+"/v1", time.Second, 3, WithBackoffs([]time.Duration{0, 0}))
	_, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error on 400")
	}
	var se *oai.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error must wrap *oai.StatusError, got %T: %v", err, err)
	}
	if se.Code != 400 {
		t.Errorf("status code: want 400 got %d", se.Code)
	}
	if !strings.Contains(se.Body, "temperature") {
		t.Errorf("body must identify the failure: %q", se.Body)
	}
	if len(se.Body) > 500 {
		t.Errorf("body must be excerpted to at most 500 bytes, got %d", len(se.Body))
	}
	if !strings.Contains(err.Error(), "anthropic: POST") || !strings.Contains(err.Error(), "400") {
		t.Errorf("error message: %v", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("non-2xx is not retried: want 1 request got %d", len(*reqs))
	}
}

// The 400-temperature case the loop keys on (rejectsTemperature): even
// with retries armed, a deterministic 4xx never re-sends the request.
func TestChat_NonRetryable400NotRetried(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"temperature must be between 0 and 1"}}`
	srv, reqs := scriptedAnthropicServer(t, scriptedResponse{status: 400, body: body})
	client := New(srv.URL+"/v1", time.Second, 3, WithBackoffs([]time.Duration{0, 0, 0}))
	_, err := client.Chat(context.Background(), oai.ChatRequest{
		Model: "m", Messages: []oai.Message{{Role: oai.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error on 400")
	}
	if len(*reqs) != 1 {
		t.Errorf("a deterministic 400 must not be retried: want 1 request got %d", len(*reqs))
	}
}

func TestMapStopReason(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"tool_use", "tool_calls"},
		{"max_tokens", "length"},
		{"refusal", "refusal"}, // unknown reasons pass through verbatim
		{"", ""},
	} {
		if got := mapStopReason(tc.in); got != tc.want {
			t.Errorf("mapStopReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// netErr is the minimal net.Error implementation for retry-classification
// tests; timeout selects the Timeout() outcome.
type netErr struct{ timeout bool }

func (e *netErr) Error() string   { return "net err" }
func (e *netErr) Timeout() bool   { return e.timeout }
func (e *netErr) Temporary() bool { return false }

func TestRetryClassification(t *testing.T) {
	t.Run("isRetryableTimeout", func(t *testing.T) {
		for _, tc := range []struct {
			err  error
			want bool
		}{
			{&netErr{timeout: true}, true},
			{&netErr{timeout: false}, false},
			{io.EOF, false},
			{oai.ErrNoChoices, false},
		} {
			if got := isRetryableTimeout(tc.err); got != tc.want {
				t.Errorf("isRetryableTimeout(%v) = %v, want %v", tc.err, got, tc.want)
			}
		}
	})
	t.Run("isRetryableTransportError", func(t *testing.T) {
		for _, tc := range []struct {
			err  error
			want bool
		}{
			{io.ErrUnexpectedEOF, true},
			{io.EOF, true},
			{&netErr{timeout: true}, true},
			{syscall.ECONNRESET, true},
			{errors.New("read tcp: connection reset by peer"), true},
			{&statusError{code: 400, inner: &oai.StatusError{Code: 400}}, false},
			{&netErr{timeout: false}, false},
		} {
			if got := isRetryableTransportError(tc.err); got != tc.want {
				t.Errorf("isRetryableTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		}
	})
}
