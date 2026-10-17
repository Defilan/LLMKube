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

// Command affected-packages prints the Go packages affected by changes since
// a base ref: the packages the changed files belong to (committed, staged,
// unstaged and untracked) plus every package whose dependency graph reaches
// them, including test-only dependencies, via go list -deps -test.
// scripts/test-affected.sh feeds the output to go test. Renames report both
// sides (--no-renames), so the package that loses a file to a git mv stays
// selected.
//
// A change to go.mod, go.sum, go.work or go.work.sum selects every package,
// as does a .go file that belongs to no listed package (its directory was
// deleted, or it is a stray file such as an untracked scratch main.go where
// no package lives) or any package go list reports an error for (the tree
// does not compile). The run-everything fallback is announced on stderr.
// .go files under test/e2e are //go:build e2e gated, so go list never
// reports them; changing one selects nothing. Changes outside Go
// packages select nothing. test/e2e is never printed, matching make test.
//
// Usage:
//
//	go run ./cmd/affected-packages -base upstream/main
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
)

// node is the subset of go list -json output the selection needs.
type node struct {
	ImportPath string
	Dir        string
	Imports    []string
	Failed     bool
}

// buildAffectingFiles are non-Go files whose change invalidates every
// package's build, not just the package directory they sit in.
var buildAffectingFiles = map[string]bool{
	"go.mod":      true,
	"go.sum":      true,
	"go.work":     true,
	"go.work.sum": true,
}

func main() {
	base := flag.String("base", "upstream/main", "base ref to diff against")
	flag.Parse()
	if code := run(*base, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(base string, stdout, stderr io.Writer) int {
	root, err := git("", "rev-parse", "--show-toplevel")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "affected-packages: %v\n", err)
		return 1
	}
	if _, err := git(root, "rev-parse", "--verify", base); err != nil {
		_, _ = fmt.Fprintf(stderr, "affected-packages: base ref %q does not resolve: %v\n",
			base, err)
		return 1
	}
	mb, err := git(root, "merge-base", base, "HEAD")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "affected-packages: merge-base with %s failed: %v\n",
			base, err)
		return 1
	}
	files, err := changedFiles(root, mb)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "affected-packages: %v\n", err)
		return 1
	}
	nodes, err := loadNodes(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "affected-packages: %v\n", err)
		return 1
	}
	pkgs, all, reason := selectPackages(nodes, root, files)
	if all {
		_, _ = fmt.Fprintf(stderr, "affected-packages: %s; selecting every package\n", reason)
	}
	if len(pkgs) == 0 {
		_, _ = fmt.Fprintf(stderr, "affected-packages: no Go packages affected by %s\n", base)
		return 0
	}
	for _, p := range pkgs {
		_, _ = fmt.Fprintln(stdout, p)
	}
	return 0
}

// selectPackages reduces changed files to the packages go test should run:
// the packages containing or owning those files plus every package whose
// dependency graph reaches them. all is true with a reason when the change
// invalidates the whole module and every package must run instead.
func selectPackages(nodes []node, root string, files []string) (pkgs []string, all bool, reason string) {
	module := modulePackages(nodes, root)
	dirs := packageDirs(nodes, root)
	dirList := make([]string, 0, len(dirs))
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Strings(dirList)
	mapped, unmappedGo := mapFilesToPackages(files, dirList)
	switch {
	case firstBuildAffecting(files) != "":
		return sortedKeys(module), true, firstBuildAffecting(files) + " changed"
	case unmappedGo:
		return sortedKeys(module), true, "changed .go files belong to no listed package"
	case listFailed(nodes):
		return sortedKeys(module), true, "go list reported package errors"
	}
	seeds := map[string]bool{}
	for dir := range mapped {
		for _, n := range dirs[dir] {
			seeds[n.ImportPath] = true
		}
	}
	affected := reverseClosure(importEdges(nodes), seeds)
	selected := map[string]bool{}
	for p := range affected {
		pkg := testablePackage(p)
		// modulePackages already excludes test/e2e, so the membership check
		// filters it here too.
		if !module[pkg] || selected[pkg] {
			continue
		}
		selected[pkg] = true
	}
	return sortedKeys(selected), false, ""
}

// firstBuildAffecting returns the first changed file that invalidates the
// whole build, or "".
func firstBuildAffecting(files []string) string {
	for _, f := range files {
		if buildAffectingFiles[f] {
			return f
		}
	}
	return ""
}

// listFailed reports whether go list flagged any package, which means the
// dependency graph is incomplete and nothing can be narrowed safely.
func listFailed(nodes []node) bool {
	for i := range nodes {
		if nodes[i].Failed {
			return true
		}
	}
	return false
}

// modulePackages is the set of packages rooted inside the module checkout,
// reduced through testablePackage so test variants map to their package.
// test/e2e is excluded here, so every path out of selectPackages matches
// make test's package list.
func modulePackages(nodes []node, root string) map[string]bool {
	out := map[string]bool{}
	for i := range nodes {
		if !inModule(nodes[i].Dir, root) {
			continue
		}
		pkg := testablePackage(nodes[i].ImportPath)
		if strings.HasSuffix(pkg, "/test/e2e") {
			continue
		}
		out[pkg] = true
	}
	return out
}

