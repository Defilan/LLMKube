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

package router

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitForRepro polls cond until it holds. It is defined here rather than reused
// so this file compiles unchanged against the base branch, whose waitFor has a
// different signature.
func waitForRepro(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// reproPools returns the reporter's two-member pool (qwen-27b, muse-glimmer)
// as a BackendPool for the named member.
func reproPools(member string) *BackendPool {
	return &BackendPool{
		Name:      "heavy-slot",
		Namespace: "lab",
		Member:    member,
		Members:   []string{"qwen-27b", "muse-glimmer"},
	}
}

// reproReadyDelay makes the fake member report Ready after d instead of at
// once, so a call can give up while the member is still in its boot and a
// caller can observe the difference. The returned func marks the member Ready
// and stops the timer, so the test leaves no goroutine behind holding a metric
// lock while a later test collects the registry.
func reproReadyDelay(f *fakeMemberController, member string, d time.Duration) func() {
	f.mu.Lock()
	f.noAutoReady = true
	f.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-time.After(d):
			f.setPhase(member, modelReadyPhase)
		case <-time.After(5 * time.Second):
		}
	}()
	return func() {
		f.setPhase(member, modelReadyPhase)
		<-done
	}
}

// TestActivatorKeepsSwapAfterLastWaiterLeaves is #2130: the only caller waiting
// for a swap target gives up during the target's boot, and the load must still
// run to completion so the member comes up warm for the retry, instead of being
// scaled back to zero and cold-loaded again on every request.
func TestActivatorKeepsSwapAfterLastWaiterLeaves(t *testing.T) {
	fake := newFakeMemberController()
	defer reproReadyDelay(fake, "muse-glimmer", 150*time.Millisecond)()
	a := NewActivator(context.Background(), fake, "r", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := a.Acquire(ctx, reproPools("muse-glimmer")); !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("Acquire err = %v, want ErrHoldBudgetExceeded", err)
	}

	a.mu.Lock()
	swapping := a.pools["lab/heavy-slot"].swapping
	a.mu.Unlock()
	if !swapping {
		t.Fatal("the swap was dropped when its last waiter left; a started load must keep running")
	}
	if got := fake.deactivateCount("muse-glimmer"); got != 0 {
		t.Fatalf("Deactivate(muse-glimmer) called %d times; the load was thrown away", got)
	}

	// The model finishes loading: the member becomes resident and nothing
	// scaled it back.
	waitForRepro(t, "the swap to publish muse-glimmer as resident", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		pr := a.pools["lab/heavy-slot"]
		return !pr.swapping && pr.resident == "muse-glimmer"
	})
	if got := fake.replicaCount("muse-glimmer"); got != 1 {
		t.Fatalf("muse-glimmer replicas = %d, want 1 (loaded, not scaled back)", got)
	}
	if got := fake.activateCount("muse-glimmer"); got != 1 {
		t.Fatalf("muse-glimmer activate count = %d, want 1", got)
	}

	// The retry is served on the warm member with no second load.
	retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
	defer retryCancel()
	release, err := a.Acquire(retryCtx, reproPools("muse-glimmer"))
	if err != nil {
		t.Fatalf("retry Acquire: %v", err)
	}
	release()
	if got := fake.activateCount("muse-glimmer"); got != 1 {
		t.Fatalf("muse-glimmer activate count = %d after the retry, want 1", got)
	}
}

// TestActivatorHoldsIncumbentRequesterBehindUnwaitedSwap pins the cost of
// keeping a started load: a request for the member that is still loaded waits
// out a swap nobody else wants, bounded by swapBudget, rather than reading a
// residency belief the controller has already invalidated by draining it.
func TestActivatorHoldsIncumbentRequesterBehindUnwaitedSwap(t *testing.T) {
	fake := newFakeMemberController()
	fake.setPhase("qwen-27b", modelReadyPhase)
	// muse-glimmer is mid-boot for longer than the second caller's hold budget.
	defer reproReadyDelay(fake, "muse-glimmer", 400*time.Millisecond)()
	a := NewActivator(context.Background(), fake, "r", nil)

	// qwen-27b is resident.
	rel, err := a.Acquire(context.Background(), reproPools("qwen-27b"))
	if err != nil {
		t.Fatalf("cold Acquire: %v", err)
	}
	rel()

	// A muse-glimmer caller starts a swap and gives up mid-load.
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer firstCancel()
	if _, err := a.Acquire(firstCtx, reproPools("muse-glimmer")); !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("first muse Acquire err = %v, want ErrHoldBudgetExceeded", err)
	}
	a.mu.Lock()
	swapping := a.pools["lab/heavy-slot"].swapping
	a.mu.Unlock()
	if !swapping {
		t.Fatal("the swap is no longer in flight; the hold under test cannot be observed")
	}

	// A request for the incumbent while that swap runs is held behind it.
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer secondCancel()
	if _, err := a.Acquire(secondCtx, reproPools("qwen-27b")); !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("incumbent request err = %v, want ErrHoldBudgetExceeded while an unwaited swap is loading", err)
	}
}
