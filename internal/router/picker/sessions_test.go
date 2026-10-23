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

package picker

import (
	"sync"
	"testing"
	"time"
)

// pod builds a candidate with a fresh sample at t0.
func pod(ep string, running, waiting, kv float64) Candidate {
	return Candidate{Endpoint: ep, Sample: &Sample{Running: running, Waiting: waiting, KVUsage: kv, At: t0}}
}

// bare builds a candidate with no sample and the given in-flight count.
func bare(ep string, inflight int64) Candidate {
	return Candidate{Endpoint: ep, InFlight: inflight}
}

func place(t *testing.T, s *Sessions, key uint64, cands []Candidate, now time.Time, want string, wantRes Result, wantReason Reason) {
	t.Helper()
	ep, res, reason := s.Place(key, cands, now, 0, "")
	if ep != want || res != wantRes || reason != wantReason {
		t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", ep, res, reason, want, wantRes, wantReason)
	}
}

func TestSessionsNewKeyGoesLeastLoadedAndBinds(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 10, 0, 0.1), pod("b", 2, 0, 0.1), pod("c", 7, 0, 0.1)}
	place(t, s, 1, cands, t0, "b", LeastLoaded, ReasonNone)
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	place(t, s, 1, cands, t0, "b", Sticky, ReasonNone)
}

func TestSessionsStaysWhileNothingQueues(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 0, 0, 0.1), pod("b", 1, 0, 0.1)}
	place(t, s, 1, cands, t0, "a", LeastLoaded, ReasonNone)
	cands = []Candidate{pod("a", 30, 0, 0.1), pod("b", 1, 0, 0.1)}
	place(t, s, 1, cands, t0, "a", Sticky, ReasonNone)
}

// bindToA binds key 1 to pod "a" by making it the only candidate.
func bindToA(t *testing.T, s *Sessions, now time.Time) {
	t.Helper()
	place(t, s, 1, []Candidate{pod("a", 0, 0, 0.1)}, now, "a", LeastLoaded, ReasonNone)
}

func TestSessionsSpillsOnQueueAndMargin(t *testing.T) {
	cases := []struct {
		leastRunning float64
		want         string
		res          Result
		reason       Reason
	}{
		{15, "b", Spill, ReasonWaiting},
		{18, "b", Spill, ReasonWaiting},
		{19, "a", Sticky, ReasonNone},
	}
	for _, c := range cases {
		s := NewSessions(0, 10)
		bindToA(t, s, t0)
		cands := []Candidate{pod("a", 20, 2, 0.1), pod("b", c.leastRunning, 0, 0.1)}
		place(t, s, 1, cands, t0, c.want, c.res, c.reason)
	}
}

func TestSessionsDoesNotReturnHomeAfterSpill(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 15, 0, 0.1)}, t0, "b", Spill, ReasonWaiting)
	place(t, s, 1, []Candidate{pod("a", 0, 0, 0.1), pod("b", 15, 0, 0.1)}, t0.Add(time.Second), "b", Sticky, ReasonNone)
}

func TestSessionsCooldown(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 0, 0, 0.1)}, t0, "b", Spill, ReasonWaiting)
	// b is now under pressure with a gap of 6.
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	mk := func(d time.Duration) []Candidate {
		now := at(d)
		a := pod("a", 0, 0, 0.1)
		b := pod("b", 3, 3, 0.1)
		a.Sample.At, b.Sample.At = now, now
		return []Candidate{a, b}
	}
	place(t, s, 1, mk(30*time.Second), at(30*time.Second), "b", Sticky, ReasonNone)
	place(t, s, 1, mk(61*time.Second), at(61*time.Second), "a", Spill, ReasonWaiting)
}

func TestSessionsMigratesWhenPodLeaves(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 2, 0.1), pod("b", 0, 0, 0.1)}, t0, "b", Spill, ReasonWaiting)
	// b leaves inside the cooldown.
	now := t0.Add(5 * time.Second)
	c := pod("c", 1, 0, 0.1)
	c.Sample.At = now
	place(t, s, 1, []Candidate{c}, now, "c", Migrated, ReasonNotReady)
	place(t, s, 1, []Candidate{c}, now, "c", Sticky, ReasonNone)
}

func TestSessionsKVFullAloneDoesNotMove(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 5, 0, 0.95), pod("b", 0, 0, 0.4)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsKVFullAndQueuedSpillsAtLevelLoad(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 4, 1, 0.95), pod("b", 5, 0, 0.4)}, t0, "b", Spill, ReasonKVFull)
}

func TestSessionsAllKVFullStays(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	cands := []Candidate{pod("a", 5, 2, 0.95), pod("b", 5, 2, 0.95)}
	place(t, s, 1, cands, t0, "a", Sticky, ReasonNone)
	place(t, s, 1, cands, t0.Add(time.Second), "a", Sticky, ReasonNone)
}

