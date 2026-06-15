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

import (
	"reflect"
	"testing"
)

// ptrInt64 is a tiny helper for building backendRef priorities in table cases.
func ptrInt64(v int64) *int64 { return &v }

// backendRefNames pulls the ordered names out of a rule's backendRefs so cases
// can assert filtering and ordering without comparing the priority pointers.
func backendRefNames(refs []routerBackendRef) []string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	return names
}

// TestEjectUnhealthyBackends exercises the pure route-level ejection transform:
// it drops unhealthy backends from each rule's backendRefs (so Envoy fails over
// to a healthy fallback), preserves order, never empties a rule, and reports the
// backends actually ejected for status.
func TestEjectUnhealthyBackends(t *testing.T) {
	tests := []struct {
		name        string
		rules       []routerRuleResource
		backends    []routerBackendResource
		wantRefs    [][]string
		wantEjected []string
	}{
		{
			name: "all healthy leaves rules unchanged",
			rules: []routerRuleResource{
				{BackendRefs: []routerBackendRef{
					{Name: "cuda", Priority: ptrInt64(0)},
					{Name: "metal", Priority: ptrInt64(1)},
				}},
			},
			backends: []routerBackendResource{
				{Name: "cuda", Healthy: true},
				{Name: "metal", Healthy: true},
			},
			wantRefs:    [][]string{{"cuda", "metal"}},
			wantEjected: nil,
		},
		{
			name: "primary unhealthy drops primary keeps fallback",
			rules: []routerRuleResource{
				{BackendRefs: []routerBackendRef{
					{Name: "cuda", Priority: ptrInt64(0)},
					{Name: "metal", Priority: ptrInt64(1)},
				}},
			},
			backends: []routerBackendResource{
				{Name: "cuda", Healthy: false},
				{Name: "metal", Healthy: true},
			},
			wantRefs:    [][]string{{"metal"}},
			wantEjected: []string{"cuda"},
		},
		{
			name: "all backends of a rule unhealthy keeps rule intact and ejects nothing",
			rules: []routerRuleResource{
				{BackendRefs: []routerBackendRef{
					{Name: "cuda", Priority: ptrInt64(0)},
					{Name: "metal", Priority: ptrInt64(1)},
				}},
			},
			backends: []routerBackendResource{
				{Name: "cuda", Healthy: false},
				{Name: "metal", Healthy: false},
			},
			wantRefs:    [][]string{{"cuda", "metal"}},
			wantEjected: nil,
		},
		{
			// The defaultRoute catch-all compiles to a single-backendRef rule; an
			// unhealthy sole backend must be kept (never empty), not ejected.
			name: "single-backendRef rule (defaultRoute shape) with unhealthy backend keeps it and ejects nothing",
			rules: []routerRuleResource{
				{BackendRefs: []routerBackendRef{
					{Name: "metal"},
				}},
			},
			backends: []routerBackendResource{
				{Name: "metal", Healthy: false},
			},
			wantRefs:    [][]string{{"metal"}},
			wantEjected: nil,
		},
		{
			name: "multi-rule ejects an unhealthy backend from every rule except where it would empty",
			rules: []routerRuleResource{
				// rule 0: cuda(down) + metal(up) -> metal only
				{BackendRefs: []routerBackendRef{
					{Name: "cuda", Priority: ptrInt64(0)},
					{Name: "metal", Priority: ptrInt64(1)},
				}},
				// rule 1: cuda(down) only -> would empty, keep intact
				{BackendRefs: []routerBackendRef{
					{Name: "cuda", Priority: ptrInt64(0)},
				}},
			},
			backends: []routerBackendResource{
				{Name: "cuda", Healthy: false},
				{Name: "metal", Healthy: true},
			},
			// cuda is ejected because it was actually removed from rule 0, even
			// though it was kept in rule 1 to avoid emptying it.
			wantRefs:    [][]string{{"metal"}, {"cuda"}},
			wantEjected: []string{"cuda"},
		},
		{
			name: "ordering preserved when a middle backend is ejected",
			rules: []routerRuleResource{
				{BackendRefs: []routerBackendRef{
					{Name: "a", Priority: ptrInt64(0)},
					{Name: "b", Priority: ptrInt64(1)},
					{Name: "c", Priority: ptrInt64(2)},
				}},
			},
			backends: []routerBackendResource{
				{Name: "a", Healthy: true},
				{Name: "b", Healthy: false},
				{Name: "c", Healthy: true},
			},
			wantRefs:    [][]string{{"a", "c"}},
			wantEjected: []string{"b"},
		},
		{
			name: "unknown backend name is treated as healthy and never blackholes a rule",
			rules: []routerRuleResource{
				{BackendRefs: []routerBackendRef{
					{Name: "ghost", Priority: ptrInt64(0)},
				}},
			},
			backends: []routerBackendResource{
				{Name: "cuda", Healthy: false},
			},
			wantRefs:    [][]string{{"ghost"}},
			wantEjected: nil,
		},
		{
			name: "ejected names are sorted and de-duplicated across rules",
			rules: []routerRuleResource{
				{BackendRefs: []routerBackendRef{
					{Name: "metal", Priority: ptrInt64(0)},
					{Name: "cuda", Priority: ptrInt64(1)},
					{Name: "tpu", Priority: ptrInt64(2)},
				}},
				{BackendRefs: []routerBackendRef{
					{Name: "tpu", Priority: ptrInt64(0)},
					{Name: "cuda", Priority: ptrInt64(1)},
				}},
			},
			backends: []routerBackendResource{
				{Name: "metal", Healthy: true},
				{Name: "cuda", Healthy: false},
				{Name: "tpu", Healthy: false},
			},
			// rule 0 (metal up, cuda+tpu down) ejects cuda and tpu, keeping metal.
			// rule 1 (tpu+cuda both down) would empty, so it is kept intact and
			// ejects nothing. cuda+tpu still count as ejected (from rule 0), sorted
			// and de-duplicated.
			wantRefs:    [][]string{{"metal"}, {"tpu", "cuda"}},
			wantEjected: []string{"cuda", "tpu"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered, ejected := ejectUnhealthyBackends(tt.rules, tt.backends)

			if len(filtered) != len(tt.wantRefs) {
				t.Fatalf("got %d rules, want %d", len(filtered), len(tt.wantRefs))
			}
			for i := range filtered {
				if got := backendRefNames(filtered[i].BackendRefs); !reflect.DeepEqual(got, tt.wantRefs[i]) {
					t.Errorf("rule %d backendRefs = %v, want %v", i, got, tt.wantRefs[i])
				}
			}
			if !reflect.DeepEqual(ejected, tt.wantEjected) {
				t.Errorf("ejected = %v, want %v", ejected, tt.wantEjected)
			}
		})
	}
}

