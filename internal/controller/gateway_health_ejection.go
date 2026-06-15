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

package controller

import "sort"

// ejectUnhealthyBackends drops the backendRefs of unhealthy backends from each
// compiled rule so Envoy fails over to a healthy backend (slice 4b, #662). It is
// a pure transform: it never mutates its inputs (returns new slices) and is
// table-test friendly.
//
// Invariants:
//   - Never-empty: if filtering a rule would leave it with ZERO backendRefs (all
//     its backends are unhealthy), the rule's ORIGINAL backendRefs are kept
//     unchanged. An AIGatewayRoute rule with no backendRefs is invalid, and there
//     is nothing healthy to fail over to, so the route stays valid and 4a's active
//     probe carries it.
//   - Order preserving: rules and the backendRefs within each rule keep their
//     order (failover priority depends on it).
//   - Defensive on unknown names: a backendRef whose name is not present in
//     backends is treated as healthy (never ejected), so a name mismatch never
//     blackholes a rule.
//
// It returns the filtered rules and the sorted, de-duplicated list of backend
// names actually removed from at least one rule (a backend kept solely by the
// never-empty rule does NOT count as ejected). The ejected list feeds the ready
// status so an operator sees the route is degraded-but-serving.
func ejectUnhealthyBackends(
	rules []routerRuleResource,
	backends []routerBackendResource,
) (filtered []routerRuleResource, ejected []string) {
	unhealthy := make(map[string]bool, len(backends))
	for _, b := range backends {
		if !b.Healthy {
			unhealthy[b.Name] = true
		}
	}

	ejectedSet := make(map[string]struct{})
	filtered = make([]routerRuleResource, len(rules))
	for i, rule := range rules {
		kept := make([]routerBackendRef, 0, len(rule.BackendRefs))
		var removed []string
		for _, ref := range rule.BackendRefs {
			if unhealthy[ref.Name] {
				removed = append(removed, ref.Name)
				continue
			}
			kept = append(kept, ref)
		}

		out := rule
		if len(kept) == 0 {
			// Never-empty: keep the original backendRefs and eject nothing from
			// this rule. Copy the original slice so the result still owns its data.
			out.BackendRefs = append([]routerBackendRef(nil), rule.BackendRefs...)
		} else {
			out.BackendRefs = kept
			for _, name := range removed {
				ejectedSet[name] = struct{}{}
			}
		}
		filtered[i] = out
	}

	if len(ejectedSet) > 0 {
		ejected = make([]string, 0, len(ejectedSet))
		for name := range ejectedSet {
			ejected = append(ejected, name)
		}
		sort.Strings(ejected)
	}
	return filtered, ejected
}
