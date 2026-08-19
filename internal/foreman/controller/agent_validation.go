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

package controller

import (
	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// Validated-condition plumbing (#1609). The judgment itself lives on
// AgentSpec.ConfigFloor in the api package, shared with the executor's
// task-record stamp so the two surfaces cannot drift.
// conditionValidated is the condition type the M3 stub reserved.
const conditionValidated = "Validated"

// validateAgentConfig is the config floor behind the Validated condition;
// see AgentSpec.ConfigFloor for the rules and the incident behind them.
func validateAgentConfig(spec *foremanv1alpha1.AgentSpec) (valid bool, reason, message string) {
	return spec.ConfigFloor()
}
