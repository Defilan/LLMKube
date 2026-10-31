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

package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoProductionModelCacheAssignment enforces the resolution contract of
// #2142: production code never writes an InferenceService's spec.modelCache.
// The default comes from the Model, and writing it into the spec in memory is
// what a status write silently undoes (client.Status().Update decodes the
// stored object back into the same pointer). The resolved value is the only
// way a reader may reach the block.
//
// The rule is per assignment, so it holds whatever the variable is named.
func TestNoProductionModelCacheAssignment(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing package files: %v", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatalf("reading %s: %v", file, readErr)
		}
		f, parseErr := parser.ParseFile(fset, file, src, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", file, parseErr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				if isModelCacheSelector(lhs) {
					pos := fset.Position(assign.Pos())
					offenders = append(offenders, pos.String())
				}
			}
			return true
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("production code assigns .Spec.ModelCache or a field of it, which a status write undoes mid-reconcile; "+
			"pass the value from resolveEffectiveModelCache instead (#2142):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// isModelCacheSelector reports whether expr is rooted at .Spec.ModelCache,
// whatever the receiver is called and however the block is reached: the whole
// block (x.Spec.ModelCache), a field of it (x.Spec.ModelCache.ClaimName), or
// the block dereferenced (*x.Spec.ModelCache).
func isModelCacheSelector(expr ast.Expr) bool {
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.X
		case *ast.StarExpr:
			expr = e.X
		case *ast.SelectorExpr:
			if e.Sel.Name == "ModelCache" {
				base, ok := e.X.(*ast.SelectorExpr)
				if ok && base.Sel.Name == "Spec" {
					return true
				}
				return false
			}
			// A field selected through the block: walk down to the root.
			expr = e.X
		default:
			return false
		}
	}
}

// TestIsModelCacheSelector pins the forms the guard matches, so widening it
// does not silently narrow it: the whole block, a field written through it,
// and the block dereferenced all assign the structure a status write undoes.
func TestIsModelCacheSelector(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{"isvc.Spec.ModelCache", true},
		{"isvc.Spec.ModelCache.ClaimName", true},
		{"isvc.Spec.ModelCache.Persistence", true},
		{"*isvc.Spec.ModelCache", true},
		{"(isvc.Spec.ModelCache).ClaimName", true},
		{"m.Spec.ModelCache", true},
		{"isvc.Spec.Env", false},
		{"isvc.Spec.ModelCache", true},
		{"isvc.Status.Phase", false},
		{"model.Spec.ModelCache", true},
		{"cache.spec", false},
		{"cache.claimName()", false},
		{"isvc.Spec", false},
		{"spec.ModelCache", false},
		{"counts", false},
	}
	fset := token.NewFileSet()
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			expr, err := parser.ParseExprFrom(fset, "probe.go", tc.expr, 0)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.expr, err)
			}
			if got := isModelCacheSelector(expr); got != tc.want {
				t.Errorf("isModelCacheSelector(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}
