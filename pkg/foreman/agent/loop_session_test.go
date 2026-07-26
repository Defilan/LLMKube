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
	wire, _ := compactTranscriptForWire(tx, 100000, 0)
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
	wire, _ := compactTranscriptForWire(tx, 0, 0)
	if len(wire) != len(tx) {
		t.Fatalf("wire length: want %d got %d", len(tx), len(wire))
	}
}

func TestCompactTranscript_OverBudgetDropsOldestMiddle(t *testing.T) {
	// 8 turn-groups, each tool ~4000 bytes (~1000 tokens). Head is tiny.
	tx := transcriptFixture(8, 4000)
	budget := 3500 // tokens: keeps head + a few newest groups
	wire, _ := compactTranscriptForWire(tx, budget, 0)

	// Something was dropped.
	if len(wire) >= len(tx) {
		t.Fatalf("expected compaction to drop messages: in=%d out=%d", len(tx), len(wire))
	}
	// Head preserved: system then the original task message.
	if wire[0].Role != oai.RoleSystem {
		t.Fatalf("head[0] not system: %s", wire[0].Role)
	}
	if wire[1].Role != oai.RoleUser || wire[1].Content != "fix issue 510" {
		t.Fatalf("original task message not pinned: %+v", wire[1])
	}
	// Most recent turn-group preserved: last two messages of tx survive
	// as the last two of wire.
	if wire[len(wire)-1].Content != tx[len(tx)-1].Content {
		t.Errorf("most recent tool result not kept")
	}
	if wire[len(wire)-2].Role != oai.RoleAssistant {
		t.Errorf("most recent assistant turn not kept")
	}
	// Under budget after compaction.
	if approxTokens(wire) > budget {
		t.Errorf("still over budget after compaction: %d > %d", approxTokens(wire), budget)
	}
	// No orphaned tool_call_id.
	assertNoOrphanedToolMessages(t, wire)
}

func TestCompactTranscript_DegenerateKeepsHeadAndLastGroup(t *testing.T) {
	tx := transcriptFixture(4, 8000)                // each tool ~2000 tokens
	wire, _ := compactTranscriptForWire(tx, 100, 0) // absurdly small budget

	if wire[0].Role != oai.RoleSystem || wire[1].Role != oai.RoleUser {
		t.Fatalf("head not preserved: %+v", wire[:2])
	}
	// Last turn-group (assistant + tool) preserved as the final two messages.
	if wire[len(wire)-1].Role != oai.RoleTool || wire[len(wire)-2].Role != oai.RoleAssistant {
		t.Fatalf("last turn-group not preserved: tail=%+v", wire[len(wire)-2:])
	}
	assertNoOrphanedToolMessages(t, wire)
}

func TestSelectWireTranscript_SessionDoesNotMask(t *testing.T) {
	tx := transcriptFixture(6, 4000)
	cfg := LoopConfig{ContextStrategy: ContextStrategySession, ContextWindowTokens: 1000000}
	wire, _ := selectWireTranscript(cfg, tx, 0)
	for i := range tx {
		if wire[i].Content != tx[i].Content {
			t.Errorf("session strategy masked content at %d", i)
		}
	}
}

func TestSelectWireTranscript_WindowMasks(t *testing.T) {
	tx := transcriptFixture(6, 4000)
	cfg := LoopConfig{ContextStrategy: ContextStrategyWindow, ObservationWindowTurns: 1}
	wire, _ := selectWireTranscript(cfg, tx, 0)
	changed := false
	for i := range tx {
		if tx[i].Role == oai.RoleTool && wire[i].Content != tx[i].Content {
			changed = true
		}
	}
	if !changed {
		t.Error("window strategy did not mask any tool messages")
	}
}

