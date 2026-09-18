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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// Client is a minimal client for the Anthropic Messages API.
//
// As with the oai client it is deliberately not the official SDK: Foreman
// needs exactly one HTTP shape, wants provider-specific retry semantics,
// and does not want the SDK's transitive cone (#1627).
//
// Wire protocol is SSE (text/event-stream) only. Aggregation happens
// transparently inside Chat; callers see the same oai-shaped
// ChatResponse regardless of which native endpoint they dialed. The two
// clients therefore sit behind the same ChatCompleter seam in the loop.
type Client struct {
	baseURL    string
	httpClient *http.Client
	maxRetries int
	// apiKey, when non-empty, is sent on every request as the x-api-key
	// header (the Messages API's auth scheme — not Authorization/Bearer,
	// which is what the oai client's WithAuthHeader sends). Empty dials
	// without auth (local Anthropic-compatible servers).
	apiKey string
	// backoffs are the inter-attempt delays, each wrapped with +/- 20%
	// jitter so a fleet of agents does not retry in lockstep.
	backoffs []time.Duration
}

// Option configures the Client.
type Option func(*Client)

// WithAPIKey sets the x-api-key header value sent on every request.
// Anthropic's scheme is a dedicated header, not Authorization: Bearer;
// the oai client's WithAuthHeader is deliberately not reused here so a
// caller cannot conflate the two. Empty disables the header.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// WithHTTPClient overrides the default http.Client (mainly for tests).
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// WithBackoffs replaces the default 50ms / 250ms / 1s schedule. Useful
// in tests to keep the suite fast.
func WithBackoffs(b []time.Duration) Option {
	return func(cl *Client) {
		cl.backoffs = append([]time.Duration(nil), b...)
	}
}