func TestSessionsSinglePodNeverSpills(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 5, 10, 0.5)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsNoSampleUsesInFlightMargin(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{bare("a", 12), bare("b", 5)}, t0, "a", Sticky, ReasonNone)
	place(t, s, 1, []Candidate{bare("a", 13), bare("b", 5)}, t0, "b", Spill, ReasonWaiting)
}

func TestSessionsExpiredKeyIsNew(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 5, 0, 0.1), pod("b", 1, 0, 0.1)}
	place(t, s, 1, cands, t0, "b", LeastLoaded, ReasonNone)
	later := t0.Add(31 * time.Minute)
	a, b := pod("a", 1, 0, 0.1), pod("b", 5, 0, 0.1)
	a.Sample.At, b.Sample.At = later, later
	place(t, s, 1, []Candidate{a, b}, later, "a", LeastLoaded, ReasonNone)
}

func TestSessionsSweep(t *testing.T) {
	s := NewSessions(10*time.Minute, 10)
	cands := []Candidate{pod("a", 0, 0, 0.1)}
	for i, d := range []time.Duration{0, 5 * time.Minute, 9 * time.Minute} {
		now := t0.Add(d)
		place(t, s, uint64(i+1), cands, now, "a", LeastLoaded, ReasonNone)
	}
	if got := s.Sweep(t0.Add(12 * time.Minute)); got != 2 {
		t.Fatalf("Sweep = %d, want 2", got)
	}
	if got := s.Sweep(t0.Add(20 * time.Minute)); got != 0 {
		t.Fatalf("Sweep = %d, want 0", got)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
}

func TestSessionsFullTableFallsBackToRingHome(t *testing.T) {
	s := NewSessions(0, 2)
	cands := []Candidate{pod("a", 0, 0, 0.1), pod("b", 5, 0, 0.1)}
	place(t, s, 1, cands, t0, "a", LeastLoaded, ReasonNone)
	place(t, s, 2, cands, t0, "a", LeastLoaded, ReasonNone)
	ep, res, reason := s.Place(3, cands, t0, 0, "b")
	if ep != "b" || res != Fallback || reason != ReasonNone {
		t.Fatalf("got (%q, %q, %q), want (b, fallback, none)", ep, res, reason)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	place(t, s, 1, cands, t0, "a", Sticky, ReasonNone)
	ep, res, _ = s.Place(4, cands, t0, 0, "gone")
	if ep != "a" || res != Fallback {
		t.Fatalf("got (%q, %q), want (a, fallback)", ep, res)
	}
}

func TestSessionsFullOfExpiredStillBinds(t *testing.T) {
	s := NewSessions(10*time.Minute, 2)
	place(t, s, 1, []Candidate{pod("a", 0, 0, 0.1)}, t0, "a", LeastLoaded, ReasonNone)
	place(t, s, 2, []Candidate{pod("a", 0, 0, 0.1)}, t0, "a", LeastLoaded, ReasonNone)
	later := t0.Add(11 * time.Minute)
	c := pod("a", 0, 0, 0.1)
	c.Sample.At = later
	place(t, s, 3, []Candidate{c}, later, "a", LeastLoaded, ReasonNone)
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
}

func TestSessionsNoCandidates(t *testing.T) {
	s := NewSessions(0, 10)
	place(t, s, 1, nil, t0, "", "", ReasonNone)
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
}

func TestSessionsConcurrentFirstRequestsBindOnce(t *testing.T) {
	s := NewSessions(0, 10)
	cands := []Candidate{pod("a", 1, 0, 0.1), pod("b", 1, 0, 0.1), pod("c", 1, 0, 0.1)}
	const n = 64
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _, _ = s.Place(7, cands, t0, i, "")
		}(i)
	}
	wg.Wait()
	for i, ep := range got {
		if ep != got[0] {
			t.Fatalf("call %d got %q, want %q", i, ep, got[0])
		}
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
}

func TestSessionsKVFullQueuedShedsToStaleTargetByGap(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 10, 0.95), bare("b", 0)}, t0, "b", Spill, ReasonKVFull)
}

func TestSessionsKVFullQueuedStaysWhenStaleTargetGapSmall(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 10, 0.95), bare("b", 27)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsAllKVFullStaysEvenWithLargeGap(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	place(t, s, 1, []Candidate{pod("a", 20, 10, 0.95), pod("b", 2, 0, 0.95)}, t0, "a", Sticky, ReasonNone)
}

func TestSessionsQueuedNotKVFullNeverTargetsFullPod(t *testing.T) {
	s := NewSessions(0, 10)
	bindToA(t, s, t0)
	cands := []Candidate{pod("a", 15, 5, 0.5), pod("b", 1, 0, 0.95), pod("c", 15, 0, 0.5)}
	place(t, s, 1, cands, t0, "c", Spill, ReasonWaiting)
}
