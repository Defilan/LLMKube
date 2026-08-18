package agent

import "sort"

// MCPCallEvent is a single MCP tool call observed during a Foreman run. It
// mirrors the per-call "mcp tool call" record the MCP registry accumulates
// over a task. CallError is non-empty only when the call failed at the
// transport/protocol level (e.g. a network or HTTP error); a tool-level
// isError=true with no transport error leaves CallError empty.
type MCPCallEvent struct {
	Server    string
	Tool      string
	CallError string
}

// MCPToolStat is the aggregated outcome of all calls to one (Server, Tool)
// pair over a task. OK counts calls with no transport error; Err counts calls
// with one.
type MCPToolStat struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
	OK     int    `json:"ok"`
	Err    int    `json:"err"`
}

// aggregateMCPCalls folds a set of per-call events into a per-tool histogram
// keyed on (Server, Tool). A call counts as OK when CallError is empty and as
// Err otherwise.
//
// Keying on CallError — not on any isError flag — keeps a tool-level
// isError=true with no transport error in the OK column. That is a successful
// call that returned a negative result; counting it as an error is the exact
// reporting bug this work addresses.
//
// The result is sorted by Server then Tool so the record is stable across
// runs. An empty input returns nil, not an empty slice.
func aggregateMCPCalls(events []MCPCallEvent) []MCPToolStat {
	if len(events) == 0 {
		return nil
	}

	type key struct {
		server string
		tool   string
	}
	agg := make(map[key]*MCPToolStat)
	for _, ev := range events {
		k := key{server: ev.Server, tool: ev.Tool}
		stat, ok := agg[k]
		if !ok {
			stat = &MCPToolStat{Server: ev.Server, Tool: ev.Tool}
			agg[k] = stat
		}
		if ev.CallError == "" {
			stat.OK++
		} else {
			stat.Err++
		}
	}

	out := make([]MCPToolStat, 0, len(agg))
	for _, stat := range agg {
		out = append(out, *stat)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Tool < out[j].Tool
	})
	return out
}

// MCPAudit captures MCP usage for a Foreman run: how many MCP servers were
// configured, how many tools were added to the registry, and a per-tool call
// histogram. It is pure data; embedding it into the foreman.audit.v1 record
// (the foreman-audit-<task> ConfigMap) is a deliberate follow-up, not part of
// these aggregation functions.
type MCPAudit struct {
	ServersConfigured int           `json:"serversConfigured"`
	ToolsAdded        int           `json:"toolsAdded"`
	Calls             []MCPToolStat `json:"calls,omitempty"`
}

// buildMCPAudit assembles an MCPAudit from the registry's configured/added
// counts and the accumulated per-call events. With no events, Calls is nil so
// the field is absent from the marshalled record rather than an empty array.
func buildMCPAudit(serversConfigured, toolsAdded int, events []MCPCallEvent) MCPAudit {
	return MCPAudit{
		ServersConfigured: serversConfigured,
		ToolsAdded:        toolsAdded,
		Calls:             aggregateMCPCalls(events),
	}
}