// New builds a client. baseURL must include the API version prefix
// ("https://api.anthropic.com/v1"); requests POST to <baseURL>/messages.
// requestTimeout bounds how long we wait for the upstream to start
// sending response headers; it does not bound the total streaming
// duration (a long thinking stream would otherwise spuriously time
// out). The caller's context.Context is the overall guard. maxRetries
// bounds how many additional attempts are made on a truncated tool-call
// stream or transient transport failure; pass 0 to disable retries
// entirely.
func New(baseURL string, requestTimeout time.Duration, maxRetries int, opts ...Option) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = requestTimeout
	c := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: transport,
			// No Client.Timeout: the streaming body can legitimately
			// take longer than the header wait (a thinking model
			// emitting a long reasoning trace). ctx.Done is the
			// overall guard.
		},
		maxRetries: maxRetries,
		backoffs: []time.Duration{
			50 * time.Millisecond,
			250 * time.Millisecond,
			time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Chat sends a single Messages request and returns the aggregated
// streamed response. On oai.ErrTruncatedToolCallArguments, a transient
// header timeout, or a transient transport disconnect the call is
// retried up to MaxRetries times with bounded backoff plus jitter
// (mirroring the oai client's policy). Any other error (HTTP non-2xx,
// stream-embedded error, SSE parse failure) is returned to the caller
// immediately without retry.
func (c *Client) Chat(ctx context.Context, req oai.ChatRequest) (*oai.ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.sleepBackoff(ctx, attempt-1); err != nil {
				return nil, err
			}
		}
		resp, err := c.doOnce(ctx, req)
		if err == nil {
			return resp, nil
		}
		// Never retry once the parent context is done: a loop-wide
		// budget (#532) or external cancellation must propagate so the
		// caller can exit gracefully instead of spinning retries.
		// Checked before classification because a context deadline
		// also satisfies net.Error.Timeout().
		if ctx.Err() != nil {
			return nil, err
		}
		if errors.Is(err, oai.ErrTruncatedToolCallArguments) || isRetryableTimeout(err) || isRetryableTransportError(err) {
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("after %d retries: %w", c.maxRetries, lastErr)
}

// isRetryableTimeout reports whether err is a network timeout (e.g. the
// transport's ResponseHeaderTimeout firing while awaiting the first
// byte). Mirrors oai.isRetryableTimeout (unexported there, so duplicated
// here — keep the two in sync). Callers must rule out a done parent
// context first, since context.DeadlineExceeded also satisfies
// net.Error.Timeout().
func isRetryableTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isRetryableTransportError reports whether err is a transient transport
// failure that can be retried by re-issuing the same request: mid-stream
// disconnects (io.ErrUnexpectedEOF, io.EOF), network timeouts, and
// connection resets. Genuine API errors (non-2xx, stream-embedded error
// events) are NOT retryable and must pass through unchanged. Mirrors
// oai.isRetryableTransportError (unexported there, so duplicated here —
// keep the two in sync).
func isRetryableTransportError(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if strings.Contains(err.Error(), "connection reset by peer") {
		return true
	}
	return false
}

// sleepBackoff blocks for backoffs[i] +/- 20% jitter, honoring ctx
// cancellation. i is clamped to the last backoff if the schedule is
// shorter than the retry count.
func (c *Client) sleepBackoff(ctx context.Context, i int) error {
	if len(c.backoffs) == 0 {
		return nil
	}
	if i >= len(c.backoffs) {
		i = len(c.backoffs) - 1
	}
	d := c.backoffs[i]
	// math/rand/v2 is intentional here: this is jitter to de-synchronize a
	// fleet of agents retrying at the same time, not a security primitive.
	// crypto/rand would be a wasteful overcorrection and add an unbounded
	// failure mode (entropy exhaustion under load).
	jitter := time.Duration(float64(d) * 0.4 * (rand.Float64() - 0.5)) //nolint:gosec // G404: jitter, not security
	d += jitter
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// doOnce performs a single streaming round trip with no retry. Returns
// the aggregated response on 2xx + a stream with substance, or one of:
//
//   - oai.ErrNoChoices                   :: 2xx but no text/thinking/tool content
//   - oai.ErrTruncatedToolCallArguments  :: 2xx but tool_use input is not a JSON object
//   - *statusError                       :: non-2xx (wraps *oai.StatusError)
//   - a stream-embedded error            :: the API's in-stream abort
//   - a wrapped transport error          :: dial / TLS / read failure
func (c *Client) doOnce(ctx context.Context, req oai.ChatRequest) (*oai.ChatResponse, error) {
	wire, err := translateRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", APIVersion)
	if c.apiKey != "" {
		httpReq.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: dispatch request: %w", err)
	}
	defer func() {
		// Drain so the TCP connection can be reused, but bound the
		// drain so a server that holds the connection open after
		// message_stop cannot stall the caller. Cap at 64 KiB and
		// 100ms; on hit, we close without reuse and the next call
		// opens a fresh conn.
		drained := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(100 * time.Millisecond):
		}
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Cap the body in the message: provider error bodies can be
		// verbose and this string lands in task status. 500 bytes is
		// enough to identify the failure (the "temperature"-style
		// keywords rejectsTemperature looks for arrive early).
		excerpt := string(b)
		if len(excerpt) > 500 {
			excerpt = excerpt[:500]
		}
		return nil, &statusError{
			code:  resp.StatusCode,
			path:  httpReq.URL.String(),
			body:  excerpt,
			inner: &oai.StatusError{Code: resp.StatusCode, Body: excerpt},
		}
	}

	return readSSEStream(resp.Body)
}

// statusError is a non-2xx response from the Messages API. It wraps
// *oai.StatusError (via Unwrap) so the loop's rejectsTemperature check
// — which does errors.As for *oai.StatusError and looks for a 400 whose
// body mentions "temperature" — works identically against both native
// endpoints.
type statusError struct {
	code  int
	path  string
	body  string
	inner *oai.StatusError
}

func (e *statusError) Error() string {
	return fmt.Sprintf("anthropic: POST %s: %d: %s", e.path, e.code, e.body)
}

func (e *statusError) Unwrap() error { return e.inner }

// readSSEStream reads the Messages SSE stream and aggregates it into a
// single oai-shaped ChatResponse.
//
// The wire format is standard SSE: events are separated by blank lines,
// each with an `event:` name line and one `data:` line carrying a JSON
// object whose `type` field duplicates the event name. This parser
// dispatches on the type field and uses the event: name only as a
// fallback, so an endpoint that omits one or the other still works.
// `ping` keep-alives and unknown event types are ignored (forward
// compatibility). The stream ends on message_stop, but a clean EOF
// before it is also valid — some Anthropic-compatible endpoints stop
// sending at the last content_block_stop without the terminator, as
// llama.cpp does with [DONE].
func readSSEStream(body io.Reader) (*oai.ChatResponse, error) {
	scanner := bufio.NewScanner(body)
	// SSE data lines carry complete JSON objects and can be large
	// (a thinking block or a big tool input fragment). Same max-token
	// budget as the oai reader.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	agg := &messageAggregator{blockByIndex: make(map[int]*toolBlock)}
	pendingEvent := ""

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(line[len("event:"):])
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Blank lines, comments, and other fields are protocol
			// noise for our purposes.
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}

		evt := ""
		if strings.HasPrefix(payload, "{") {
			var probe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(payload), &probe); err == nil {
				evt = probe.Type
			}
		}
		if evt == "" {
			evt = pendingEvent
		}
		pendingEvent = ""

		if err := agg.absorb(evt, payload); err != nil {
			return nil, err
		}
		if agg.done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("anthropic: read SSE: %w", err)
	}
	if agg.streamErr != nil {
		return nil, agg.streamErr
	}
	return agg.build()
}

