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
		t.Fatalf("production code assigns .Spec.ModelCache, which a status write undoes mid-reconcile; "+
			"pass the value from resolveEffectiveModelCache instead (#2142):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// isModelCacheSelector reports whether expr ends in .Spec.ModelCache or
// .Spec.ModelCache[<key>], whatever the receiver is called.
func isModelCacheSelector(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "ModelCache" {
		return false
	}
	base, ok := sel.X.(*ast.SelectorExpr)
	return ok && base.Sel.Name == "Spec"
}
