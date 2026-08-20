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

package v1alpha1

import (
	"fmt"
	"slices"
	"strings"
)

// Config-floor reasons (#1609). Shared here because two surfaces publish
// the same judgment and must not drift: the Agent controller writes it as
// the Validated condition, and the executor stamps a failing Agent's task
// records so a verdict minted by an under-configured agent carries the
// fact in its own record.
const (
	AgentConfigReasonComplete         = "ConfigComplete"
	AgentConfigReasonNoSystemPrompt   = "NoSystemPrompt"
	AgentConfigReasonMissingRoleTools = "MissingRoleTools"
)

// agentRoleRequiredTools names tools a role cannot meaningfully run
// without. A reviewer without fetch_issue cannot read the issue it is
// reviewing against: the issue-ask and scope-overlap rails depend on the
// fetched body, and without the tool they can only ever record a skip.
var agentRoleRequiredTools = map[AgentRole][]string{
	AgentRoleReviewer: {"fetch_issue"},
}

// isDeterministicSpec mirrors pkg/foreman/agent.IsDeterministicAgent: it
// reports whether the Agent runs the model-free branch, i.e. the executor
// routes it by ENDPOINT ABSENCE (no LLM at all), not by tool shape. A
// deterministic agent never renders a system prompt, so the
// NoSystemPrompt floor requirement does not apply to it.
//
// Kept as a private copy rather than importing the agent package because
// the api package is the leaf both sides depend on; importing
// pkg/foreman/agent here would invert the dependency (the executor imports
// this package for AgentSpec). The two implementations are asserted
// equivalent by the equivalence test in pkg/foreman/agent, following the
// same pattern the admission webhook uses.
func (s *AgentSpec) isDeterministicSpec() bool {
	if s.Provider != "" && s.Provider != AgentProviderLocal {
		return false
	}
	return s.InferenceServiceRef.Name == ""
}

// ConfigFloor reports whether the spec meets the minimum an agent of this
// role needs to produce groundable work, with a machine reason and a
// human message when it does not. It is a warning surface, not an
// admission gate. The incident behind it (#1609): a hand-created reviewer
// Agent ran for days with no systemPrompt and no fetch_issue, receiving
// an empty system message while task prompts said "follow Step 1 of your
// system prompt", and every verdict it minted was uninstructed with
// nothing anywhere saying so.
//
// A deterministic agent (no endpoint; the executor runs its first
// non-terminal tool directly, no model loop) never renders a system
// prompt, so the NoSystemPrompt requirement is skipped for it (#1613).
// The role-required tools check still applies: a deterministic reviewer
// that cannot fetch the issue it is reviewing is still misconfigured.
func (s *AgentSpec) ConfigFloor() (ok bool, reason, message string) {
	if strings.TrimSpace(s.SystemPrompt) == "" && !s.isDeterministicSpec() {
		return false, AgentConfigReasonNoSystemPrompt,
			"spec.systemPrompt is empty: the model runs uninstructed while task prompts reference its steps"
	}
	for _, required := range agentRoleRequiredTools[s.Role] {
		if !slices.Contains(s.Tools, required) {
			return false, AgentConfigReasonMissingRoleTools,
				fmt.Sprintf("role %q requires tool %q in spec.tools: without it the %s cannot ground its verdicts",
					s.Role, required, s.Role)
		}
	}
	return true, AgentConfigReasonComplete, "systemPrompt present and role-required tools advertised"
}