func TestSelectWireTranscript_EmptyDefaultsToWindow(t *testing.T) {
	tx := transcriptFixture(6, 4000)
	got, _ := selectWireTranscript(LoopConfig{ObservationWindowTurns: 1}, tx, 0)
	want := maskTranscriptForWire(tx, 1, 0)
	if len(got) != len(want) {
		t.Fatalf("empty strategy did not route to window: len got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if got[i].Content != want[i].Content {
			t.Errorf("message %d differs from window output", i)
		}
	}
}

// wireIsPrefixOf reports whether prev is an unchanged leading run of next.
// This is the property the server's KV cache actually needs: it matches on
// prefix, so a payload that keeps the previous turn's payload intact at the
// front reuses the cache, and one that does not re-prefills everything.
func wireIsPrefixOf(prev, next []oai.Message) bool {
	if len(prev) > len(next) {
		return false
	}
	for i := range prev {
		if prev[i].Role != next[i].Role ||
			prev[i].Content != next[i].Content ||
			prev[i].ToolCallID != next[i].ToolCallID {
			return false
		}
	}
	return true
}

// TestCompactTranscript_PrefixStableAcrossTurns is the regression for #1286.
//
// Compacting only to the budget line leaves the payload sitting exactly at the
// ceiling, so the next turn is over again and drops one more group. The prefix
// then changes on EVERY turn, the KV cache matches nothing, and the full
// context is re-prefilled each turn: measured live at 95,538 tokens and
// n_prompt_tokens_cache=0, about 12.6 minutes of prefill per turn.
//
// Compacting to a low-water mark buys headroom, so most turns only append and
// the prefix survives. This walks a run turn by turn and counts how often the
// prefix breaks, which is exactly how often a real server pays a re-prefill.
func TestCompactTranscript_PrefixStableAcrossTurns(t *testing.T) {
	const (
		budget    = 20000 // tokens
		toolBytes = 4000  // ~1000 tokens per turn-group
		turns     = 30    // well past the budget, so compaction must happen
	)

	var prev []oai.Message
	drop := 0
	breaks, compactions := 0, 0
	for n := 1; n <= turns; n++ {
		wire, next := compactTranscriptForWire(transcriptFixture(n, toolBytes), budget, drop)
		drop = next
		if approxTokens(wire) > budget {
			t.Fatalf("turn %d: payload over budget: %d > %d", n, approxTokens(wire), budget)
		}
		if prev != nil && !wireIsPrefixOf(prev, wire) {
			breaks++
			compactions++
		}
		prev = wire
	}

	// Each break is one full re-prefill. Dropping the minimum would break on
	// essentially every turn past the budget (about 10 of these 30 turns);
	// with a low-water mark the run should pay it only a couple of times.
	if breaks > 3 {
		t.Errorf("wire prefix broke %d times over %d turns; the KV cache is being "+
			"invalidated nearly every turn, which is #1286", breaks, turns)
	}
	if compactions == 0 {
		t.Fatal("fixture never crossed the budget; this test proves nothing as written")
	}
}

// TestCompactTranscript_CompactsBelowBudgetNotToIt pins the mechanism directly:
// a compaction must leave real headroom, otherwise the next turn re-compacts
// and the prefix churns again.
func TestCompactTranscript_CompactsBelowBudgetNotToIt(t *testing.T) {
	const budget = 20000
	tx := transcriptFixture(40, 4000) // far over budget, forces a compaction
	wire, _ := compactTranscriptForWire(tx, budget, 0)

	got := approxTokens(wire)
	want := budget * sessionCompactionTargetPercent / 100
	if got > want {
		t.Errorf("compaction left %d tokens, above the %d low-water mark: "+
			"no headroom means the next turn re-compacts and the prefix churns",
			got, want)
	}
	// The existing guarantees still hold.
	if wire[0].Role != oai.RoleSystem || wire[1].Content != "fix issue 510" {
		t.Errorf("pinned head not preserved: %+v %+v", wire[0], wire[1])
	}
	if wire[len(wire)-1].Content != tx[len(tx)-1].Content {
		t.Error("most recent tool result not kept")
	}
	assertNoOrphanedToolMessages(t, wire)
}
