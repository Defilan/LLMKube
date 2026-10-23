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
	"fmt"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func fresh(running, waiting, kv float64, inflightAt int64) *Sample {
	return &Sample{Running: running, Waiting: waiting, KVUsage: kv, At: t0, InFlightAt: inflightAt}
}

func TestLoad(t *testing.T) {
	cases := []struct {
		name string
		c    Candidate
		now  time.Time
		want int64
	}{
		{"no sample uses in-flight", Candidate{InFlight: 3}, t0, 3},
		{"fresh sample, no requests since", Candidate{InFlight: 2, Sample: fresh(4, 1, 0.2, 2)}, t0.Add(time.Second), 5},
		{"fresh sample plus requests sent since", Candidate{InFlight: 5, Sample: fresh(4, 1, 0.2, 2)}, t0.Add(time.Second), 8},
		{"in-flight fell since the sample", Candidate{InFlight: 0, Sample: fresh(4, 1, 0.2, 2)}, t0.Add(time.Second), 5},
		{"fractional gauges round up", Candidate{Sample: fresh(1.2, 0, 0, 0)}, t0, 2},
		{"stale sample falls back to in-flight", Candidate{InFlight: 1, Sample: fresh(30, 9, 0.5, 0)}, t0.Add(StaleAfter + time.Millisecond), 1},
	}
	for _, tc := range cases {
		if got := Load(tc.c, tc.now); got != tc.want {
			t.Errorf("%s: Load = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestEligibleDropsKVFullWhileAnyIsBelow(t *testing.T) {
	cands := []Candidate{
		{Endpoint: "a", Sample: fresh(1, 0, 0.95, 0)},
		{Endpoint: "b", Sample: fresh(1, 0, 0.50, 0)},
		{Endpoint: "c"}, // no sample: not known to be full
	}
	got := Eligible(cands, t0)
	if len(got) != 2 || got[0].Endpoint != "b" || got[1].Endpoint != "c" {
		t.Errorf("Eligible = %v, want b and c", got)
	}
	allFull := []Candidate{{Endpoint: "a", Sample: fresh(1, 0, 0.95, 0)}, {Endpoint: "b", Sample: fresh(1, 0, 0.91, 0)}}
	if got := Eligible(allFull, t0); len(got) != 2 {
		t.Errorf("all full: Eligible kept %d, want both", len(got))
	}
	stale := []Candidate{{Endpoint: "a", Sample: fresh(1, 0, 0.99, 0)}, {Endpoint: "b"}}
	if got := Eligible(stale, t0.Add(StaleAfter+time.Second)); len(got) != 2 {
		t.Errorf("a stale KV reading must not exclude a pod; kept %d", len(got))
	}
}

func TestPickLeastLoaded(t *testing.T) {
	cands := []Candidate{
		{Endpoint: "a", InFlight: 4},
		{Endpoint: "b", InFlight: 1},
		{Endpoint: "c", InFlight: 1},
	}
	ep, res := PickLeastLoaded(cands, t0, 0)
	if res != LeastLoaded || (ep != "b" && ep != "c") {
		t.Fatalf("pick = %s/%s, want b or c, least_loaded", ep, res)
	}
	seen := map[string]bool{}
	for start := 0; start < 6; start++ {
		ep, _ := PickLeastLoaded(cands, t0, start)
		seen[ep] = true
	}
	if !seen["b"] || !seen["c"] || seen["a"] {
		t.Errorf("tie rotation picked %v, want b and c only", seen)
	}
}

func endpoints(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("http://10.0.0.%d:8000", i+1)
	}
	return out
}

func idle(eps []string) []Candidate {
	out := make([]Candidate, len(eps))
	for i, ep := range eps {
		out[i] = Candidate{Endpoint: ep}
	}
	return out
}

func TestRingStickyAndDeterministic(t *testing.T) {
	eps := endpoints(8)
	a, b := NewRing(eps), NewRing(append([]string(nil), eps...))
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("session-%d", i)
		ep1, res := a.Pick(key, idle(eps), t0, 125)
		ep2, _ := b.Pick(key, idle(eps), t0, 125)
		if res != Sticky || ep1 != a.Home(key) {
			t.Fatalf("%s: idle pick %s/%s, want its home %s sticky", key, ep1, res, a.Home(key))
		}
		if ep1 != ep2 {
			t.Fatalf("%s: two rings disagree (%s vs %s)", key, ep1, ep2)
		}
	}
}

func TestRingSpillsWhenHomeIsHot(t *testing.T) {
	eps := endpoints(4)
	r := NewRing(eps)
	key := "agent-42"
	home := r.Home(key)
	cands := idle(eps)
	for i := range cands {
		if cands[i].Endpoint == home {
			cands[i].InFlight = 10 // far above 125% of the average
		}
	}
	ep, res := r.Pick(key, cands, t0, 125)
	if ep == home || res != Spill {
		t.Fatalf("hot home: pick %s/%s, want a spill away from %s", ep, res, home)
	}
	// The bound: no candidate is chosen above it.
	var sum int64
	for _, c := range cands {
		sum += Load(c, t0)
	}
	bound := ceilDiv((sum+1)*125, 100*int64(len(cands)))
	for _, c := range cands {
		if c.Endpoint == ep && Load(c, t0)+1 > bound {
			t.Errorf("picked %s with load %d over bound %d", ep, Load(c, t0), bound)
		}
	}
}

func TestRingHomeMissingFromCandidatesSpills(t *testing.T) {
	eps := endpoints(4)
	r := NewRing(eps)
	key := "agent-7"
	home := r.Home(key)
	var cands []Candidate
	for _, c := range idle(eps) {
		if c.Endpoint != home { // the home pod is quarantined
			cands = append(cands, c)
		}
	}
	ep, res := r.Pick(key, cands, t0, 125)
	if ep == "" || ep == home || res != Spill {
		t.Fatalf("pick %s/%s, want another pod as a spill", ep, res)
	}
	if ep2, res2 := r.Pick(key, idle(eps), t0, 125); ep2 != home || res2 != Sticky {
		t.Errorf("after recovery: pick %s/%s, want home %s sticky", ep2, res2, home)
	}
}

func TestRingMovesFewKeysWhenAPodJoins(t *testing.T) {
	before, after := NewRing(endpoints(8)), NewRing(endpoints(9))
	moved := 0
	const keys = 10000
	for i := 0; i < keys; i++ {
		k := fmt.Sprintf("k%d", i)
		if before.Home(k) != after.Home(k) {
			moved++
		}
	}
	frac := float64(moved) / keys
	if frac < 0.5/9 || frac > 2.0/9 {
		t.Errorf("moved %.3f of keys when one of 9 pods joined, want between %.3f and %.3f", frac, 0.5/9, 2.0/9)
	}
}

func TestRingPickWithNoCandidates(t *testing.T) {
	if ep, _ := NewRing(endpoints(3)).Pick("k", nil, t0, 125); ep != "" {
		t.Errorf("no candidates: pick %q, want empty", ep)
	}
}
