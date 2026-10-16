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

package watchcanary

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
)

// testCanary builds a Canary without an informer, for tests of the checker
// and event filtering alone. New() requires a real cache; the wiring is
// covered by the envtest test.
func testCanary(t *testing.T, mutate func(*Canary)) *Canary {
	t.Helper()
	c := &Canary{
		opts: Options{
			Namespace: "llmkube-system",
			Interval:  time.Second,
			FailAfter: 5 * time.Second,
			Now:       func() time.Time { return time.Unix(1750000000, 0) },
			Log:       logr.Discard(),
		},
	}
	if mutate != nil {
		mutate(c)
	}
	return c
}

func TestLivenessCheck(t *testing.T) {
	now := time.Unix(1750000000, 0)

	t.Run("passes before the write loop has started", func(t *testing.T) {
		c := testCanary(t, nil)
		if err := c.LivenessCheck()(nil); err != nil {
			t.Fatalf("expected a not-started canary to pass, got %v", err)
		}
	})

	t.Run("passes while nothing has been observed but the window is open", func(t *testing.T) {
		c := testCanary(t, func(c *Canary) {
			c.started = true
			c.startedAt = now.Add(-2 * time.Second)
		})
		if err := c.LivenessCheck()(nil); err != nil {
			t.Fatalf("expected an open startup window to pass, got %v", err)
		}
	})

	t.Run("fails when nothing was ever observed past the window", func(t *testing.T) {
		c := testCanary(t, func(c *Canary) {
			c.started = true
			c.startedAt = now.Add(-10 * time.Second)
		})
		err := c.LivenessCheck()(nil)
		if err == nil {
			t.Fatal("expected the liveness check to fail past fail-after with no observation")
		}
		if err.Error() == "" {
			t.Fatalf("expected a descriptive error, got %v", err)
		}
	})

	t.Run("passes on a fresh observation", func(t *testing.T) {
		c := testCanary(t, func(c *Canary) {
			c.started = true
			c.startedAt = now.Add(-time.Hour)
			c.haveObserved = true
			c.lastObserved = now.Add(-time.Second)
		})
		if err := c.LivenessCheck()(nil); err != nil {
			t.Fatalf("expected a fresh observation to pass, got %v", err)
		}
	})

	t.Run("fails on a stale observation", func(t *testing.T) {
		c := testCanary(t, func(c *Canary) {
			c.started = true
			c.startedAt = now.Add(-time.Hour)
			c.haveObserved = true
			c.lastObserved = now.Add(-10 * time.Second)
		})
		if err := c.LivenessCheck()(nil); err == nil {
			t.Fatal("expected a stale observation to fail the liveness check")
		}
	})
}

// TestObservedFiltersToTheCanaryLease pins the filter that keeps foreign
// lease events from faking a healthy pipeline: only the canary lease in the
// canary's namespace may advance lastObserved.
func TestObservedFiltersToTheCanaryLease(t *testing.T) {
	now := time.Unix(1750000000, 0)
	c := testCanary(t, func(c *Canary) {
		c.started = true
		c.startedAt = now.Add(-time.Hour)
	})
	c.sawWrite = true

	foreign := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "llmkube-system", Name: "some-other-lease",
	}}
	c.observed(foreign)
	if _, ok := c.Observed(); ok {
		t.Fatal("a foreign lease must not count as an observation")
	}

	wrongNS := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: "other", Name: LeaseName,
	}}
	c.observed(wrongNS)
	if _, ok := c.Observed(); ok {
		t.Fatal("a canary lease in another namespace must not count as an observation")
	}

	mine := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: c.opts.Namespace, Name: LeaseName,
	}}
	c.observed(mine)
	if got, ok := c.Observed(); !ok || !got.Equal(now) {
		t.Fatalf("expected the canary lease to be observed at %v, got %v ok=%v", now, got, ok)
	}
	if !c.up {
		t.Fatal("expected up after a write followed by an observation")
	}

	// A delete delivered as an unknown tombstone still proves the pipeline
	// moved and must count.
	tombstone := toolscache.DeletedFinalStateUnknown{Key: "x", Obj: mine}
	c.haveObserved = false
	c.observed(tombstone)
	if _, ok := c.Observed(); !ok {
		t.Fatal("expected a deleted-final-state tombstone to count as an observation")
	}
}

// TestObservedBeforeWriteDoesNotSetUp pins the up-gauge semantics: an
// observed event before the canary has written anything (the initial List of
// a lease left by a previous pod) must not report a full cycle.
func TestObservedBeforeWriteDoesNotSetUp(t *testing.T) {
	c := testCanary(t, nil)
	c.observed(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: c.opts.Namespace, Name: LeaseName,
	}})
	if _, ok := c.Observed(); !ok {
		t.Fatal("expected the event to be observed")
	}
	if c.up {
		t.Fatal("expected up to stay false before the canary's own write")
	}
}

func TestNewValidation(t *testing.T) {
	valid := Options{
		Namespace: "llmkube-system",
		Interval:  time.Second,
		FailAfter: 5 * time.Second,
	}

	if _, err := New(context.Background(), Options{}); err == nil {
		t.Fatal("expected an error for a zero interval")
	}
	if _, err := New(context.Background(), Options{Interval: time.Second}); err == nil {
		t.Fatal("expected an error for an empty namespace")
	}
	if _, err := New(context.Background(), valid); err == nil {
		t.Fatal("expected an error without a reader, writer and cache")
	}
	if _, err := New(context.Background(), Options{Namespace: "ns", Interval: time.Second, FailAfter: time.Second}); err == nil {
		t.Fatal("expected an error when fail-after does not exceed the interval")
	}
}

func TestResolveNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	if got := ResolveNamespace("explicit", "leader"); got != "explicit" {
		t.Errorf("expected the flag to win, got %q", got)
	}
	if got := ResolveNamespace("", "leader"); got != "leader" {
		t.Errorf("expected the leader-election namespace fallback, got %q", got)
	}
	t.Setenv("POD_NAMESPACE", "pod-ns")
	if got := ResolveNamespace("", ""); got != "pod-ns" {
		t.Errorf("expected the POD_NAMESPACE fallback, got %q", got)
	}
	t.Setenv("POD_NAMESPACE", "")
	// The projected serviceaccount file does not exist in the test
	// environment, so the last fallback resolves to empty and the caller
	// disables the canary.
	if got := ResolveNamespace("", ""); got != "" {
		t.Errorf("expected empty when nothing resolves, got %q", got)
	}
}
