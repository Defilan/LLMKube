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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// auditLog collects the proxy's log output; the handler writes it from the
// server goroutine after the client is gone.
type auditLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *auditLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// dispatchLine waits for the router.dispatch audit line and returns it.
func (l *auditLog) dispatchLine(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		out := l.buf.String()
		l.mu.Unlock()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "msg=router.dispatch") {
				return line
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no router.dispatch audit line within 5s")
	return ""
}

// newOutcomeProxy serves cfg behind a real HTTP server, so a client hanging
// up is a real disconnect, and returns the proxy's audit log.
func newOutcomeProxy(t *testing.T, cfg *Config, opts ...ProxyOption) (*httptest.Server, *auditLog) {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg: %v", err)
	}
	logs := &auditLog{}
	p := NewProxy(cfg, slog.New(slog.NewTextHandler(logs, nil)), opts...)
	mux := http.NewServeMux()
	p.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, logs
}

// requestsTotal reads llmkube_router_requests_total for a default-route
// request to backend "b" (no rule, no classification).
func requestsTotal(router, outcome string) float64 {
	return testutil.ToFloat64(prommetrics.RouterRequestsTotal.WithLabelValues(router, "", "b", "", outcome))
}

// A client that hangs up mid-stream is a cancellation, not a served stream.
func TestOutcomeClientCancelledMidStream(t *testing.T) {
	be := newSSEBackend(t, 50, 10*time.Millisecond, 1) // one chunk, then silence
	srv, logs := newOutcomeProxy(t, &Config{
		Backends:     []Backend{{Name: "b", Tier: "local", Address: be.srv.URL}},
		DefaultRoute: "b",
	}, WithRouterName("outcome-midstream"))
	before := requestsTotal("outcome-midstream", "client_cancelled")

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := postStream(ctx, t, srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	line := logs.dispatchLine(t)
	if !strings.Contains(line, "outcome=client_cancelled") || !strings.Contains(line, "status=200") {
		t.Errorf("audit line = %s\nwant outcome=client_cancelled with the status already sent (200)", line)
	}
	if got := requestsTotal("outcome-midstream", "client_cancelled") - before; got != 1 {
		t.Errorf("requests_total{outcome=client_cancelled} grew by %v, want 1", got)
	}
}

// A backend that breaks off mid-stream is an upstream error, not a served
// stream: the client got a truncated response.
func TestOutcomeUpstreamErrorMidStream(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"i\":0}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection mid-body
	}))
	t.Cleanup(be.Close)
	srv, logs := newOutcomeProxy(t, &Config{
		Backends:     []Backend{{Name: "b", Tier: "local", Address: be.URL}},
		DefaultRoute: "b",
	}, WithRouterName("outcome-upstream"))
	before := requestsTotal("outcome-upstream", "upstream_error")

	resp, err := postStream(context.Background(), t, srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	line := logs.dispatchLine(t)
	if !strings.Contains(line, "outcome=upstream_error") || !strings.Contains(line, "error=") {
		t.Errorf("audit line = %s\nwant outcome=upstream_error carrying the copy error", line)
	}
	if got := requestsTotal("outcome-upstream", "upstream_error") - before; got != 1 {
		t.Errorf("requests_total{outcome=upstream_error} grew by %v, want 1", got)
	}
}

// A client that hangs up before any backend answers is a cancellation, not an
// outage, and the cancellation stops the fallback walk: a pooled fallback must
// not start a cold swap for a client that is already gone.
func TestOutcomeClientCancelledBeforeHeaders(t *testing.T) {
	reached := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		reached <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)
	pooled := newFakeBackend(t)

	fake := newFakeMemberController()
	act := NewActivator(context.Background(), fake, "r", slog.Default())
	srv, logs := newOutcomeProxy(t, &Config{
		Backends: []Backend{
			{Name: "slow", Tier: "local", Address: slow.URL},
			{Name: "judge", Tier: "local", Address: pooled.URL(), InferenceService: "judge",
				Pool: &BackendPool{Name: "slot", Namespace: "lab", Member: "judge",
					Members: []string{"judge"}, SwapBudget: 5 * time.Second}},
		},
		Rules: []Rule{{
			Name:  "fallback",
			Match: RuleMatch{Models: []string{"work"}},
			Route: RuleRoute{Backends: []string{"slow", "judge"}},
		}},
		DefaultRoute: "slow",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}, WithActivator(act))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"work"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("slow backend never received the request")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("client error = %v, want context.Canceled", err)
	}

	line := logs.dispatchLine(t)
	if !strings.Contains(line, "outcome=client_cancelled") || !strings.Contains(line, "status=499") {
		t.Errorf("audit line = %s\nwant outcome=client_cancelled status=499", line)
	}
	// The swap would start asynchronously; give it the time it takes.
	time.Sleep(200 * time.Millisecond)
	if got := fake.activateCount("judge"); got != 0 {
		t.Errorf("judge activate count = %d, want 0 (a cancelled request must not start a swap)", got)
	}
	if got := pooled.calls.Load(); got != 0 {
		t.Errorf("pooled fallback calls = %d, want 0", got)
	}
}

func TestResponseOutcome(t *testing.T) {
	live := context.Background()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	idle := fmt.Errorf("%w after 1s: %v", ErrStreamIdle, context.Canceled)
	write := (&clientWriter{failed: true}).wrap(errors.New("broken pipe"))

	cases := []struct {
		name     string
		ctx      context.Context
		streamed bool
		err      error
		want     string
	}{
		{"served stream", live, true, nil, "ok_stream"},
		{"served body", live, false, nil, "ok"},
		{"client left, upstream read cancelled", gone, true, context.Canceled, "client_cancelled"},
		{"client write failed first", live, true, write, "client_cancelled"},
		{"idle guard fired", live, true, idle, "timeout"},
		{"upstream broke off", live, false, io.ErrUnexpectedEOF, "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responseOutcome(tc.ctx, tc.streamed, tc.err); got != tc.want {
				t.Errorf("responseOutcome = %q, want %q", got, tc.want)
			}
		})
	}
}
