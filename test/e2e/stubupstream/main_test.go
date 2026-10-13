package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func do(t *testing.T, h http.Handler, method, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestKeyEnforcedOnV1Only(t *testing.T) {
	h := newMux(stubConfig{label: "s", apiKey: "k"}, &recorder{})
	if w := do(t, h, "POST", "/v1/chat/completions", "", `{}`); w.Code != 401 || !strings.Contains(w.Body.String(), "Unauthorized") { //nolint:lll // verbatim test code from task-5-brief.md Step 1
		t.Errorf("no key: %d %s, want 401 Unauthorized", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/v1/chat/completions", "Bearer k", `{}`); w.Code != 200 {
		t.Errorf("right key: %d, want 200", w.Code)
	}
	if w := do(t, h, "GET", "/health", "", ""); w.Code != 200 {
		t.Errorf("/health without key: %d, want 200 (vLLM leaves it open)", w.Code)
	}
}

func TestNoKeyConfiguredKeepsOldBehavior(t *testing.T) {
	h := newMux(stubConfig{label: "s"}, &recorder{})
	if w := do(t, h, "POST", "/v1/chat/completions", "", `{}`); w.Code != 200 {
		t.Errorf("router e2e stubs (no key) must stay open: %d", w.Code)
	}
}

func TestIncludeUsageChunk(t *testing.T) {
	h := newMux(stubConfig{label: "s"}, &recorder{})
	w := do(t, h, "POST", "/v1/chat/completions", "", `{"stream":true,"stream_options":{"include_usage":true}}`)
	body := w.Body.String()
	if !strings.Contains(body, `"usage":{"completion_tokens":3,"prompt_tokens":7,"total_tokens":10}`) {
		t.Errorf("stream lacks the usage chunk: %s", body)
	}
	if strings.Index(body, `"usage"`) > strings.Index(body, "[DONE]") {
		t.Errorf("usage chunk must precede [DONE]")
	}
}

func TestChunkDelayPacesTheStream(t *testing.T) {
	const delay = 300 * time.Millisecond
	mux := newMux(stubConfig{label: "s", chunkDelay: delay}, &recorder{})
	flushes := make(chan []time.Time, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc := &flushClock{ResponseWriter: w}
		mux.ServeHTTP(fc, r)
		flushes <- fc.at
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "data: "); n != 4 {
		t.Fatalf("got %d SSE events, want 3 chunks and [DONE]:\n%s", n, body)
	}

	var at []time.Time
	select {
	case at = <-flushes:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
	if len(at) != 4 {
		t.Fatalf("got %d flushes, want one per chunk plus [DONE]", len(at))
	}
	for i := 1; i < 3; i++ {
		if gap := at[i].Sub(at[i-1]); gap < delay {
			t.Errorf("chunk %d flushed %v after chunk %d, want at least %v", i, gap, i-1, delay)
		}
	}
}

// flushClock timestamps every Flush on the server side, so the pacing check
// measures the gap between chunk boundaries as the stub emits them, not when
// the client goroutine happened to be scheduled to read.
type flushClock struct {
	http.ResponseWriter
	at []time.Time
}

func (f *flushClock) Flush() {
	f.at = append(f.at, time.Now())
	if fl, ok := f.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// --- Additional coverage beyond the brief's Step 1 tests ---

// TestUnauthorizedBodyMatchesVLLM pins the exact wire body, not just a
// substring: APISIX / vLLM clients downstream may assert on the literal
// JSON, not merely that the word "Unauthorized" shows up somewhere.
func TestUnauthorizedBodyMatchesVLLM(t *testing.T) {
	h := newMux(stubConfig{label: "s", apiKey: "k"}, &recorder{})
	w := do(t, h, "POST", "/v1/chat/completions", "", `{}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"Unauthorized"}` {
		t.Errorf("body = %q, want %q", got, `{"error":"Unauthorized"}`)
	}
}

// TestKeyEnforcedOnV2AndInferenceToo checks the other two guarded
// prefixes named in the brief, not just /v1.
func TestKeyEnforcedOnV2AndInferenceToo(t *testing.T) {
	h := newMux(stubConfig{label: "s", apiKey: "k"}, &recorder{})
	for _, path := range []string{"/v2/models", "/inference/whatever"} {
		if w := do(t, h, "POST", path, "", `{}`); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without key: %d, want 401", path, w.Code)
		}
		if w := do(t, h, "POST", path, "Bearer k", `{}`); w.Code == http.StatusUnauthorized {
			t.Errorf("%s with right key: got 401, want it to pass the key check", path)
		}
	}
}

// TestIntrospectAndHealthzStayOpenWithKeyConfigured pins that the
// test-plumbing and liveness endpoints are never gated, even once a key
// is configured.
func TestIntrospectAndHealthzStayOpenWithKeyConfigured(t *testing.T) {
	h := newMux(stubConfig{label: "s", apiKey: "k"}, &recorder{})
	for _, tc := range []struct{ method, path string }{
		{"GET", "/healthz"},
		{"GET", "/__introspect__"},
		{"GET", "/__introspect__/health"},
		{"POST", "/__introspect__/reset"},
	} {
		if w := do(t, h, tc.method, tc.path, "", ""); w.Code >= 400 {
			t.Errorf("%s %s without key: %d, want it to stay open", tc.method, tc.path, w.Code)
		}
	}
}

// TestHealthRecordedSeparatelyFromIntrospect pins Ruling B: /health
// requests must show up at the new /__introspect__/health endpoint and
// must NOT pollute the existing /__introspect__ list that the router
// e2e suite reads, since kubelet probes hit /health every few seconds
// and would otherwise flood those records.
func TestHealthRecordedSeparatelyFromIntrospect(t *testing.T) {
	rec := &recorder{}
	h := newMux(stubConfig{label: "s"}, rec)

	do(t, h, "GET", "/health", "", "")
	do(t, h, "GET", "/health", "", "")
	do(t, h, "POST", "/v1/chat/completions", "", `{}`)

	main := decodeIntrospect(t, do(t, h, "GET", "/__introspect__", "", ""))
	if len(main.Requests) != 1 {
		t.Fatalf("/__introspect__ has %d requests, want 1 (health must not leak in): %+v",
			len(main.Requests), main.Requests)
	}
	if main.Requests[0].Path != "/v1/chat/completions" {
		t.Errorf("/__introspect__ recorded %q, want the chat-completions request", main.Requests[0].Path)
	}

	health := decodeIntrospect(t, do(t, h, "GET", "/__introspect__/health", "", ""))
	if len(health.Requests) != 2 {
		t.Fatalf("/__introspect__/health has %d requests, want 2", len(health.Requests))
	}
	for _, req := range health.Requests {
		if req.Path != "/health" {
			t.Errorf("/__introspect__/health recorded path %q, want /health", req.Path)
		}
	}
}

// TestResetClearsBothLists pins that /__introspect__/reset clears the
// health list along with the main one.
func TestResetClearsBothLists(t *testing.T) {
	rec := &recorder{}
	h := newMux(stubConfig{label: "s"}, rec)

	do(t, h, "GET", "/health", "", "")
	do(t, h, "POST", "/v1/chat/completions", "", `{}`)
	do(t, h, "POST", "/__introspect__/reset", "", "")

	main := decodeIntrospect(t, do(t, h, "GET", "/__introspect__", "", ""))
	if len(main.Requests) != 0 {
		t.Errorf("/__introspect__ after reset has %d requests, want 0", len(main.Requests))
	}
	health := decodeIntrospect(t, do(t, h, "GET", "/__introspect__/health", "", ""))
	if len(health.Requests) != 0 {
		t.Errorf("/__introspect__/health after reset has %d requests, want 0", len(health.Requests))
	}
}

// TestNonStreamedAlwaysCarriesUsage pins the brief's "non-streamed
// responses always carry that usage" behavior, independent of
// stream_options.
func TestNonStreamedAlwaysCarriesUsage(t *testing.T) {
	h := newMux(stubConfig{label: "s"}, &recorder{})
	w := do(t, h, "POST", "/v1/chat/completions", "", `{}`)
	if !strings.Contains(w.Body.String(), `"usage":{"completion_tokens":3,"prompt_tokens":7,"total_tokens":10}`) {
		t.Errorf("non-streamed response lacks usage: %s", w.Body.String())
	}
}

// TestStreamClientDisconnectEndsPromptly pins that writeStreamed's
// between-chunk sleep honors r.Context().Done(): once the request
// context is canceled (as net/http does when the client connection
// goes away), the handler must return promptly instead of sleeping out
// the rest of the configured chunk-delay. Exercised at the writeStreamed
// level with an explicitly canceled context rather than over a real
// network connection, since closing a client socket is itself
// instantaneous and wouldn't observe whether the *server* goroutine
// actually noticed.
func TestStreamClientDisconnectEndsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`)).WithContext(ctx)
	w := httptest.NewRecorder()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		writeStreamed(w, req, "s", 5*time.Second, false)
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("writeStreamed took %v to return after context cancellation, want well under the 5s chunk delay", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writeStreamed did not return after its context was canceled; r.Context().Done() is not honored")
	}
}

func decodeIntrospect(t *testing.T, w *httptest.ResponseRecorder) stubIntrospectResponseForTest {
	t.Helper()
	var out stubIntrospectResponseForTest
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding introspect response: %v (body: %s)", err, w.Body.String())
	}
	return out
}

type stubIntrospectResponseForTest struct {
	Label    string            `json:"label"`
	Requests []recordedRequest `json:"requests"`
}
