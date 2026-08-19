package v1alpha1

import "testing"

// The config floor is shared: the Agent controller publishes it as the
// Validated condition, and the executor stamps failing runs' task records
// (#1609). One implementation, so the two surfaces cannot drift.
func TestAgentSpecConfigFloor(t *testing.T) {
	ok, reason, _ := (&AgentSpec{
		Role: AgentRoleReviewer, SystemPrompt: "# p",
		Tools: []string{"fetch_issue", "submit_result"},
	}).ConfigFloor()
	if !ok || reason != AgentConfigReasonComplete {
		t.Fatalf("complete config must pass, got ok=%v reason=%q", ok, reason)
	}

	ok, reason, msg := (&AgentSpec{
		Role: AgentRoleReviewer, Tools: []string{"submit_result"},
	}).ConfigFloor()
	if ok || reason != AgentConfigReasonNoSystemPrompt || msg == "" {
		t.Fatalf("empty prompt must fail with reason+message, got ok=%v reason=%q msg=%q", ok, reason, msg)
	}

	ok, reason, _ = (&AgentSpec{
		Role: AgentRoleReviewer, SystemPrompt: "# p", Tools: []string{"bash", "submit_result"},
	}).ConfigFloor()
	if ok || reason != AgentConfigReasonMissingRoleTools {
		t.Fatalf("reviewer without fetch_issue must fail, got ok=%v reason=%q", ok, reason)
	}
}

// #1613: a deterministic agent (single non-terminal tool, e.g. the gate
// verifier) never renders a prompt — the executor routes it through
// pickDeterministicTool with no model loop — so the NoSystemPrompt floor
// must not fire for it. LLM-path agents keep the requirement.
func TestAgentSpecConfigFloorDeterministic(t *testing.T) {
	// Gate-shaped spec: deterministic, empty prompt, passes.
	ok, reason, _ := (&AgentSpec{
		Role: AgentRoleVerifier, Tools: []string{"run_gate_job"},
	}).ConfigFloor()
	if !ok || reason != AgentConfigReasonComplete {
		t.Fatalf("deterministic single-tool spec must pass without a prompt, got ok=%v reason=%q", ok, reason)
	}

	// LLM-path spec with an empty prompt still fails.
	ok, reason, _ = (&AgentSpec{
		Role: AgentRoleCoder, Tools: []string{"read_file", "bash", "submit_result"},
	}).ConfigFloor()
	if ok || reason != AgentConfigReasonNoSystemPrompt {
		t.Fatalf("LLM-path spec with empty prompt must fail NoSystemPrompt, got ok=%v reason=%q", ok, reason)
	}

	// A reviewer missing fetch_issue still fails MissingRoleTools.
	ok, reason, _ = (&AgentSpec{
		Role: AgentRoleReviewer, SystemPrompt: "# p", Tools: []string{"bash", "submit_result"},
	}).ConfigFloor()
	if ok || reason != AgentConfigReasonMissingRoleTools {
		t.Fatalf("reviewer without fetch_issue must fail, got ok=%v reason=%q", ok, reason)
	}

	// A spec whose single tool is submit_result is NOT deterministic.
	ok, reason, _ = (&AgentSpec{
		Role: AgentRoleCoder, Tools: []string{"submit_result"},
	}).ConfigFloor()
	if ok || reason != AgentConfigReasonNoSystemPrompt {
		t.Fatalf("single submit_result tool is not deterministic; must fail NoSystemPrompt, got ok=%v reason=%q", ok, reason)
	}
}
