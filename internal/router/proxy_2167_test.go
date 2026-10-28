/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// lockedLogBuffer collects the proxy's structured log lines from handler and
// swap goroutines at once.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newProxy2167 builds the reporter's topology: one backend ("coder") whose
// ModelPool member never becomes Ready, fronted by a proxy whose default route
// is that backend.
func newProxy2167(
	t *testing.T, fake *fakeMemberController, fb *fakeBackend,
	budget time.Duration, routerName string, logger *slog.Logger, opts ...ProxyOption,
) (*Activator, http.Handler) {
	t.Helper()
	cfg := &Config{
		Backends: []Backend{{
			Name:    "coder",
			Tier:    "local",
			Address: fb.URL(),
			Timeout: 50 * time.Millisecond,
			Pool: &BackendPool{
				Name:       "heavy-slot",
				Namespace:  "lab",
				Member:     "coder",
				Members:    []string{"coder", "judge"},
				SwapBudget: budget,
			},
		}},
		DefaultRoute: "coder",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	act := NewActivator(baseCtx, fake, routerName, logger)
	opts = append(opts, WithActivator(act))
	proxy := NewProxy(cfg, logger, opts...)
	mux := http.NewServeMux()
	proxy.Mount(mux)
	return act, mux
}

func req2167(t *testing.T) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    "coder",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func swapDeadlineFailures(routerName string) prometheus.Counter {
	return prommetrics.ModelPoolSwapFailuresTotal.WithLabelValues(routerName, "heavy-slot", "coder", "deadline")
}

// TestProxyPoolAbandonedSwap503RetryAfter pins #2167: a request that waits on a
// swap abandoned at the pool's swap deadline gets the same 503 + Retry-After as
// one whose own hold expired, whichever of the two timers fires first. The
// fixture puts the swap's abandonment first, so no timer ordering is involved.
func TestProxyPoolAbandonedSwap503RetryAfter(t *testing.T) {
	fb := newFakeBackend(t)
	fake := newFakeMemberController()
	fake.noAutoReady = true
	logs := &lockedLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	act, mux := newProxy2167(t, fake, fb, 500*time.Millisecond, "r2167", logger)
	before := testutil.ToFloat64(swapDeadlineFailures("r2167"))

	// The first request starts the swap and then gives up on its own timeout, a
	// client that walked away mid-load. The swap keeps loading.
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer firstCancel()
	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req2167(t).WithContext(firstCtx))
		firstDone <- rec.Code
	}()
	waitFor(t, "the swap to be in flight", func() bool {
		act.mu.Lock()
		defer act.mu.Unlock()
		pr := act.pools["lab/heavy-slot"]
		return pr != nil && pr.swapping
	})
	waitFor(t, "the first request to give up", func() bool {
		act.mu.Lock()
		defer act.mu.Unlock()
		return act.pools["lab/heavy-slot"].waiting["coder"] == 0
	})
	if first := <-firstDone; first != http.StatusServiceUnavailable {
		t.Errorf("first (client timeout mid-swap) status = %d, want 503", first)
	}

	// The second request arrives mid-swap with its whole hold budget ahead of
	// it, so the swap's deadline is what ends its wait.
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer secondCancel()
	secondStart := time.Now()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req2167(t).WithContext(secondCtx))

	if got := testutil.ToFloat64(swapDeadlineFailures("r2167")) - before; got != 1 {
		t.Errorf("swap_failures_total{reason=deadline} delta = %v, want 1", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("missing Retry-After header on abandoned-swap 503")
	}
	if got := fb.calls.Load(); got != 0 {
		t.Errorf("backend calls = %d, want 0 (must not dispatch when the target never became resident)", got)
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("activate count = %d, want 1 (the waiter must observe the abandoned swap, not start a fresh load)", got)
	}
	if !abandonedSwapAuditLine(logs.String(), secondStart) {
		t.Errorf("audit log has no status=503 outcome=pool_activation_timeout line for the abandoned-swap request:\n%s",
			logs.String())
	}
	waitFor(t, "the abandoned target to be scaled back down", func() bool {
		return fake.deactivateCount("coder") >= 1
	})
}

// abandonedSwapAuditLine reports whether the audit log holds a 503
// pool_activation_timeout dispatch that began at or after start. The first
// request's own hold timeout logs the same token before the swap is abandoned,
// so matching the token alone pins nothing about the abandoned swap.
func abandonedSwapAuditLine(log string, start time.Time) bool {
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "msg=router.dispatch") ||
			!strings.Contains(line, "status=503") ||
			!strings.Contains(line, "outcome=pool_activation_timeout") {
			continue
		}
		at, ok := auditLineTime(line)
		if ok && !at.Before(start) {
			return true
		}
	}
	return false
}

// auditLineTime returns the wall-clock time slog's TextHandler stamped on line,
// truncated to the millisecond the handler prints. The truncation makes the
// comparison lenient by up to a millisecond, which the second request's held
// wait covers. The handler prints the zone offset with its colon (-07:00),
// which is what RFC 3339 requires and what RFC3339Nano parses.
func auditLineTime(line string) (time.Time, bool) {
	for _, field := range strings.Fields(line) {
		stamp, ok := strings.CutPrefix(field, "time=")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return time.Time{}, false
		}
		return at, true
	}
	return time.Time{}, false
}

// TestProxyPoolSwapErrorStays502 pins the other half of #2167: a swap that fails
// because the controller rejected the activation, rather than because the
// deadline passed, is not a transient capacity condition and keeps its 502.
func TestProxyPoolSwapErrorStays502(t *testing.T) {
	fb := newFakeBackend(t)
	fake := newFakeMemberController()
	fake.activateFail = map[string]bool{"coder": true}
	logger := slog.New(slog.NewTextHandler(&lockedLogBuffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	before := testutil.ToFloat64(swapDeadlineFailures("r2167-error"))
	_, mux := newProxy2167(t, fake, fb, 5*time.Second, "r2167-error", logger)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req2167(t))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (a rejected activation is not the retryable deadline class)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on a 502, want none", got)
	}
	if got := testutil.ToFloat64(swapDeadlineFailures("r2167-error")) - before; got != 0 {
		t.Errorf("swap_failures_total{reason=deadline} delta = %v, want 0 (a rejected activation is counted as reason=error)", got)
	}
}

// TestProxyPoolNoSwapBudgetHoldStill503 pins the hold path of a pool that
// declares no swapBudget: the hold is the response-header timeout while the
// swap bound is twice the *default* one, so the hold always ends the request
// with 503 and no swap is ever abandoned for it.
func TestProxyPoolNoSwapBudgetHoldStill503(t *testing.T) {
	fb := newFakeBackend(t)
	fake := newFakeMemberController()
	fake.noAutoReady = true
	logger := slog.New(slog.NewTextHandler(&lockedLogBuffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	before := testutil.ToFloat64(swapDeadlineFailures("r2167-nobudget"))
	_, mux := newProxy2167(t, fake, fb, 0, "r2167-nobudget", logger,
		WithDispatcherOptions(WithResponseHeaderTimeout(150*time.Millisecond)))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req2167(t))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (hold expiry on a pool with no swapBudget)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("missing Retry-After header")
	}
	if got := testutil.ToFloat64(swapDeadlineFailures("r2167-nobudget")) - before; got != 0 {
		t.Errorf("swap_failures_total{reason=deadline} delta = %v, want 0 (the 240s swap bound is not what ended this request)", got)
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("activate count = %d, want 1", got)
	}
	if got := fb.calls.Load(); got != 0 {
		t.Errorf("backend calls = %d, want 0", got)
	}
}