// toolBlock is one in-flight tool_use content block, accumulated from
// its content_block_start (id, name, and the initial input field) plus
// the input_json_delta fragments.
type toolBlock struct {
	index    int
	id       string
	name     string
	inputBuf strings.Builder
	// inputStart is the input field of the content_block_start payload.
	// The Anthropic API sends a "{}" placeholder (the real arguments
	// arrive as partial_json deltas), but a third-party endpoint could
	// send the full object up front and no deltas — build() falls back
	// to it only when the delta buffer is empty.
	inputStart json.RawMessage
}

// messageAggregator collapses the Messages SSE events into one
// oai-shaped ChatResponse. Content and reasoning accumulate as strings
// (the oai Message shape); tool_use blocks are tracked by their stream
// index and emitted in start order.
type messageAggregator struct {
	id           string
	content      strings.Builder
	reasoning    strings.Builder
	hadSubstance bool // any non-empty text or thinking delta, or a tool_use block
	blocks       []*toolBlock
	blockByIndex map[int]*toolBlock
	stopReason   string
	// inputTokens / outputTokens are parsed from message_start and
	// message_delta but not yet surfaced: the oai-shaped ChatResponse
	// has no Usage field. They exist so a future usage field has a
	// ready source (#1627).
	inputTokens  int
	outputTokens int
	done         bool
	streamErr    error
}

