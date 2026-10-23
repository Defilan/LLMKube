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
	"net/http"
	"strings"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/internal/router/picker"
)

// sessionKey returns the first configured header with a non-empty value. The
// value is used only to hash onto the ring; it is never stored, logged or
// used as a metric label.
func sessionKey(h http.Header, names []string) string {
	if h == nil {
		return ""
	}
	for _, n := range names {
		if v := strings.TrimSpace(h.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

// ringCache is a backend's ring and the endpoint set it was built from.
type ringCache struct {
	set  string
	ring *picker.Ring
}

// ringFor returns the backend's ring for the current endpoint set, rebuilding
// it when the set changed.
func (d *Dispatcher) ringFor(backend string, eps []string) *picker.Ring {
	set := strings.Join(eps, ",")
	if v, ok := d.rings.Load(backend); ok {
		if rc := v.(*ringCache); rc.set == set {
			return rc.ring
		}
	}
	rc := &ringCache{set: set, ring: picker.NewRing(eps)}
	d.rings.Store(backend, rc)
	return rc.ring
}

// place chooses the endpoint for a request. Backends without balancing
// round-robin exactly as before and report an empty result.
func (d *Dispatcher) place(backend *Backend, headers http.Header) (string, picker.Result) {
	eps := d.liveEndpoints(backend)
	bal := backend.Balancing
	if len(eps) == 0 || bal == nil || (bal.Mode != BalancingLoadAware && len(bal.SessionHeaders) == 0) {
		return d.roundRobin(backend, eps), ""
	}
	cands := make([]picker.Candidate, 0, len(eps))
	for _, ep := range eps {
		if !d.dispatchable(healthKey(backend.Name, ep)) {
			continue
		}
		l := d.loadFor(backend.Name, ep)
		c := picker.Candidate{Endpoint: ep, InFlight: l.inflight.Load()}
		if bal.Mode == BalancingLoadAware {
			c.Sample = l.sample.Load()
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return d.roundRobin(backend, eps), picker.Fallback
	}
	now := d.nowFn()
	cands = picker.Eligible(cands, now)
	if key := sessionKey(headers, bal.SessionHeaders); key != "" {
		return d.ringFor(backend.Name, eps).Pick(key, cands, now, bal.LoadPercent())
	}
	if bal.Mode == BalancingLoadAware {
		// The tie start shares the round-robin cursor so equal loads rotate;
		// PickLeastLoaded accepts any int, so a wrapped conversion is fine.
		return picker.PickLeastLoaded(cands, now, int(d.endpointCursor(backend.Name).Add(1)-1)) //nolint:gosec // wraps harmlessly, PickLeastLoaded takes any int
	}
	return d.roundRobin(backend, eps), ""
}

// countPlacement records a balanced placement decision.
func (d *Dispatcher) countPlacement(backend string, res picker.Result) {
	if res == "" {
		return
	}
	prommetrics.RouterPlacementsTotal.WithLabelValues(d.routerName, backend, string(res)).Inc()
}
