package agent

// MCPNameSeparator joins the segments of an MCP tool's namespaced name. It is
// "__" rather than "/" because the OAI tool-call spec constrains function.name
// to ^[a-zA-Z0-9_-]+$: a "/" makes every request carrying an MCP tool
// malformed, and providers that validate reject it before the model runs
// (#1527).
const MCPNameSeparator = "__"

// MCPToolNamePrefix is the prefix every MCP-provided tool name carries, so a
// transcript consumer can tell an MCP result from a native tool's.
//
// It lives here rather than in pkg/foreman/agent/mcp because that package
// already imports this one, and the two must not drift apart again: #1527
// changed the separator in the mcp package but left this package's own "mcp/"
// literal behind in the coder grounding rail, which then matched nothing and
// silently collected no evidence on every run.
const MCPToolNamePrefix = "mcp" + MCPNameSeparator