func inModule(dir, root string) bool {
	return dir != "" && (dir == root || strings.HasPrefix(dir, root+"/"))
}

// packageDirs groups nodes by directory relative to the module root. go list
// omits testdata directories, so fixture files map to the enclosing package
// by prefix matching in mapFilesToPackages.
func packageDirs(nodes []node, root string) map[string][]*node {
	out := map[string][]*node{}
	for i := range nodes {
		n := &nodes[i]
		if !inModule(n.Dir, root) {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(n.Dir, root), "/")
		if rel == "" {
			rel = "."
		}
		out[rel] = append(out[rel], n)
	}
	return out
}

// isE2ETreePath reports whether a repo-relative path is inside test/e2e,
// whose .go files are all //go:build e2e gated: go list never reports them
// in the default build, so a change there selects nothing instead of every
// package. test/e2e is the only build-tag-gated tree today; a new one must be
// added here, or every change to it falls back to selecting every package.
func isE2ETreePath(p string) bool {
	return strings.HasPrefix(p, "test/e2e/")
}

// mapFilesToPackages maps changed files to package directories by longest
// matching directory prefix. It reports whether any .go file matched no
// package at all (a deleted package directory or a stray file), in which
// case nothing can be narrowed.
func mapFilesToPackages(files []string, dirs []string) (selected map[string]bool, unmappedGo bool) {
	selected = map[string]bool{}
	for _, f := range files {
		dir := path.Dir(f)
		best := ""
		for _, d := range dirs {
			if (dir == d || strings.HasPrefix(dir, d+"/")) && len(d) > len(best) {
				best = d
			}
		}
		if best == "" {
			// test/e2e sources are //go:build e2e gated, so go list never
			// reports their directory; they are not build inputs for the
			// unit gate and must not trip the deleted-package fallback.
			if isE2ETreePath(f) {
				continue
			}
			if strings.HasSuffix(f, ".go") {
				unmappedGo = true
			}
			continue
		}
		selected[best] = true
	}
	return selected, unmappedGo
}

// importEdges maps each import path to the nodes that import it, over the
// whole -test graph so a package whose test imports a changed package is
// selected too.
func importEdges(nodes []node) map[string][]string {
	edges := map[string][]string{}
	for i := range nodes {
		for _, imp := range nodes[i].Imports {
			edges[imp] = append(edges[imp], nodes[i].ImportPath)
		}
	}
	return edges
}

// reverseClosure adds to seeds every node that imports a seeded node,
// directly or transitively.
func reverseClosure(edges map[string][]string, seeds map[string]bool) map[string]bool {
	reached := map[string]bool{}
	queue := make([]string, 0, len(seeds))
	for s := range seeds {
		reached[s] = true
		queue = append(queue, s)
	}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, dependent := range edges[cur] {
			if !reached[dependent] {
				reached[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	return reached
}

// testablePackage reduces a go list -test node name to the package path go
// test accepts: "pkg [pkg.test]", "pkg_test [pkg.test]" and the generated
// "pkg.test" main all reduce to "pkg". A plain path whose last segment ends
// in _test is a real package directory and is kept as is.
func testablePackage(p string) string {
	rest := p
	testNode := false
	if i := strings.Index(p, " ["); i >= 0 {
		rest = p[:i]
		testNode = true
	}
	if strings.HasSuffix(rest, ".test") {
		rest = strings.TrimSuffix(rest, ".test")
		testNode = true
	}
	if testNode && strings.HasSuffix(rest, "_test") {
		rest = strings.TrimSuffix(rest, "_test")
	}
	return rest
}

// changedFiles collects every changed path since the merge base: committed
// changes, staged and unstaged working-tree changes, and untracked files.
// --no-renames makes a rename report both its source and its destination, so
// the package that lost the file is selected too; rename detection (on by
// default) would list only the destination and silently skip the source
// package's suite and its reverse dependencies.
func changedFiles(root, mergeBase string) ([]string, error) {
	specs := [][]string{
		{"diff", "--no-renames", "--name-only", mergeBase, "HEAD"},
		{"diff", "--no-renames", "--name-only", "HEAD"},
		{"ls-files", "--others", "--exclude-standard"},
	}
	seen := map[string]bool{}
	files := make([]string, 0, len(specs))
	for _, spec := range specs {
		out, err := git(root, spec...)
		if err != nil {
			return nil, err
		}
		for _, f := range strings.Split(out, "\n") {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			files = append(files, f)
		}
	}
	return files, nil
}

// loadNodes runs go list -deps -test -json over the whole module. -e keeps
// the listing going past broken packages; their nodes carry Failed so the
// caller can fall back to running everything.
func loadNodes(root string) ([]node, error) {
	cmd := exec.Command("go", "list", "-e", "-deps", "-test", "-json", "./...")
	cmd.Dir = root
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list -deps -test: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	nodes := make([]node, 0, 256)
	for dec.More() {
		var raw struct {
			ImportPath string   `json:"ImportPath"`
			Dir        string   `json:"Dir"`
			Imports    []string `json:"Imports"`
			Error      *struct {
				Err string `json:"Err"`
			} `json:"Error"`
		}
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		nodes = append(nodes, node{
			ImportPath: raw.ImportPath,
			Dir:        raw.Dir,
			Imports:    raw.Imports,
			Failed:     raw.Error != nil,
		})
	}
	return nodes, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
