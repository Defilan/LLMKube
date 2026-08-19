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
