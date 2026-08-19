package agent

import (
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// A verdict minted by an under-configured agent must carry that fact in its
// own task record (#1609). The Validated condition covers the control plane;
// this covers the artifact, so an uninstructed review is self-incriminating
// even if nobody looked at `kubectl get agents`.
func TestStampAgentConfigWarnings(t *testing.T) {
	blind := foremanv1alpha1.AgentSpec{
		Role:  foremanv1alpha1.AgentRoleReviewer,
		Tools: []string{"read_file", "grep", "bash", "submit_result"},
	}
	terminal := &ToolResult{}
	stampAgentConfigWarnings(terminal, &blind)
	warnings, _ := terminal.Extra["agentConfigWarnings"].([]string)
	if len(warnings) != 1 {
		t.Fatalf("want 1 warning (and a nil Extra initialized), got %v", terminal.Extra)
	}

	// A complete config stamps nothing: a warning on every record is noise.
	complete := foremanv1alpha1.AgentSpec{
		Role: foremanv1alpha1.AgentRoleReviewer, SystemPrompt: "# p",
		Tools: []string{"fetch_issue", "submit_result"},
	}
	terminal = &ToolResult{Extra: map[string]any{}}
	stampAgentConfigWarnings(terminal, &complete)
	if _, present := terminal.Extra["agentConfigWarnings"]; present {
		t.Fatalf("complete config must not stamp, got %v", terminal.Extra)
	}

	// Nil terminal must not panic.
	stampAgentConfigWarnings(nil, &blind)
}
