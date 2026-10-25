/*
Copyright 2026.

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

// Package macos embeds the macOS deployment manifests that Go code applies,
// so the YAML files in this directory stay the single source of truth.
package macos

import (
	_ "embed"
)

// MetalAgentRBAC is metal-agent-rbac.yaml: the audited shared identity of
// the Metal agent (ServiceAccount, Role llmkube-metal-agent, RoleBinding).
// `llmkube metal token create` creates its Role in a namespace that lacks it.
//
//go:embed metal-agent-rbac.yaml
var MetalAgentRBAC []byte
