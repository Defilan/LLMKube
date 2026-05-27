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

package repomap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkRepo seeds a workspace dir with a small Go project. The structure
// mirrors what foreman-agent will see after `git clone`: top-level
// cmd/ package, a couple of internal packages, a vendor/ dir that
// must be skipped, and a generated file that must be skipped.
func mkRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	write("cmd/foo/main.go", `// Package main runs the foo binary.
package main

import "fmt"

func main() {
	fmt.Println("foo")
}
`)
	write("internal/processor/processor.go", `// Package processor contains the core processing pipeline.
package processor

import "context"

// Processor is the main worker.
type Processor struct {
	Name string
}

// Process runs one unit of work.
func (p *Processor) Process(ctx context.Context, input string) (string, error) {
	return input + ":done", nil
}

// New constructs a Processor.
func New(name string) *Processor {
	return &Processor{Name: name}
}
`)
	write("internal/scheduler/scheduler.go", `package scheduler

// Scheduler orders tasks.
type Scheduler struct{}

func (s *Scheduler) Schedule(task string) {}
`)
	// Vendor: MUST be skipped.
	write("vendor/github.com/example/lib.go", `package lib

func ShouldNotBeSeen() {}
`)
	// Generated: MUST be skipped.
	write("internal/processor/zz_generated.deepcopy.go", `package processor

func (in *Processor) DeepCopy() *Processor { return nil }
`)
	// Test file: MUST be skipped (v0.3 default).
	write("internal/processor/processor_test.go", `package processor

func TestNoise(t *testing.T) {}
`)
	// Hidden dir except .github: MUST be skipped.
	write(".cache/junk.go", `package junk
func Junk() {}
`)
	// .github IS preserved (workflows are useful context); empty .go
	// file just to exercise the path. We don't really expect .go in
	// .github but the walker should let it through if present.
	write(".github/scripts/release.go", `// Package main is a release helper.
package main
func main() {}
`)

	return root
}

func TestBuild_RanksIssueTargetedFileFirst(t *testing.T) {
	root := mkRepo(t)
	out, err := Build(context.Background(), root,
		"fix the bug in internal/processor/processor.go where Process returns the wrong suffix",
		Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out == "" {
		t.Fatal("Build returned empty summary")
	}

	// Find the order in which file headers appear.
	headers := []string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "### ") {
			headers = append(headers, strings.TrimPrefix(line, "### "))
		}
	}
	if len(headers) == 0 {
		t.Fatal("no file headers in summary")
	}
	if headers[0] != "internal/processor/processor.go" {
		t.Errorf("expected processor.go first (path mention in issue), got order: %v", headers)
	}
}

