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

package main

import (
	"reflect"
	"testing"
)

// fixture mirrors scripts/affected-packages_test.sh's throwaway module:
// a imports b, c imports a, d is independent, p's external test package
// imports q (p itself does not), test/e2e exists.
func fixture() []node {
	m := func(p string) string { return "example.com/m/" + p }
	// go list -test node names: the internal test variant "pkg [pkg.test]",
	// the external test package "pkg_test [pkg.test]" and the generated
	// test main "pkg.test [pkg.test]".
	variant := func(pkg string) string { return m(pkg) + " [" + m(pkg+".test") + "]" }
	main := m("p.test") + " [" + m("p.test") + "]"
	return []node{
		{ImportPath: m("a"), Dir: "/r/a", Imports: []string{m("b")}},
		{ImportPath: m("b"), Dir: "/r/b"},
		{ImportPath: m("c"), Dir: "/r/c", Imports: []string{m("a")}},
		{ImportPath: m("d"), Dir: "/r/d"},
		{ImportPath: m("p"), Dir: "/r/p"},
		{ImportPath: variant("p"), Dir: "/r/p", Imports: []string{m("p")}},
		{ImportPath: variant("p_test"), Dir: "/r/p", Imports: []string{m("q")}},
		{ImportPath: main, Dir: "/r/p", Imports: []string{variant("p"), variant("p_test")}},
		{ImportPath: m("q"), Dir: "/r/q"},
	}
}

func TestSelectPackages(t *testing.T) {
	cases := []struct {
		name    string
		nodes   []node
		files   []string
		want    []string
		wantAll bool
	}{
		{
			name:  "leaf change selects the package and its reverse dependencies",
			files: []string{"b/b.go"},
			want:  []string{"example.com/m/a", "example.com/m/b", "example.com/m/c"},
		},
		{
			name:  "test-only dependency selects the package owning the test",
			files: []string{"q/q.go"},
			want:  []string{"example.com/m/p", "example.com/m/q"},
		},
		{
			name:  "untracked file in an existing package selects only that package",
			files: []string{"d/extra.go"},
			want:  []string{"example.com/m/d"},
		},
		{
			name:  "fixture under testdata maps to the enclosing package",
			files: []string{"d/testdata/fixture.yaml"},
			want:  []string{"example.com/m/d"},
		},
		{
			// Real go list never emits a node for test/e2e: every file
			// there is //go:build e2e gated. The change must select
			// nothing, not trip the deleted-package fallback.
			name:  "tag-gated e2e change selects nothing",
			files: []string{"test/e2e/e2e_test.go"},
			want:  []string{},
		},
		{
			name:  "change outside Go packages selects nothing",
			files: []string{"README.md", "charts/llmkube/values.yaml"},
			want:  []string{},
		},
		{
			name:  "go.mod change selects everything but test/e2e",
			files: []string{"README.md", "go.mod"},
			want: []string{
				"example.com/m/a", "example.com/m/b", "example.com/m/c",
				"example.com/m/d", "example.com/m/p", "example.com/m/q",
			},
			wantAll: true,
		},
		{
			name:  "go file in a deleted package selects everything",
			nodes: without(fixture(), "example.com/m/d"),
			files: []string{"d/d.go"},
			want: []string{
				"example.com/m/a", "example.com/m/b", "example.com/m/c",
				"example.com/m/p", "example.com/m/q",
			},
			wantAll: true,
		},
		{
			name:  "a package error selects everything",
			nodes: append(fixture(), node{ImportPath: "example.com/m/broken", Dir: "/r/broken", Failed: true}),
			files: []string{"b/b.go"},
			want: []string{
				"example.com/m/a", "example.com/m/b", "example.com/m/broken",
				"example.com/m/c", "example.com/m/d", "example.com/m/p",
				"example.com/m/q",
			},
			wantAll: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes := tc.nodes
			if nodes == nil {
				nodes = fixture()
			}
			got, all, _ := selectPackages(nodes, "/r", tc.files)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("selectPackages() = %v, want %v", got, tc.want)
			}
			if all != tc.wantAll {
				t.Errorf("selectPackages() all = %v, want %v", all, tc.wantAll)
			}
		})
	}
}

func without(nodes []node, importPath string) []node {
	out := make([]node, 0, len(nodes))
	for _, n := range nodes {
		if n.ImportPath != importPath {
			out = append(out, n)
		}
	}
	return out
}

func TestMapFilesToPackages(t *testing.T) {
	dirs := []string{"a", "a/b"}
	cases := []struct {
		name       string
		files      []string
		wantMapped []string
		wantUnmap  bool
	}{
		{
			name:       "longest prefix wins",
			files:      []string{"a/b/x.go"},
			wantMapped: []string{"a/b"},
		},
		{
			name:       "testdata maps to the enclosing package",
			files:      []string{"a/b/testdata/f.yaml"},
			wantMapped: []string{"a/b"},
		},
		{
			name:       "non-Go file with no package is ignored",
			files:      []string{"README.md"},
			wantMapped: []string{},
		},
		{
			name:       "Go file with no package flags the fallback",
			files:      []string{"gone/main.go"},
			wantMapped: []string{},
			wantUnmap:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapped, unmappedGo := mapFilesToPackages(tc.files, dirs)
			if !reflect.DeepEqual(sortedKeys(mapped), tc.wantMapped) {
				t.Errorf("mapped = %v, want %v", sortedKeys(mapped), tc.wantMapped)
			}
			if unmappedGo != tc.wantUnmap {
				t.Errorf("unmappedGo = %v, want %v", unmappedGo, tc.wantUnmap)
			}
		})
	}
}

func TestReverseClosure(t *testing.T) {
	edges := map[string][]string{
		"b": {"a"},
		"a": {"c"},
		"x": {"y", "z"},
	}
	got := reverseClosure(edges, map[string]bool{"b": true})
	want := map[string]bool{"b": true, "a": true, "c": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reverseClosure() = %v, want %v", got, want)
	}
	cycle := map[string][]string{"a": {"b"}, "b": {"a"}}
	got = reverseClosure(cycle, map[string]bool{"a": true})
	want = map[string]bool{"a": true, "b": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cycle closure = %v, want %v", got, want)
	}
}

func TestTestablePackage(t *testing.T) {
	cases := map[string]string{
		"example.com/m/p":          "example.com/m/p",
		"example.com/m/p [p.test]": "example.com/m/p",
		"example.com/m/p.test":     "example.com/m/p",
		"example.com/m/p.test [x]": "example.com/m/p",
		"example.com/m/p_test [x]": "example.com/m/p",
		"example.com/m/p_test":     "example.com/m/p_test",
		"example.com/m/test/e2e":   "example.com/m/test/e2e",
	}
	for in, want := range cases {
		if got := testablePackage(in); got != want {
			t.Errorf("testablePackage(%q) = %q, want %q", in, got, want)
		}
	}
}
