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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAssertionCommentsDoNotRestateImplementation enforces the writing-tests
// rule in CONTRIBUTING.md: a By(...) description that documents what the code
// does ("returns X because", "verifies Y was set to Z", "controller does W")
// is the #374 bug class, because the assertion then cannot fail when behavior
// regresses. See #378.
//
// This is a test rather than a forbidigo rule because forbidigo matches
// identifier names, not the string contents inside a call.
func TestAssertionCommentsDoNotRestateImplementation(t *testing.T) {
	pattern := regexp.MustCompile(`By\("[^"]*\((returns|verifies|controller does|sets the|stores)`)

	root := filepath.Join("..", "..")
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if pattern.MatchString(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository for assertion-style offenders: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("assertion comments restate the implementation instead of the spec; assert on side effects (see CONTRIBUTING.md, #374):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
