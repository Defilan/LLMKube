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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runGrep executes the tool against a workspace and returns its output map.
func runGrep(t *testing.T, ws string, args map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := (&GrepTool{Workspace: ws}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("grep execute: %v", err)
	}
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("grep output is %T, want map[string]any", res.Output)
	}
	return out
}

// writeWorkspace builds a temp workspace from name -> contents.
func writeWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	for name, body := range files {
		full := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return ws
}

// A single match on a minified bundle must not be able to flood the transcript.
//
// This is the regression for a real force-terminated coder run. The repo had a
// vendored three.min.js, the model grepped for "stats", and the tool returned
// 649,467 bytes from a handful of matches because the whole bundle is one line.
// That one result pushed the transcript past the stuck-loop detector's 140k
// hard cap, and the run ended at turn 8 with verdict INCOMPLETE without ever
// finding the file it was looking for.
//
// The match cap did not help: capping how MANY matches come back does nothing
// when one of them is half a megabyte.
func TestGrepClipsLongMatchLines(t *testing.T) {
	minified := "!function(t,e){" + strings.Repeat("stats.update();", 50_000) + "}"
	ws := writeWorkspace(t, map[string]string{
		"vendor/three.min.js": minified,
		"js/app.js":           "// stats panel\nconst stats = init();\n",
	})

	out := runGrep(t, ws, map[string]any{"pattern": "stats"})

	matches, ok := out["matches"].([]grepMatch)
	if !ok {
		t.Fatalf("matches is %T, want []grepMatch", out["matches"])
	}
	if len(matches) == 0 {
		t.Fatal("expected matches in both files")
	}

	total := 0
	sawClipped := false
	for _, m := range matches {
		if n := len([]rune(m.Text)); n > defaultGrepMaxLineChars+64 {
			t.Errorf("match %s:%d is %d chars; the per-line cap is %d "+
				"(+ room for the marker). An unbounded match line is what "+
				"blew the transcript budget.", m.File, m.Line, n, defaultGrepMaxLineChars)
		}
		if m.Clipped {
			sawClipped = true
		}
		total += len(m.Text)
	}

	if !sawClipped {
		t.Error("the minified line should have been clipped and marked; " +
			"an unmarked clip is indistinguishable from a short line")
	}
	if n, _ := out["clippedLines"].(int); n < 1 {
		t.Errorf("clippedLines = %d, want at least 1", n)
	}
	// The real failure was 649,467 bytes. Bound the whole result generously
	// and still catch anything of that order.
	if total > 200*defaultGrepMaxLineChars+4096 {
		t.Errorf("total match text is %d bytes; the point of the cap is that a "+
			"full result stays near %d", total, 200*defaultGrepMaxLineChars)
	}
}

// A clipped line must say so in the payload, because the model decides what to
// read next based on what it thinks it saw.
func TestGrepMarksClippedTextVisibly(t *testing.T) {
	ws := writeWorkspace(t, map[string]string{
		"big.js": strings.Repeat("x", 5000) + "needle",
	})

	out := runGrep(t, ws, map[string]any{"pattern": "needle"})
	matches := out["matches"].([]grepMatch)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if !strings.Contains(matches[0].Text, "clipped") {
		t.Errorf("clipped text carries no marker: %q", matches[0].Text[:80])
	}
	if !matches[0].Clipped {
		t.Error("Clipped flag not set on a shortened match")
	}
}

// Short lines must pass through byte-for-byte. A cap that quietly rewrites
// ordinary results would be worse than the bug it fixes.
func TestGrepLeavesShortLinesIntact(t *testing.T) {
	ws := writeWorkspace(t, map[string]string{
		"js/ui.js": "const label = 'Health';\nconst value = '80%';\n",
	})

	out := runGrep(t, ws, map[string]any{"pattern": "Health"})
	matches := out["matches"].([]grepMatch)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if matches[0].Text != "const label = 'Health';" {
		t.Errorf("short line was altered: %q", matches[0].Text)
	}
	if matches[0].Clipped {
		t.Error("Clipped set on a line that fits")
	}
	if n, _ := out["clippedLines"].(int); n != 0 {
		t.Errorf("clippedLines = %d, want 0", n)
	}
}

// truncated and clippedLines answer different questions, and conflating them
// would tell a model to re-grep when it should widen instead.
func TestGrepTruncatedAndClippedAreIndependent(t *testing.T) {
	ws := writeWorkspace(t, map[string]string{
		"a.js": strings.Repeat("hit short line\n", 10),
	})

	out := runGrep(t, ws, map[string]any{"pattern": "hit", "max": 3})
	if tr, _ := out["truncated"].(bool); !tr {
		t.Error("truncated should be true when the match list hit its cap")
	}
	if n, _ := out["clippedLines"].(int); n != 0 {
		t.Errorf("clippedLines = %d, want 0: these lines are short", n)
	}
}

// Clipping counts runes so a cut never emits invalid UTF-8.
func TestGrepClipDoesNotSplitRunes(t *testing.T) {
	body := strings.Repeat("こんにちは", 500) + "needle"
	ws := writeWorkspace(t, map[string]string{"u.txt": body})

	out := runGrep(t, ws, map[string]any{"pattern": "needle"})
	matches := out["matches"].([]grepMatch)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if !json.Valid([]byte(strconvQuote(matches[0].Text))) {
		t.Error("clipped text is not valid UTF-8; the cut landed mid-rune")
	}
}

// strconvQuote renders a string as a JSON string so validity can be checked
// the same way the wire encoder would see it.
func strconvQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
