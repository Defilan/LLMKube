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
	"fmt"
	"unicode/utf8"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// MaxSubmitSummaryLen is the cap on submit_result.summary length. It
// has to fit the AgenticTask status payload comfortably while still
// forcing a one-sentence outcome rather than a wall of text.
const MaxSubmitSummaryLen = 280

// SubmitResultTool is the terminal tool. When the model calls it, the
// loop captures the envelope and exits. The fields map directly onto
// AgenticTaskStatus.Verdict + Status.Result + the commit message the
// Phase D repo helpers use to push the branch.
type SubmitResultTool struct{}

type submitResultArgs struct {
	Verdict       string         `json:"verdict"`
	Summary       string         `json:"summary"`
	CommitMessage string         `json:"commit_message"`
	Extra         map[string]any `json:"extra"`
}

// Name returns the tool name as advertised to the model.
func (SubmitResultTool) Name() string { return "submit_result" }

// Schema returns the OAI schema advertisement.
func (SubmitResultTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name:        "submit_result",
		Description: "Terminal tool. Submit the final outcome. The loop exits after this call.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "verdict":        {"type": "string", "enum": ["GO", "NO-GO", "ERROR"]},
  "summary":        {"type": "string", "description": "One-sentence outcome summary (1-280 chars)."},
  "commit_message": {"type": "string",
    "description": "Full commit message including subject, body, and Fixes #N if applicable."},
  "extra": {"type": "object",
    "description": "Structured extra fields the executor may surface in status.result.extra."}
},
"required": ["verdict", "summary"]
}`),
	}
}

// Execute validates the envelope and returns it as the terminal result.
// Validation is intentionally strict: a bad verdict here means the
// model is hallucinating outside the locked enum, and we surface that
// rather than papering over it.
func (SubmitResultTool) Execute(_ context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a submitResultArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("submit_result: bad args: %w", err)
	}
	switch a.Verdict {
	case "GO", "NO-GO", "ERROR":
	default:
		return nil, fmt.Errorf("submit_result: invalid verdict %q (must be GO, NO-GO, or ERROR)", a.Verdict)
	}
	if a.Summary == "" {
		return nil, fmt.Errorf("submit_result: summary is required")
	}
	if len(a.Summary) > MaxSubmitSummaryLen {
		a.Summary = runeSafeTruncate(a.Summary, MaxSubmitSummaryLen)
	}
	return &agent.ToolResult{
		Terminal:      true,
		Verdict:       a.Verdict,
		Summary:       a.Summary,
		CommitMessage: a.CommitMessage,
		Extra:         a.Extra,
		Output: map[string]any{
			"accepted": true,
			"verdict":  a.Verdict,
		},
	}, nil
}

// runeSafeTruncate returns the first `n` bytes of s followed by an
// ellipsis, without splitting a multi-byte rune. The returned string
// is at most n bytes long (the ellipsis replaces trailing bytes if
// truncation would exceed the cap).
func runeSafeTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	ell := "…"
	// Reserve room for the ellipsis.
	avail := n - len(ell)
	if avail <= 0 {
		return ell
	}
	trunc := s[:avail]
	// Back up if we landed inside a multi-byte rune so we don't
	// produce invalid UTF-8.
	for len(trunc) > 0 && !utf8.ValidString(trunc) {
		trunc = trunc[:len(trunc)-1]
	}
	return trunc + ell
}
