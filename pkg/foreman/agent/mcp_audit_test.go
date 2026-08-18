package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAggregateMCPCalls_MixedServers(t *testing.T) {
	events := []MCPCallEvent{
		{Server: "perplexity", Tool: "perplexity_ask", CallError: ""},
		{Server: "perplexity", Tool: "perplexity_ask", CallError: ""},
		{Server: "perplexity", Tool: "perplexity_ask", CallError: "boom"},
		{Server: "context7", Tool: "query-docs", CallError: ""},
		{Server: "context7", Tool: "query-docs", CallError: ""},
	}

	got := aggregateMCPCalls(events)

	want := []MCPToolStat{
		{Server: "context7", Tool: "query-docs", OK: 2, Err: 0},
		{Server: "perplexity", Tool: "perplexity_ask", OK: 2, Err: 1},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregateMCPCalls mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestAggregateMCPCalls_KeyOnCallError(t *testing.T) {
	events := []MCPCallEvent{
		{Server: "s", Tool: "ok", CallError: ""},
		{Server: "s", Tool: "err", CallError: "boom"},
	}

	got := aggregateMCPCalls(events)

	want := []MCPToolStat{
		{Server: "s", Tool: "err", OK: 0, Err: 1},
		{Server: "s", Tool: "ok", OK: 1, Err: 0},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keying on CallError mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestAggregateMCPCalls_IsErrorButNoCallErrorCountsOK(t *testing.T) {
	// A tool-level isError=true with no transport error leaves CallError empty,
	// so it must land in the OK column — that is the exact reporting bug this
	// work corrects.
	events := []MCPCallEvent{
		{Server: "s", Tool: "negresult", CallError: ""},
		{Server: "s", Tool: "negresult", CallError: ""},
		{Server: "s", Tool: "negresult", CallError: ""},
	}

	got := aggregateMCPCalls(events)

	want := []MCPToolStat{
		{Server: "s", Tool: "negresult", OK: 3, Err: 0},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("isError-but-no-CallError mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestAggregateMCPCalls_DeterministicSort(t *testing.T) {
	// Build from a shuffled input twice and assert the exact same slice, so
	// the output is stable across runs regardless of map iteration order.
	base := []MCPCallEvent{
		{Server: "zeta", Tool: "z1", CallError: ""},
		{Server: "alpha", Tool: "b2", CallError: "e"},
		{Server: "alpha", Tool: "a1", CallError: ""},
		{Server: "zeta", Tool: "z0", CallError: "e"},
		{Server: "alpha", Tool: "a1", CallError: ""},
		{Server: "mid", Tool: "m1", CallError: ""},
	}

	first := aggregateMCPCalls(base)

	// Reverse the input to exercise a different insertion order.
	reversed := make([]MCPCallEvent, len(base))
	for i, ev := range base {
		reversed[len(base)-1-i] = ev
	}
	second := aggregateMCPCalls(reversed)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("non-deterministic sort\nfirst:  %+v\nsecond: %+v", first, second)
	}

	want := []MCPToolStat{
		{Server: "alpha", Tool: "a1", OK: 2, Err: 0},
		{Server: "alpha", Tool: "b2", OK: 0, Err: 1},
		{Server: "mid", Tool: "m1", OK: 1, Err: 0},
		{Server: "zeta", Tool: "z0", OK: 0, Err: 1},
		{Server: "zeta", Tool: "z1", OK: 1, Err: 0},
	}

	if !reflect.DeepEqual(first, want) {
		t.Fatalf("sorted result mismatch\n got: %+v\nwant: %+v", first, want)
	}
}

func TestAggregateMCPCalls_EmptyReturnsNil(t *testing.T) {
	if got := aggregateMCPCalls(nil); got != nil {
		t.Fatalf("expected nil for nil input, got %+v", got)
	}
	if got := aggregateMCPCalls([]MCPCallEvent{}); got != nil {
		t.Fatalf("expected nil for empty input, got %+v", got)
	}
}

func TestBuildMCPAudit_EmptyEventsOmitsCallsKey(t *testing.T) {
	audit := buildMCPAudit(2, 4, nil)

	if audit.Calls != nil {
		t.Fatalf("expected nil Calls, got %+v", audit.Calls)
	}
	if audit.ServersConfigured != 2 || audit.ToolsAdded != 4 {
		t.Fatalf("unexpected counts: %+v", audit)
	}

	b, err := json.Marshal(audit)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if got := string(b); got != `{"serversConfigured":2,"toolsAdded":4}` {
		t.Fatalf("marshalled record should omit calls key\n got: %s\nwant: %s", got, `{"serversConfigured":2,"toolsAdded":4}`)
	}
}

func TestBuildMCPAudit_ZeroCountsMarshalAsZero(t *testing.T) {
	events := []MCPCallEvent{
		{Server: "perplexity", Tool: "perplexity_ask", CallError: ""},
		{Server: "perplexity", Tool: "perplexity_search", CallError: "boom"},
	}
	audit := buildMCPAudit(2, 4, events)

	b, err := json.Marshal(audit)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// perplexity_ask has err:0, perplexity_search has ok:0 — both must be
	// present as 0, not omitted.
	want := `{"serversConfigured":2,"toolsAdded":4,"calls":[` +
		`{"server":"perplexity","tool":"perplexity_ask","ok":1,"err":0},` +
		`{"server":"perplexity","tool":"perplexity_search","ok":0,"err":1}]}`
	if got := string(b); got != want {
		t.Fatalf("zero counts must marshal as 0, not be omitted\n got: %s\nwant: %s", got, want)
	}
}
