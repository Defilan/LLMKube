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

// Whitebox tests for the "session" context strategy helpers
// (compactTranscriptForWire / selectWireTranscript) in loop.go. These
// pin the cache-stability and compaction behavior in isolation.

import (
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// assertNoOrphanedToolMessages fails if any RoleTool message in wire has
// a ToolCallID without a preceding assistant ToolCall of the same ID.
func assertNoOrphanedToolMessages(t *testing.T, wire []oai.Message) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range wire {
		if m.Role == oai.RoleAssistant {
			for _, tc := range m.ToolCalls {
				seen[tc.ID] = true
			}
		}
		if m.Role == oai.RoleTool && !seen[m.ToolCallID] {
			t.Errorf("orphaned tool message: tool_call_id %q has no preceding assistant call", m.ToolCallID)
		}
	}
}

func TestCompactTranscript_UnderBudgetReturnsIdentical(t *testing.T) {
	tx := transcriptFixture(5, 200) // small, well under budget
	wire := compactTranscriptForWire(tx, 100000)
	if len(wire) != len(tx) {
		t.Fatalf("wire length: want %d got %d", len(tx), len(wire))
	}
	for i := range tx {
		if wire[i].Content != tx[i].Content || wire[i].Role != tx[i].Role {
			t.Errorf("message %d changed under budget (cache-stability violated)", i)
		}
	}
}

func TestCompactTranscript_ZeroBudgetReturnsIdentical(t *testing.T) {
	tx := transcriptFixture(5, 200)
	wire := compactTranscriptForWire(tx, 0)
	if len(wire) != len(tx) {
		t.Fatalf("wire length: want %d got %d", len(tx), len(wire))
	}
}
