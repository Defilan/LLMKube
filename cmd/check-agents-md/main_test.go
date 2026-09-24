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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the workspace root as seen from this package's test working dir.
const repoRoot = "../.."

// TestCheck_RealRepo is the check's acceptance test: the committed AGENTS.md
// must resolve against the committed Makefile, paths, and go.mod.
func TestCheck_RealRepo(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot, agentsDoc))
	if err != nil {
		t.Fatalf("read %s: %v", agentsDoc, err)
	}
	problems, err := check(repoRoot, string(doc))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("AGENTS.md has unresolved references: %v", problems)
	}
}

// writeWorkspace lays down a minimal repo root for the negative cases: a
// Makefile defining `build`, the given AGENTS.md, and an optional go.mod.
func writeWorkspace(t *testing.T, doc, gomod string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, agentsDoc), []byte(doc), 0o644); err != nil {
		t.Fatalf("write %s: %v", agentsDoc, err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("build:\n\ttrue\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	if gomod != "" {
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
	}
	return root
}

// TestCheck_ReportsMissingTarget pins that a `make <x>` reference with no
// Makefile target fails the check. Renaming a referenced target is the exact
// drift this check exists to catch.
func TestCheck_ReportsMissingTarget(t *testing.T) {
	root := writeWorkspace(t, "Run `make test`.", "")
	problems, err := check(root, "Run `make test`.")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "make test") {
		t.Fatalf("problems = %v, want one naming `make test`", problems)
	}
}

// TestCheck_ReportsMissingPath pins that a repo path reference that does not
// exist fails the check.
func TestCheck_ReportsMissingPath(t *testing.T) {
	root := writeWorkspace(t, "See `internal/gone/`.", "")
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatalf("mkdir internal: %v", err)
	}
	problems, err := check(root, "See `internal/gone/`.")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "internal/gone") {
		t.Fatalf("problems = %v, want one naming internal/gone", problems)
	}
}

// TestCheck_ReportsStaleGoVersion pins the go.mod cross-check.
func TestCheck_ReportsStaleGoVersion(t *testing.T) {
	root := writeWorkspace(t, "Go 1.25, built with controller-runtime.", "module x\n\ngo 1.26.6\n")
	problems, err := check(root, "Go 1.25, built with controller-runtime.")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "Go 1.25") {
		t.Fatalf("problems = %v, want one about the Go version", problems)
	}
}

// TestCheck_AcceptsResolvingRefs is the positive control: a `make` target and a
// repo path that both resolve produce no problems.
func TestCheck_AcceptsResolvingRefs(t *testing.T) {
	root := writeWorkspace(t, "Run `make build` against `internal/`.", "module x\n\ngo 1.26.6\n")
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatalf("mkdir internal: %v", err)
	}
	problems, err := check(root, "Run `make build` against `internal/`.")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
}

// TestExtractRefs_IgnoresNonPaths pins the false-positive guards: label keys,
// branch slugs, and build tags are not treated as repo paths.
func TestExtractRefs_IgnoresNonPaths(t *testing.T) {
	top := map[string]bool{"internal": true}
	doc := "key `inference.llmkube.dev/model-router`, branch `feat/<slug>`, tag `//go:build`"
	for _, r := range extractRefs(doc, top) {
		t.Errorf("extractRefs(%q) produced %+v, want none", doc, r)
	}
}
