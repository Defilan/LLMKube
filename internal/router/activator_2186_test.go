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
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// routerIssue2186 labels every metric series these tests read. The other
// activator tests take deltas on their own router labels, so a shared label
// would make the deltas depend on test order.
const routerIssue2186 = "r2186"

// TestIssue2186EveryWaiterSeesDeadline is #2186: three callers waiting when a
// swap is abandoned at its deadline must each get that swap's error and none of
// them may start another load of the target that was just abandoned. Before the
// fix the first caller to wake consumed the recorded error and the rest saw no
// error, no in-flight swap and a non-resident target, so each started its own
// swap.
func TestIssue2186EveryWaiterSeesDeadline(t *testing.T) {
	fake := newFakeMemberController()
	// The target's pod never comes up, so the swap is abandoned at its budget.
	fake.noAutoReady = true
	fake.setPhase("judge", modelReadyPhase)
	a := NewActivator(context.Background(), fake, routerIssue2186, nil)
	failures := prommetrics.ModelPoolSwapFailuresTotal.
		WithLabelValues(routerIssue2186, "heavy-slot", "coder", "deadline")
	before := testutil.ToFloat64(failures)

	errs := acquireWithWaiters(t, a, 200*time.Millisecond, nil, nil)

	for i, err := range errs {
		if !errors.Is(err, ErrSwapDeadlineExceeded) {
			t.Errorf("caller %d err = %v, want ErrSwapDeadlineExceeded", i, err)
		}
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("Activate(coder) called %d times, want 1 (one abandoned load, not one per waiter)", got)
	}
	if got := testutil.ToFloat64(failures) - before; got != 1 {
		t.Errorf("swap_failures_total{reason=deadline} delta = %v, want 1", got)
	}
	waitFor(t, "the abandoned target to be scaled back down", func() bool {
		return fake.replicaCount("coder") == 0
	})
}

// TestIssue2186EveryWaiterSeesActivationError is the same shape for the failure
// that is not the deadline: every caller waiting when the activation failed gets
// that failure, and only one load was ever attempted.
func TestIssue2186EveryWaiterSeesActivationError(t *testing.T) {
	fake := newFakeMemberController()
	fake.setPhase("judge", modelReadyPhase)
	// Activate blocks on the gate so every caller is registered on the swap
	// before it can fail.
	fake.activateGate = make(chan struct{})
	fake.activateFail = map[string]bool{"coder": true}
	var held atomic.Int64
	deact := &gatedDeactivate{fakeMemberController: fake, held: &held}
	a := NewActivator(context.Background(), deact, routerIssue2186, nil)
	failures := prommetrics.ModelPoolSwapFailuresTotal.
		WithLabelValues(routerIssue2186, "heavy-slot", "coder", "error")
	before := testutil.ToFloat64(failures)

	errs := acquireWithWaiters(t, a, 5*time.Second, &held, func() { close(fake.activateGate) })

	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d succeeded; want the swap failure", i)
		}
		if errors.Is(err, ErrHoldBudgetExceeded) || errors.Is(err, ErrSwapDeadlineExceeded) {
			t.Errorf("caller %d err = %v, want the swap's own failure", i, err)
		}
		if !strings.Contains(err.Error(), "model pool swap failed for member coder") {
			t.Errorf("caller %d err = %v, want the failure for coder", i, err)
		}
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("Activate(coder) called %d times, want 1 (the co-waiters must not start a fresh swap)", got)
	}
	if got := testutil.ToFloat64(failures) - before; got != 1 {
		t.Errorf("swap_failures_total{reason=error} delta = %v, want 1", got)
	}
	// The failed swap has to scale its own target down: every waiter returns the
	// recorded failure instead of giving up, so none of them reaches the
	// deferred deactivate. Assert it happened while waiters were still
	// registered; after they return the deferred path scales the target down
	// anyway, and a final-state check would pass for the wrong reason.
	if deact.seenAt == 0 {
		t.Error("the failed swap never scaled its target down while its waiters were still registered")
	}
	waitFor(t, "the failed target to be scaled back down", func() bool {
		return fake.replicaCount("coder") == 0
	})
}

