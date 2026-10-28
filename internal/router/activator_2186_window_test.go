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
	"errors"
	"sync"
	"testing"
	"time"
)

// TestIssue2186LateArrivalKeepsTheFailure drives a request into the window it
// shares with #2186: a swap has failed but has not published its outcome yet.
// The request that arrives there joins the swap that failed rather than
// replacing it, so it gets that outcome alongside the waiter already
// registered, and neither starts a second load.
//
// This test needs Activator.beforePublishFailure, so it cannot run against the
// base revision; the base-side reproduction is TestIssue2186EveryWaiterSeesDeadline.
func TestIssue2186LateArrivalKeepsTheFailure(t *testing.T) {
	fake := newFakeMemberController()
	fake.noAutoReady = true
	fake.setPhase("judge", modelReadyPhase)
	a := NewActivator(context.Background(), fake, routerIssue2186, nil)

	// Hold the window open: the swap goroutine stops in it until released.
	entered := make(chan struct{})
	releaseWindow := make(chan struct{})
	var once sync.Once
	a.beforePublishFailure = func() {
		once.Do(func() {
			close(entered)
			<-releaseWindow
		})
	}

	firstDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := a.Acquire(ctx, budgetPool("coder", 150*time.Millisecond))
		firstDone <- err
	}()

	<-entered
	a.mu.Lock()
	swapping := a.pools["lab/heavy-slot"].swapping
	_, recorded := a.pools["lab/heavy-slot"].swapErr["coder"]
	a.mu.Unlock()
	if !swapping || recorded {
		t.Fatalf("swapping = %v, recorded = %v inside the window; want the swap still in flight and nothing recorded yet", swapping, recorded)
	}

	// A request arriving now joins this pool while the outcome is unpublished.
	arrivalDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := a.Acquire(ctx, budgetPool("coder", 150*time.Millisecond))
		arrivalDone <- err
	}()
	waitFor(t, "the late arrival to register on the pool", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.pools["lab/heavy-slot"].waiting["coder"] == 2
	})

	close(releaseWindow)

	if err := <-firstDone; !errors.Is(err, ErrSwapDeadlineExceeded) {
		t.Errorf("waiter err = %v, want ErrSwapDeadlineExceeded (the swap it waited on)", err)
	}
	if err := <-arrivalDone; !errors.Is(err, ErrSwapDeadlineExceeded) {
		t.Errorf("late arrival err = %v, want ErrSwapDeadlineExceeded (the swap it joined)", err)
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("Activate(coder) called %d times, want 1 (the arrival joined the swap, it did not start one)", got)
	}
}
