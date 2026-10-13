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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sseBackend serves an SSE stream: n chunks every gap, then [DONE]. When
// stallAfter >= 0 it stops writing after that many chunks and blocks until
// the request context ends.
type sseBackend struct {
	srv        *httptest.Server
	calls      atomic.Int64
	chunks     int
	gap        time.Duration
	stallAfter int
}

func newSSEBackend(t *testing.T, chunks int, gap time.Duration, stallAfter int) *sseBackend {
	t.Helper()
	b := &sseBackend{chunks: chunks, gap: gap, stallAfter: stallAfter}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < b.chunks; i++ {
			if b.stallAfter >= 0 && i == b.stallAfter {
				<-r.Context().Done()
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"i\":%d}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-time.After(b.gap):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// newStreamTestProxy serves a proxy for one backend (optionally with
// endpoints) behind a real HTTP server, so client disconnects are real.
func newStreamTestProxy(t *testing.T, backend Backend, opts ...DispatcherOption) (*Proxy, *httptest.Server) {
	t.Helper()
	cfg := &Config{Backends: []Backend{backend}, DefaultRoute: backend.Name}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg: %v", err)
	}
	p := NewProxy(cfg, slog.Default(), WithDispatcherOptions(opts...))
	mux := http.NewServeMux()
	p.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return p, srv
}

func postStream(ctx context.Context, t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"any","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// TestProxyStreamOutlivesHeaderTimeout: the header timeout bounds only the
// wait for headers, so a healthy stream that runs well past it is relayed in
// full, [DONE] included (#2024). The idle guard is on and shorter than the
// stream, so every chunk must re-arm it.
func TestProxyStreamOutlivesHeaderTimeout(t *testing.T) {
	be := newSSEBackend(t, 12, 30*time.Millisecond, -1) // ~360ms of stream
	_, srv := newStreamTestProxy(t, Backend{Name: "b", Tier: "local", Address: be.srv.URL},
		WithResponseHeaderTimeout(100*time.Millisecond), WithStreamIdleTimeout(150*time.Millisecond))

	resp, err := postStream(context.Background(), t, srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if got := strings.Count(string(body), `"i":`); got != 12 {
		t.Errorf("chunks relayed = %d, want 12 (stream cut at the header timeout?)", got)
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Errorf("stream ended without [DONE]; body tail = %q", tail(string(body), 80))
	}
}

// TestProxyStalledStreamQuarantinesEndpoint: an endpoint that sends headers
// and a chunk, then goes silent, is aborted after the idle timeout and
// quarantined, so following requests go to the healthy endpoint only.
func TestProxyStalledStreamQuarantinesEndpoint(t *testing.T) {
	wedged := newSSEBackend(t, 50, 10*time.Millisecond, 1)
	healthy := newSSEBackend(t, 3, 5*time.Millisecond, -1)
	p, srv := newStreamTestProxy(t, Backend{
		Name: "b", Tier: "local", Address: healthy.srv.URL,
		Endpoints: []string{wedged.srv.URL, healthy.srv.URL},
	}, WithStreamIdleTimeout(150*time.Millisecond), WithQuarantineDuration(time.Minute))

	for i := 0; i < 2 && wedged.calls.Load() == 0; i++ {
		start := time.Now()
		resp, err := postStream(context.Background(), t, srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if wedged.calls.Load() == 1 && time.Since(start) > 2*time.Second {
			t.Fatalf("stalled stream took %v to end; idle guard did not fire", time.Since(start))
		}
	}
	if wedged.calls.Load() != 1 {
		t.Fatalf("wedged endpoint calls = %d, want 1", wedged.calls.Load())
	}
	if p.disp.dispatchable(healthKey("b", wedged.srv.URL)) {
		t.Fatalf("wedged endpoint still dispatchable after an idle-timeout abort")
	}

	before := healthy.calls.Load()
	for i := 0; i < 4; i++ {
		resp, err := postStream(context.Background(), t, srv.URL)
		if err != nil {
			t.Fatalf("follow-up %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if wedged.calls.Load() != 1 {
		t.Errorf("quarantined endpoint received %d more requests", wedged.calls.Load()-1)
	}
	if healthy.calls.Load()-before != 4 {
		t.Errorf("healthy endpoint served %d of 4 follow-ups", healthy.calls.Load()-before)
	}
}

// TestProxyStreamIdleTimeoutZeroDisables: with the guard off, a stream that
// pauses longer than any idle window still completes.
func TestProxyStreamIdleTimeoutZeroDisables(t *testing.T) {
	be := newSSEBackend(t, 3, 250*time.Millisecond, -1)
	_, srv := newStreamTestProxy(t, Backend{Name: "b", Tier: "local", Address: be.srv.URL},
		WithStreamIdleTimeout(0))

	resp, err := postStream(context.Background(), t, srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Errorf("stream with idle guard off ended without [DONE]")
	}
}

// TestProxyClientDisconnectDoesNotQuarantine: a client that goes away
// mid-stream is not a backend failure (#462), so the endpoint stays healthy.
func TestProxyClientDisconnectDoesNotQuarantine(t *testing.T) {
	be := newSSEBackend(t, 1000, 20*time.Millisecond, -1)
	p, srv := newStreamTestProxy(t, Backend{
		Name: "b", Tier: "local", Address: be.srv.URL, Endpoints: []string{be.srv.URL},
	}, WithStreamIdleTimeout(time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := postStream(ctx, t, srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()
	time.Sleep(300 * time.Millisecond)

	if !p.disp.dispatchable(healthKey("b", be.srv.URL)) {
		t.Errorf("endpoint quarantined after a client disconnect")
	}
}

// TestProxyHeaderTimeoutDoesNotQuarantine: a non-streaming response slower
// than the header timeout still fails fast, and a rule deadline is still not
// a health signal (#462).
func TestProxyHeaderTimeoutDoesNotQuarantine(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(slow.Close)
	p, srv := newStreamTestProxy(t, Backend{
		Name: "b", Tier: "local", Address: slow.URL, Endpoints: []string{slow.URL},
	}, WithResponseHeaderTimeout(50*time.Millisecond))

	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"any"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Errorf("status = %d, want a 5xx for a header timeout", resp.StatusCode)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("proxy waited %v on a 50ms header timeout", time.Since(start))
	}
	if !p.disp.dispatchable(healthKey("b", slow.URL)) {
		t.Errorf("endpoint quarantined after a header timeout")
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestIdleGuardIgnoresTimeBetweenReads: the guard measures upstream silence,
// so time the proxy spends between Reads, blocked writing to a slow client
// that is applying backpressure, must not count. Before the fix the timer ran
// from construction and only re-armed after a Read, so a paused client
// quarantined a healthy backend and cancelled its request.
func TestIdleGuardIgnoresTimeBetweenReads(t *testing.T) {
	var idle atomic.Bool
	g := newIdleGuardBody(io.NopCloser(strings.NewReader(strings.Repeat("x", 64))),
		50*time.Millisecond, func() { idle.Store(true) }, nil)
	defer func() { _ = g.Close() }()

	buf := make([]byte, 8)
	for i := 0; i < 4; i++ {
		// Before the first Read this is the proxy writing headers to a slow
		// client; after it, a body write blocked on backpressure.
		time.Sleep(150 * time.Millisecond)
		if _, err := g.Read(buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if idle.Load() {
		t.Fatal("idle guard fired although the upstream always had data; it timed the caller, not the upstream")
	}
}

// TestIdleGuardFiresWhileReadBlocks: a Read blocked on a silent upstream is
// exactly what the guard is for, and it must still fire.
func TestIdleGuardFiresWhileReadBlocks(t *testing.T) {
	pr, pw := io.Pipe()
	fired := make(chan struct{})
	g := newIdleGuardBody(pr, 50*time.Millisecond, func() {
		close(fired)
		// Production cancels the request context, which fails the upstream
		// read; the pipe stands in for that.
		_ = pw.CloseWithError(context.Canceled)
	}, nil)
	defer func() { _ = g.Close() }()

	done := make(chan error, 1)
	go func() {
		_, err := g.Read(make([]byte, 8))
		done <- err
	}()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("idle guard did not fire while Read was blocked on a silent upstream")
	}
	if err := <-done; !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("Read error = %v, want ErrStreamIdle", err)
	}
}