func TestBuild_SkipsVendorGeneratedAndTests(t *testing.T) {
	root := mkRepo(t)
	out, err := Build(context.Background(), root, "anything", Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cases := []string{
		"vendor/github.com/example/lib.go",
		"internal/processor/zz_generated.deepcopy.go",
		"internal/processor/processor_test.go",
		".cache/junk.go",
	}
	for _, c := range cases {
		if strings.Contains(out, c) {
			t.Errorf("summary should not contain %q, got:\n%s", c, out)
		}
	}
}

func TestBuild_RespectsTokenBudget(t *testing.T) {
	root := mkRepo(t)
	// Tiny budget should produce a tiny output. 50 tokens ≈ 200 chars,
	// which fits the header + 1-2 files.
	out, err := Build(context.Background(), root, "processor", Options{TokenBudget: 50})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 50 tokens × 4 chars/token = 200 char ceiling target, but the
	// formatter only checks AFTER writing each file entry, so the
	// header + first file may exceed slightly. Assert a loose upper
	// bound that catches "budget completely ignored" without being
	// brittle to small formatting changes.
	if len(out) > 1000 {
		t.Errorf("tiny budget produced %d bytes; expected far less", len(out))
	}
	// Should still contain the budget-exhausted footer when not
	// every file fit.
	if !strings.Contains(out, "budget exhausted") {
		t.Errorf("expected 'budget exhausted' footer when many files filtered, got:\n%s", out)
	}
}

func TestBuild_EmptyWorkspaceReturnsEmptyString(t *testing.T) {
	out, err := Build(context.Background(), "", "anything", Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out != "" {
		t.Errorf("empty workspace should return empty string, got: %q", out)
	}
}

func TestBuild_NonGoOnlyWorkspaceReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("# nothing to index"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := Build(context.Background(), root, "anything", Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out != "" {
		t.Errorf("non-Go-only workspace should return empty (v0.3 Go-only), got:\n%s", out)
	}
}

func TestExtract_RendersFuncMethodTypeConst(t *testing.T) {
	dir := t.TempDir()
	src := `// Package demo demonstrates the extractor.
package demo

import "context"

// MaxItems caps how many widgets we keep.
const MaxItems = 100

// Widget is a thing.
type Widget struct {
	Name string
}

// New makes a Widget.
func New(name string) *Widget {
	return &Widget{Name: name}
}

// Apply does work.
func (w *Widget) Apply(ctx context.Context, input string) (string, error) {
	return input, nil
}

// unexported should be skipped.
func unexported() {}

// privateType should be skipped.
type privateType struct{}
`
	path := filepath.Join(dir, "demo.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ex, err := extractGo(path)
	if err != nil {
		t.Fatalf("extractGo: %v", err)
	}
	if ex.PackageName != "demo" {
		t.Errorf("PackageName: want demo got %q", ex.PackageName)
	}
	if !strings.Contains(ex.PackageDoc, "demonstrates the extractor") {
		t.Errorf("PackageDoc missing first paragraph: %q", ex.PackageDoc)
	}
	want := []string{
		"const MaxItems",
		"type Widget struct",
		"func New(name string) *Widget",
		"func (w *Widget) Apply(ctx context.Context, input string) (string, error)",
	}
	got := strings.Join(ex.Decls, " | ")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("Decls missing %q. Got: %q", w, got)
		}
	}
	// Unexported entries must be absent.
	for _, w := range []string{"unexported", "privateType"} {
		if strings.Contains(got, w) {
			t.Errorf("Decls should not include unexported %q, got: %q", w, got)
		}
	}
}

func TestExtractPathMentions(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"fix the bug in internal/foo/bar.go", []string{"internal/foo/bar.go"}},
		{"see tools/bash.go and tools/grep.go", []string{"tools/bash.go", "tools/grep.go"}},
		{"file.go has the issue", []string{"file.go"}},
		// URL fragments are conservatively matched: only the host/path
		// portion gets extracted (the "https:" piece is stripped by
		// the regex's "stop at non-identifier characters" rule). Not
		// ideal as path mentions, but harmless: substring-matching
		// "example.com/path" against a workspace path won't match
		// anything real. Pinning current behavior so the regex
		// doesn't silently regress.
		{"docs at https://example.com/path are fine", []string{"//example.com/path"}},
		// Plain English: no path mentions.
		{"the bug is somewhere", nil},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got := extractPathMentions(tc.text)
			if len(got) != len(tc.want) {
				t.Errorf("count: want %d got %d (%v)", len(tc.want), len(got), got)
				return
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("[%d]: want %q got %q", i, w, got[i])
				}
			}
		})
	}
}

func TestTokenize_StripsStopwordsAndSplits(t *testing.T) {
	tokens := tokenize("Fix the bug in the str_replace tool")
	for _, sw := range []string{"the", "fix", "bug"} {
		if tokens[sw] {
			t.Errorf("stopword %q should not be in token set", sw)
		}
	}
	for _, want := range []string{"str", "replace", "tool"} {
		if !tokens[want] {
			t.Errorf("token %q should be in token set: %v", want, tokens)
		}
	}
}
