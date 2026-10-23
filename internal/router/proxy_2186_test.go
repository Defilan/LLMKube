/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIssue2186ProxyParity is a parity test, not a reproduction: three requests
// held on one swap abandoned at the pool's swap deadline are answered 503 with
// Retry-After on the base and after the #2186 fix alike. It pins the request
// path that must not move while the activator's failure bookkeeping changes,
// and it fails loudly if a future change starts dispatching to the member that
// never became resident.
//
// The requests carry a non-cancellable context: an httptest request context is
// cancelled when the handler returns, which turns a held request into a
// transport error rather than the pool's answer.
func TestIssue2186ProxyParity(t *testing.T) {
	fb := newFakeBackend(t)
	fake := newFakeMemberController()
	fake.noAutoReady = true
	logger := slog.New(slog.NewTextHandler(&lockedLogBuffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	act, mux := newProxy2167(t, fake, fb, 300*time.Millisecond, routerIssue2186, logger)

	const calls = 3
	codes := make(chan int, calls)
	serve := func() {
		req := req2167(t).WithContext(context.WithoutCancel(context.Background()))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		codes <- rec.Code
	}
	go serve()
	waitFor(t, "the swap to be in flight", func() bool {
		act.mu.Lock()
		defer act.mu.Unlock()
		pr := act.pools["lab/heavy-slot"]
		return pr != nil && pr.swapping
	})
	for i := 1; i < calls; i++ {
		go serve()
	}

	got := make([]int, 0, calls)
	deadline := time.After(10 * time.Second)
	for len(got) < calls {
		select {
		case code := <-codes:
			got = append(got, code)
		case <-deadline:
			t.Fatalf("only %d of %d held requests returned", len(got), calls)
		}
	}
	for _, code := range got {
		if code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (the swap was abandoned at its deadline)", code)
		}
	}
	if n := fb.calls.Load(); n != 0 {
		t.Errorf("backend calls = %d, want 0 (the target never became resident)", n)
	}
	waitFor(t, "the abandoned target to be scaled back down", func() bool {
		return fake.replicaCount("coder") == 0
	})
}