// TestEjectUnhealthyBackends_DoesNotMutateInputs verifies the transform returns
// new slices and never edits the caller's rules or backendRefs in place (the
// reconciler still generates Backend/AIServiceBackend objects for every backend
// from the original, unfiltered data).
func TestEjectUnhealthyBackends_DoesNotMutateInputs(t *testing.T) {
	rules := []routerRuleResource{
		{BackendRefs: []routerBackendRef{
			{Name: "cuda", Priority: ptrInt64(0)},
			{Name: "metal", Priority: ptrInt64(1)},
		}},
	}
	backends := []routerBackendResource{
		{Name: "cuda", Healthy: false},
		{Name: "metal", Healthy: true},
	}

	wantOriginal := []string{"cuda", "metal"}

	filtered, _ := ejectUnhealthyBackends(rules, backends)

	if got := backendRefNames(rules[0].BackendRefs); !reflect.DeepEqual(got, wantOriginal) {
		t.Errorf("input rule was mutated: backendRefs = %v, want %v", got, wantOriginal)
	}
	// The returned slice must be a distinct filtered result.
	if got := backendRefNames(filtered[0].BackendRefs); !reflect.DeepEqual(got, []string{"metal"}) {
		t.Errorf("filtered rule = %v, want [metal]", got)
	}
}