// absorb folds one SSE event into the aggregator. evt is the event type
// (from the data JSON's type field, or the event: name as fallback).
// Unknown types are ignored so a newer API version can add events
// without breaking older clients.
func (a *messageAggregator) absorb(evt, payload string) error {
	switch evt {
	case sseTypePing:
		// Keep-alive; nothing to do.

	case sseTypeMsgStart:
		var e sseMessageStart
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return fmt.Errorf("anthropic: decode message_start: %w", err)
		}
		a.id = e.Message.ID
		a.inputTokens = e.Message.Usage.InputTokens

	case sseTypeBlkStart:
		var e sseContentBlockStart
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return fmt.Errorf("anthropic: decode content_block_start: %w", err)
		}
		switch e.ContentBlock.Type {
		case blockTypeText, blockTypeThinking:
			// The block opens; bytes arrive as deltas.
		case blockTypeToolUse:
			tb := &toolBlock{
				index:      e.Index,
				id:         e.ContentBlock.ID,
				name:       e.ContentBlock.Name,
				inputStart: e.ContentBlock.Input,
			}
			a.blockByIndex[e.Index] = tb
			a.blocks = append(a.blocks, tb)
			a.hadSubstance = true
		default:
			// Unknown block kind (e.g. a future type): its deltas
			// will not match a known delta type and are dropped.
		}

	case sseTypeBlkDelta:
		var e sseContentBlockDelta
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return fmt.Errorf("anthropic: decode content_block_delta: %w", err)
		}
		switch e.Delta.Type {
		case deltaTypeText:
			if e.Delta.Text != "" {
				a.hadSubstance = true
			}
			a.content.WriteString(e.Delta.Text)
		case deltaTypeThinking:
			if e.Delta.Thinking != "" {
				a.hadSubstance = true
			}
			a.reasoning.WriteString(e.Delta.Thinking)
		case deltaTypeInputJSON:
			if b := a.blockByIndex[e.Index]; b != nil {
				b.inputBuf.WriteString(e.Delta.PartialJSON)
			}
		case deltaTypeSignature:
			// The thinking block's signature. It is required to
			// replay a thinking block and unusable here (third-party
			// endpoints emit unsigned ones); dropped with the rest
			// of the reasoning on the way back into history.
		default:
			// Unknown delta kind: dropped.
		}

	case sseTypeBlkStop:
		// Blocks close in the same order they open; no state to update.

	case sseTypeMsgDelta:
		var e sseMessageDelta
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return fmt.Errorf("anthropic: decode message_delta: %w", err)
		}
		a.stopReason = e.Delta.StopReason
		a.outputTokens = e.Usage.OutputTokens

	case sseTypeMsgStop:
		a.done = true

	case sseTypeErrorEvent:
		var e sseStreamError
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return fmt.Errorf("anthropic: decode stream error: %w", err)
		}
		a.streamErr = fmt.Errorf("anthropic: stream error: %s: %s", e.Error.Type, e.Error.Message)

	default:
		// Unknown event type: ignore (forward compatibility).
	}
	return nil
}

// build assembles the aggregated oai-shaped response. A stream that
// carried no substance at all maps to oai.ErrNoChoices (a misconfigured
// upstream, non-retryable). Tool-call arguments that are non-empty but
// not a JSON object map to oai.ErrTruncatedToolCallArguments (a
// mid-stream truncation, retryable — the same class of failure as the
// oai client's, and the loop's retry budget covers both).
func (a *messageAggregator) build() (*oai.ChatResponse, error) {
	if !a.hadSubstance {
		return nil, fmt.Errorf("%w: stream ended with no text, thinking, or tool content", oai.ErrNoChoices)
	}

	var toolCalls []oai.ToolCall
	for _, b := range a.blocks {
		args := b.inputBuf.String()
		if args == "" && len(b.inputStart) > 0 && string(b.inputStart) != "{}" {
			args = string(b.inputStart)
		}
		if args != "" {
			var probe map[string]any
			if err := json.Unmarshal([]byte(args), &probe); err != nil {
				return nil, fmt.Errorf("%w (tool_use %s): %w", oai.ErrTruncatedToolCallArguments, b.id, err)
			}
		}
		// An empty argument string is kept as-is: the loop's dispatch
		// converts "" to "{}", same as the oai path.
		toolCalls = append(toolCalls, oai.ToolCall{
			ID:   b.id,
			Type: "function",
			Function: oai.ToolCallFunction{
				Name:      b.name,
				Arguments: args,
			},
		})
	}

	msg := oai.Message{
		Role:      oai.RoleAssistant,
		Content:   a.content.String(),
		ToolCalls: toolCalls,
	}
	if a.reasoning.Len() > 0 {
		msg.ReasoningContent = a.reasoning.String()
	}
	return &oai.ChatResponse{
		ID: a.id,
		Choices: []oai.Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: mapStopReason(a.stopReason),
		}},
	}, nil
}

// mapStopReason maps an Anthropic stop_reason to the OpenAI finish_reason
// vocabulary the loop switches on. end_turn and stop_sequence are both
// "the model finished normally" (the loop treats them as the same);
// tool_use is the tool-call turn; max_tokens is a token-cap truncation
// (the loop's truncation-continuation path). Anything else — including
// the empty value of a stream that ended before message_delta — is
// passed through verbatim so a new reason degrades to "unknown" rather
// than being silently reclassified.
func mapStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return s
	}
}
