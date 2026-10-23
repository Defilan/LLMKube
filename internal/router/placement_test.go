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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/defilantech/llmkube/internal/router/picker"
)

func balancedDispatcher(headers []string, n int, opts ...DispatcherOption) (*Dispatcher, *Backend, []string) {
	cfg := &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
		Balancing:      &Balancing{Mode: BalancingLoadAware, SessionHeaders: headers},
		Metrics:        &vllmProfile,
	}}}
	d := NewDispatcher(cfg, append([]DispatcherOption{WithQuarantineDuration(time.Hour)}, opts...)...)
	eps := make([]string, n)
	for i := range eps {
		eps[i] = fmt.Sprintf("http://10.0.1.%d:8000", i+1)
	}
	d.SetEndpoints("qwen", eps)
	return d, &d.cfg.Backends[0], eps
}

func TestSessionKeyHeaderOrder(t *testing.T) {
	names := []string{"x-session-affinity", "x-openwebui-chat-id", "x-opencode-session"}
	h := http.Header{}
	h.Set("X-OpenWebUI-Chat-Id", "chat-1")
	h.Set("X-Opencode-Session", "oc-1")
	if got := sessionKey(h, names); got != "chat-1" {
		t.Errorf("key = %q, want the second header (first is absent)", got)
	}
	h.Set("X-Session-Affinity", "")
	if got := sessionKey(h, names); got != "chat-1" {
		t.Errorf("an empty first header must fall through; key = %q", got)
	}
	h.Set("X-Session-Affinity", "s-9")
	if got := sessionKey(h, names); got != "s-9" {
		t.Errorf("key = %q, want the first header", got)
	}
	if got := sessionKey(h, nil); got != "" {
		t.Errorf("no configured headers: key = %q, want empty", got)
	}
}

func TestPlaceLoadAwarePicksLeastLoaded(t *testing.T) {
	d, b, eps := balancedDispatcher(nil, 3)
	now := d.nowFn()
	d.RecordSample("qwen", eps[0], picker.Sample{Running: 9, At: now})
	d.RecordSample("qwen", eps[1], picker.Sample{Running: 1, At: now})
	d.RecordSample("qwen", eps[2], picker.Sample{Running: 5, At: now})
	ep, res := d.place(b, http.Header{})
	if ep != eps[1] || res != picker.LeastLoaded {
		t.Errorf("place = %s/%s, want %s least_loaded", ep, res, eps[1])
	}
}

func TestPlaceSessionStickyAcrossReplicas(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-Affinity", "agent-1")
	d1, b1, _ := balancedDispatcher([]string{"x-session-affinity"}, 8)
	d2, b2, _ := balancedDispatcher([]string{"x-session-affinity"}, 8)
	ep1, res := d1.place(b1, h)
	ep2, _ := d2.place(b2, h)
	if res != picker.Sticky || ep1 != ep2 {
		t.Errorf("replicas placed the session on %s and %s (%s), want the same pod, sticky", ep1, ep2, res)
	}
	for i := 0; i < 5; i++ {
		if ep, _ := d1.place(b1, h); ep != ep1 {
			t.Fatalf("session moved from %s to %s while idle", ep1, ep)
		}
	}
}

// TestPlaceSessionAgreesAcrossReplicasUnderLoad: two proxy replicas that see
// the same pods in a different order and the same engine samples place every
// session on the same pod, whether it stays home or spills.
func TestPlaceSessionAgreesAcrossReplicasUnderLoad(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clock := WithNowFunc(func() time.Time { return t0 })
	headers := []string{"x-session-affinity"}
	d1, b1, eps := balancedDispatcher(headers, 8, clock)
	d2, b2, _ := balancedDispatcher(headers, 8, clock)
	shuffled := make([]string, 0, len(eps))
	for _, i := range []int{5, 2, 7, 0, 6, 3, 1, 4} {
		shuffled = append(shuffled, eps[i])
	}
	d2.SetEndpoints("qwen", shuffled)
	// Unequal, non-idle loads; the first pod runs hot enough to spill its
	// sessions while the others keep theirs.
	for i, ep := range eps {
		smp := picker.Sample{Running: []float64{8, 1, 2, 2, 1, 2, 3, 1}[i], Waiting: float64(i % 2), At: t0}
		d1.RecordSample("qwen", ep, smp)
		d2.RecordSample("qwen", ep, smp)
	}

	results := map[picker.Result]int{}
	for k := 0; k < 200; k++ {
		h := http.Header{}
		h.Set("X-Session-Affinity", fmt.Sprintf("agent-%d", k))
		ep1, res1 := d1.place(b1, h)
		ep2, res2 := d2.place(b2, h)
		if ep1 != ep2 || res1 != res2 {
			t.Errorf("session %d: replicas placed it on %s (%s) and %s (%s), want the same pod", k, ep1, res1, ep2, res2)
		}
		results[res1]++
	}
	if results[picker.Sticky] == 0 || results[picker.Spill] == 0 {
		t.Errorf("results = %v, want both sticky and spilled sessions so both paths are compared", results)
	}
}

func TestPlaceSessionSpillsOffQuarantinedHomeAndReturns(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-Affinity", "agent-2")
	d, b, _ := balancedDispatcher([]string{"x-session-affinity"}, 4)
	home, _ := d.place(b, h)
	d.markUnhealthy("qwen", home)
	ep, res := d.place(b, h)
	if ep == home || ep == "" || res != picker.Spill {
		t.Fatalf("quarantined home: place = %s/%s, want another pod as a spill", ep, res)
	}
	d.markHealthy("qwen", home)
	if ep, res := d.place(b, h); ep != home || res != picker.Sticky {
		t.Errorf("after recovery: place = %s/%s, want home %s sticky", ep, res, home)
	}
}

func TestPlaceRoundRobinUnchanged(t *testing.T) {
	cfg := &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
	}}}
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{"http://10.0.2.1:8000", "http://10.0.2.2:8000"})
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		ep, res := d.place(&d.cfg.Backends[0], http.Header{"X-Session-Affinity": {"s"}})
		if res != "" {
			t.Errorf("an unbalanced backend reported result %q", res)
		}
		seen[ep] = true
	}
	if len(seen) != 2 {
		t.Errorf("round-robin picks = %v, want both endpoints", seen)
	}
}
