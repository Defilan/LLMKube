package agent

import (
	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// stampAgentConfigWarnings records in the terminal extra that the agent
// producing this result runs below its role's config floor (#1609). The
// Validated condition on the Agent CR covers the control plane; this covers
// the artifact, so a verdict minted by an uninstructed or under-tooled
// agent is self-incriminating in its own task record. A complete config
// stamps nothing: a warning on every record is noise. Owns initializing a
// nil Extra map, because this stamp exists precisely for mis-configured
// setups and must not depend on the model having sent the optional field.
func stampAgentConfigWarnings(terminal *ToolResult, spec *foremanv1alpha1.AgentSpec) {
	if terminal == nil || spec == nil {
		return
	}
	ok, reason, message := spec.ConfigFloor()
	if ok {
		return
	}
	if terminal.Extra == nil {
		terminal.Extra = map[string]any{}
	}
	existing, _ := terminal.Extra["agentConfigWarnings"].([]string)
	terminal.Extra["agentConfigWarnings"] = append(existing, reason+": "+message)
}
