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

package agent

import (
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
)

func TestNormalizeFindingFile(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		diffFile string
		want     string
	}{
		{
			name:     "repo-root-relative path unchanged",
			file:     "pkg/cli/cache.go",
			diffFile: "pkg/cli/cache.go",
			want:     "pkg/cli/cache.go",
		},
		{
			name:     "dot-prefixed path stripped",
			file:     "./pkg/cli/cache.go",
			diffFile: "pkg/cli/cache.go",
			want:     "pkg/cli/cache.go",
		},
		{
			name:     "absolute path resolved via suffix match",
			file:     "/workspace/pkg/cli/cache.go",
			diffFile: "pkg/cli/cache.go",
			want:     "pkg/cli/cache.go",
		},
		{
			name:     "basename matched against diff file list",
			file:     "cache.go",
			diffFile: "pkg/cli/cache.go",
			want:     "pkg/cli/cache.go",
		},
		{
			name:     "empty file unchanged",
			file:     "",
			diffFile: "pkg/cli/cache.go",
			want:     "",
		},
		{
			name:     "file not in diff list, no normalization",
			file:     "pkg/cli/missing.go",
			diffFile: "pkg/cli/cache.go",
			want:     "pkg/cli/missing.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeFindingFile(tt.file, []string{tt.diffFile})
			if got != tt.want {
				t.Errorf("normalizeFindingFile(%q, %v) = %q, want %q", tt.file, []string{tt.diffFile}, got, tt.want)
			}
		})
	}
}

func TestNormalizeFindingFile_AmbiguousBasename(t *testing.T) {
	// Two files with the same basename: basename should NOT be resolved.
	diffFiles := []string{"pkg/cli/cache.go", "pkg/foreman/cache.go"}
	got := normalizeFindingFile("cache.go", diffFiles)
	if got != "cache.go" {
		t.Errorf("normalizeFindingFile(%q, %v) = %q, want %q (ambiguous)", "cache.go", diffFiles, got, "cache.go")
	}
}

func TestNormalizeFindingFile_UniqueBasename(t *testing.T) {
	diffFiles := []string{"pkg/cli/cache.go", "pkg/foreman/agent/executor.go"}
	got := normalizeFindingFile("cache.go", diffFiles)
	if got != "pkg/cli/cache.go" {
		t.Errorf("normalizeFindingFile(%q, %v) = %q, want %q", "cache.go", diffFiles, got, "pkg/cli/cache.go")
	}
}

func TestGroundedBlockingFindings_NormalizesFilePath(t *testing.T) {
	// The normalization happens in reviewerGroundedChangedLines, not in
	// groundedBlockingFindings. Here we test that groundedBlockingFindings
	// correctly uses the normalized file path when the changedLines callback
	// is keyed on the normalized path.
	diffFiles := []string{"pkg/cli/cache.go"}

	// Build a changedLines callback that normalizes the file path first,
	// then looks it up — mirroring what reviewerGroundedChangedLines does.
	changedLines := func(file string) map[int]bool {
		normalized := normalizeFindingFile(file, diffFiles)
		if normalized == "pkg/cli/cache.go" {
			return map[int]bool{10: true, 20: true}
		}
		return map[int]bool{}
	}

	tests := []struct {
		name       string
		file       string
		line       int
		wantGround bool
	}{
		{
			name:       "repo-root-relative path grounds",
			file:       "pkg/cli/cache.go",
			line:       10,
			wantGround: true,
		},
		{
			name:       "dot-prefixed path grounds after normalization",
			file:       "./pkg/cli/cache.go",
			line:       10,
			wantGround: true,
		},
		{
			name:       "absolute path grounds after normalization",
			file:       "/workspace/pkg/cli/cache.go",
			line:       20,
			wantGround: true,
		},
		{
			name:       "basename grounds after normalization",
			file:       "cache.go",
			line:       10,
			wantGround: true,
		},
		{
			name:       "wrong line is ungrounded",
			file:       "pkg/cli/cache.go",
			line:       99,
			wantGround: false,
		},
		{
			name:       "unknown file is ungrounded",
			file:       "pkg/cli/missing.go",
			line:       10,
			wantGround: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := []reviewer.Finding{
				{Severity: reviewer.SeverityBlocker, Area: reviewer.AreaScope, Message: "test", File: tt.file, Line: tt.line},
			}
			grounded, _ := groundedBlockingFindings(findings, changedLines)
			gotGround := len(grounded) > 0
			if gotGround != tt.wantGround {
				t.Errorf("groundedBlockingFindings: file=%q line=%d gotGround=%v wantGround=%v",
					tt.file, tt.line, gotGround, tt.wantGround)
			}
		})
	}
}