// gatedDeactivate records, on the first Deactivate for a member, the value of a
// counter the test keeps of callers registered on the pool and not yet returned.
type gatedDeactivate struct {
	*fakeMemberController
	held   *atomic.Int64
	mu     sync.Mutex
	seenAt int64
}

func (g *gatedDeactivate) Deactivate(ctx context.Context, namespace, isvc string) error {
	// Sample before touching the fake and without any fake lock: the swap path
	// calls this with the activator's lock held.
	n := g.held.Load()
	g.mu.Lock()
	if g.seenAt == 0 && n > 0 {
		g.seenAt = n
	}
	g.mu.Unlock()
	return g.fakeMemberController.Deactivate(ctx, namespace, isvc)
}

// acquireWithWaiters runs three Acquire calls that share one swap and returns
// their errors. The first caller starts the swap, the other two join it while it
// is in flight, and release runs only once all three are registered, so no
// caller can have seen the swap's outcome before the last one joined. held, when
// non-nil, counts the callers registered and not yet returned.
func acquireWithWaiters(t *testing.T, a *Activator, budget time.Duration, held *atomic.Int64, release func()) []error {
	t.Helper()
	const calls = 3
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	acquire := func() {
		defer wg.Done()
		// Every hold budget outlives the swap by far, so the swap's outcome,
		// not a caller's own timer, decides the result.
		ctx, cancel := context.WithTimeout(context.Background(), budget+10*time.Second)
		defer cancel()
		if held != nil {
			held.Add(1)
		}
		rel, err := a.Acquire(ctx, budgetPool("coder", budget))
		if err == nil {
			rel()
		}
		if held != nil {
			held.Add(-1)
		}
		errs <- err
	}

	wg.Add(1)
	go acquire()
	waitFor(t, "the swap to be in flight", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		pr := a.pools["lab/heavy-slot"]
		return pr != nil && pr.swapping
	})
	for i := 1; i < calls; i++ {
		wg.Add(1)
		go acquire()
	}
	waitFor(t, "all three callers to be registered on the swap", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.pools["lab/heavy-slot"].waiting["coder"] == calls
	})
	if release != nil {
		release()
	}
	wg.Wait()
	close(errs)

	got := make([]error, 0, calls)
	for err := range errs {
		got = append(got, err)
	}
	if len(got) != calls {
		t.Fatalf("%d of %d callers returned", len(got), calls)
	}
	return got
}

// TestIssue2186ArrivalAfterFailureStartsFreshLoad pins the other half of #2186:
// a request that arrives after the failure is not handed that failure and
// starts exactly one fresh load. It gives the same result before and after the
// fix.
func TestIssue2186ArrivalAfterFailureStartsFreshLoad(t *testing.T) {
	fake := newFakeMemberController()
	fake.noAutoReady = true
	fake.setPhase("judge", modelReadyPhase)
	a := NewActivator(context.Background(), fake, routerIssue2186, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.Acquire(ctx, budgetPool("coder", 150*time.Millisecond)); !errors.Is(err, ErrSwapDeadlineExceeded) {
		t.Fatalf("first Acquire err = %v, want ErrSwapDeadlineExceeded", err)
	}
	waitFor(t, "the pool to stop swapping", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.pools["lab/heavy-slot"].swapping
	})

	// The request arrives now, after the failure: it gets its own load.
	if _, err := a.Acquire(ctx, budgetPool("coder", 150*time.Millisecond)); !errors.Is(err, ErrSwapDeadlineExceeded) {
		t.Fatalf("second Acquire err = %v, want its own ErrSwapDeadlineExceeded", err)
	}
	if got := fake.activateCount("coder"); got != 2 {
		t.Errorf("Activate(coder) called %d times, want 2 (a fresh load for the new arrival)", got)
	}
}
