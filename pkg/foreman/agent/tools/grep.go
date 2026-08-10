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

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// defaultGrepMaxMatches caps results so a permissive pattern does not
// dominate the transcript budget. The model can re-grep with a tighter
// pattern if it wants more.
const defaultGrepMaxMatches = 200

// defaultGrepMaxLineChars bounds the text of a single match.
//
// Capping the match COUNT alone does not bound the output, because nothing
// bounded a match's length: the scanner accepts lines up to 1 MiB, so 200
// matches could return ~200 MiB and the result still reported
// "truncated": false, since truncated only ever described the match count.
//
// Minified bundles make that the common case rather than a corner case. A
// coder searching a repo with a vendored three.min.js for "stats" got back
// 649,467 bytes from a handful of matches, because the whole bundle is one
// line. That single tool result pushed the transcript past the stuck-loop
// detector's 140k hard cap and the run was force-terminated at turn 8 with
// verdict INCOMPLETE, having never found the file it was looking for.
//
// 512 chars is far more than enough to identify a hit and decide whether to
// read the file, and it bounds a full result at roughly 200*512 = 100 KiB.
const defaultGrepMaxLineChars = 512

// GrepTool runs a regex search across files under a workspace subtree.
// Skips .git aggressively (only directory, not the .gitignore file).
type GrepTool struct {
	Workspace  string
	MaxMatches int // 0 falls back to defaultGrepMaxMatches
	// MaxLineChars bounds each match's text. 0 falls back to
	// defaultGrepMaxLineChars.
	MaxLineChars int
}

// clipMatchText shortens a match line to at most maxChars characters and
// reports whether it did. The marker matters: an unmarked clip is
// indistinguishable from a genuinely short line, and a model that cannot see
// that its evidence was cut will reason from a partial line as if it were
// whole.
//
// Counts runes rather than bytes so a cut never lands mid-character and hands
// the model invalid UTF-8.
func clipMatchText(s string, maxChars int) (string, bool) {
	if maxChars <= 0 {
		maxChars = defaultGrepMaxLineChars
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s, false
	}
	return fmt.Sprintf("%s... [clipped, line is %d chars]",
		string(runes[:maxChars]), len(runes)), true
}

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Max     int    `json:"max"`
	// MaxLineChars lets the model widen the per-line cap when it genuinely
	// needs more of a long line, rather than being stuck with the default.
	MaxLineChars int `json:"maxLineChars"`
}

type grepMatch struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
	// Clipped marks this individual match as shortened, so a model reading
	// one match in isolation is not misled by the aggregate count.
	Clipped bool `json:"clipped,omitempty"`
}

// Name returns the tool name as advertised to the model.
func (t *GrepTool) Name() string { return "grep" }

// Schema returns the OAI schema advertisement.
func (t *GrepTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name:        "grep",
		Description: "Run a regex search across files under a workspace path. Returns matching lines with file:line.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "pattern": {"type": "string", "description": "Go regexp syntax."},
  "path":    {"type": "string", "description": "Workspace-relative root to search (default \".\")."},
  "max":     {"type": "integer", "minimum": 1, "description": "Cap on returned matches."},
  "maxLineChars": {
    "type": "integer", "minimum": 1,
    "description": "Cap on each match's text length (default 512). Long lines are clipped and marked."
  }
},
"required": ["pattern"]
}`),
	}
}

// errStopWalk is an internal sentinel signaling "cap reached, stop the
// walk cleanly". filepath.SkipAll would work too, but a typed sentinel
// is easier to recognize in the post-walk error check.
var errStopWalk = errors.New("grep: cap reached")

// Execute walks the workspace subtree rooted at args.path and collects
// up to Max matches against args.pattern.
func (t *GrepTool) Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a grepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("grep: bad args: %w", err)
	}
	if a.Pattern == "" {
		return nil, fmt.Errorf("grep: pattern is required")
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return nil, fmt.Errorf("grep: invalid pattern: %w", err)
	}
	if a.Path == "" {
		a.Path = "."
	}
	root, err := resolveInside(t.Workspace, a.Path)
	if err != nil {
		return nil, fmt.Errorf("grep: %w", err)
	}
	limit := a.Max
	if limit <= 0 {
		limit = t.MaxMatches
		if limit <= 0 {
			limit = defaultGrepMaxMatches
		}
	}
	lineLimit := a.MaxLineChars
	if lineLimit <= 0 {
		lineLimit = t.MaxLineChars
		if lineLimit <= 0 {
			lineLimit = defaultGrepMaxLineChars
		}
	}
	matches := make([]grepMatch, 0, limit)
	clipped := 0

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, inErr error) error {
		if inErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= limit {
			return errStopWalk
		}
		f, err := os.Open(p) //nolint:gosec // G304: walking a resolveInside-validated root
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		lineNo := 0
		rel, _ := filepath.Rel(t.Workspace, p)
		for sc.Scan() {
			lineNo++
			if re.Match(sc.Bytes()) {
				text, wasClipped := clipMatchText(sc.Text(), lineLimit)
				if wasClipped {
					clipped++
				}
				matches = append(matches, grepMatch{File: rel, Line: lineNo, Text: text, Clipped: wasClipped})
				if len(matches) >= limit {
					return errStopWalk
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) && !errors.Is(walkErr, ctx.Err()) {
		return nil, fmt.Errorf("grep: walk: %w", walkErr)
	}
	return &agent.ToolResult{
		Output: map[string]any{
			"pattern": a.Pattern,
			"matches": matches,
			// truncated keeps its original meaning: the match LIST was cut
			// short. clippedLines is reported separately so a model can tell
			// "there are more matches" from "these matches are abridged",
			// which call for different next steps.
			"truncated":    len(matches) >= limit,
			"clippedLines": clipped,
		},
	}, nil
}
