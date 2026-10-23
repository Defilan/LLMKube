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

func balancedDispatcher(headers []string, n int) (*Dispatcher, *Backend, []string) {
	cfg := &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
		Balancing:      &Balancing{Mode: BalancingLoadAware, SessionHeaders: headers},
		Metrics:        &vllmProfile,
	}}}
	d := NewDispatcher(cfg, WithQuarantineDuration(time.Hour))
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
