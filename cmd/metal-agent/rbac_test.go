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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// rbacManifestPath locates deployment/macos/metal-agent-rbac.yaml relative to
// this test file rather than the working directory `go test` happens to run
// from, so the test passes whether it's invoked from the module root or from
// this package's directory.
func rbacManifestPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file to find the repo root")
	}
	// cmd/metal-agent/rbac_test.go -> repo root is two directories up.
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	return filepath.Join(root, "deployment", "macos", "metal-agent-rbac.yaml")
}

// loadRBACRole reads deployment/macos/metal-agent-rbac.yaml and returns the
// rbac.authorization.k8s.io/v1 Role document it contains. The manifest also
// has a ServiceAccount and a RoleBinding document; those are skipped.
func loadRBACRole(t *testing.T) *rbacv1.Role {
	t.Helper()
	path := rbacManifestPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var role *rbacv1.Role
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &kind); err != nil {
			t.Fatalf("parse document kind in %s: %v", path, err)
		}
		if kind.Kind != "Role" {
			continue
		}
		role = &rbacv1.Role{}
		if err := yaml.Unmarshal([]byte(doc), role); err != nil {
			t.Fatalf("parse Role document in %s: %v", path, err)
		}
	}
	if role == nil {
		t.Fatalf("%s has no Role document", path)
	}
	return role
}

// roleGrants reports whether role has a rule granting verb on resource in
// apiGroup, honoring the "*" wildcard the way Kubernetes RBAC does.
func roleGrants(role *rbacv1.Role, apiGroup, resource, verb string) bool {
	contains := func(list []string, want string) bool {
		for _, v := range list {
			if v == want || v == "*" {
				return true
			}
		}
		return false
	}
	for _, rule := range role.Rules {
		if !contains(rule.APIGroups, apiGroup) {
			continue
		}
		if !contains(rule.Resources, resource) {
			continue
		}
		if contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

// rbacCall is one (apiGroup, resource, verb) the agent's code uses.
type rbacCall struct {
	name     string
	apiGroup string
	resource string
	verb     string
}

// metalAgentSharedCalls are the Kubernetes API calls the Metal agent's own
// code makes (pkg/agent, cmd/metal-agent) that the shared, audited Role in
// deployment/macos/metal-agent-rbac.yaml grants, enumerated by grepping every
// client Get/List/Create/Update/Patch/Delete/Status().Update and the event
// recorder.
var metalAgentSharedCalls = []rbacCall{
	// InferenceServiceWatcher.poll/listExisting (watcher.go) and the
	// agent's own re-Gets (agent.go, pressure.go, refusal.go).
	{"get InferenceServices", "inference.llmkube.dev", "inferenceservices", "get"},
	{"list InferenceServices", "inference.llmkube.dev", "inferenceservices", "list"},
	// pressure.go, refusal.go, agent.go write scheduling/condition status.
	{"update InferenceServices status", "inference.llmkube.dev", "inferenceservices/status", "update"},
	// watcher.go shouldWatch, agent.go reconcileProcess fetch the
	// referenced Model.
	{"get Models", "inference.llmkube.dev", "models", "get"},
	// registry.go upsertEndpoint/checkEndpointOwnership/WithdrawOwnedEndpoints/
	// ReconcileOrphanEndpoints/RemoveRelayRegistrations, ingress_wiring.go
	// relay-adoption check.
	{"get Services", "", "services", "get"},
	{"list Services", "", "services", "list"},
	{"create Services", "", "services", "create"},
	{"update Services", "", "services", "update"},
	{"delete Services", "", "services", "delete"},
	// registry.go upsertEndpoint/checkEndpointOwnership/removeLegacySlice/
	// deleteOrphanPair.
	{"get EndpointSlices", "discovery.k8s.io", "endpointslices", "get"},
	{"create EndpointSlices", "discovery.k8s.io", "endpointslices", "create"},
	{"update EndpointSlices", "discovery.k8s.io", "endpointslices", "update"},
	{"delete EndpointSlices", "discovery.k8s.io", "endpointslices", "delete"},
	// registry.go reapLegacyEndpoints reaps the pre-EndpointSlice
	// core/v1 Endpoints object.
	{"get legacy Endpoints", "", "endpoints", "get"},
	{"delete legacy Endpoints", "", "endpoints", "delete"},
	// pressure.go emitInferenceEvent / cmd/metal-agent's EventRecorder
	// (client-go's EventSinkImpl uses Create then Patch for aggregation).
	{"create Events", "", "events", "create"},
	{"patch Events", "", "events", "patch"},
	// s3.go resolveS3Credentials/resolveHFAuth read Model.spec.sourceSecretRef;
	// ingress/tokens.go reads the fixed-name relay token Secret
	// "llmkube-metal-relay" on every relayed request in the (default)
	// non---legacy-direct-endpoints mode.
	{"get Secrets", "", "secrets", "get"},
}

// metalAgentPerMacCalls are the calls the agent makes only on its own
// objects. They are granted by the small per-Mac Role that
// `llmkube metal token create` creates, scoped by resourceNames to that one
// Mac's objects, and never by the shared Role: RBAC cannot scope a shared
// rule to "the caller's own Lease", so a shared leases rule would let every
// Mac renew every other Mac's heartbeat. deployment/macos/README.md documents
// the per-Mac Role.
var metalAgentPerMacCalls = []rbacCall{
	// metalnode_reporter.go renew: Get then Update of its own Lease, only
	// with --metal-node set.
	{"get own MetalNode Lease", "coordination.k8s.io", "leases", "get"},
	{"update own MetalNode Lease", "coordination.k8s.io", "leases", "update"},
	// tokenrenew.go renew: TokenRequest for its own ServiceAccount, only
	// with --renew-service-account-token set.
	{"renew own ServiceAccount token", "", "serviceaccounts/token", "create"},
}

// TestMetalAgentRBACCoversAgentAPICalls pins deployment/macos/metal-agent-rbac.yaml
// to metalAgentSharedCalls. It is not a substitute for the audit: a new agent
// code path that starts a genuinely new (group, resource, verb) tuple must
// add a row there as well as a rule to the manifest (or to the per-Mac Role).
// It exists to catch the manifest silently losing a rule an existing code
// path already depends on (e.g. an edit that drops a line).
func TestMetalAgentRBACCoversAgentAPICalls(t *testing.T) {
	role := loadRBACRole(t)
	for _, tt := range metalAgentSharedCalls {
		t.Run(tt.name, func(t *testing.T) {
			if !roleGrants(role, tt.apiGroup, tt.resource, tt.verb) {
				t.Errorf("metal-agent-rbac.yaml Role %q does not grant verb %q on resource %q (apiGroup %q)",
					role.Name, tt.verb, tt.resource, tt.apiGroup)
			}
		})
	}
}

// TestMetalAgentRBACGrantsOnlyAgentCalls is the other direction: every
// (group, resource, verb) the shared Role grants is one the agent uses. A
// rule added to the manifest without an agent call behind it, or a wildcard,
// fails here.
func TestMetalAgentRBACGrantsOnlyAgentCalls(t *testing.T) {
	role := loadRBACRole(t)
	used := map[string]bool{}
	for _, c := range metalAgentSharedCalls {
		used[c.apiGroup+"/"+c.resource+"/"+c.verb] = true
	}
	for _, rule := range role.Rules {
		if len(rule.ResourceNames) > 0 || len(rule.NonResourceURLs) > 0 {
			t.Errorf("shared Role rule %+v: resourceNames and nonResourceURLs do not belong in the shared Role", rule)
		}
		for _, g := range rule.APIGroups {
			for _, res := range rule.Resources {
				for _, verb := range rule.Verbs {
					if !used[g+"/"+res+"/"+verb] {
						t.Errorf("metal-agent-rbac.yaml grants verb %q on resource %q (apiGroup %q), which no agent call uses",
							verb, res, g)
					}
				}
			}
		}
	}
}

// TestMetalAgentRBACLeavesPerMacRulesOut keeps the self-scoped calls out of
// the shared Role.
func TestMetalAgentRBACLeavesPerMacRulesOut(t *testing.T) {
	role := loadRBACRole(t)
	for _, c := range metalAgentPerMacCalls {
		if roleGrants(role, c.apiGroup, c.resource, c.verb) {
			t.Errorf("metal-agent-rbac.yaml grants %q on %q (apiGroup %q) to every Mac; it belongs in the per-Mac Role (%s)",
				c.verb, c.resource, c.apiGroup, c.name)
		}
	}
}

// TestMetalAgentRBACRoleBindingMatchesRole pins the RoleBinding to the same
// Role and a ServiceAccount subject, so a rename of either name silently
// breaking the binding is caught here instead of at kubectl apply time.
func TestMetalAgentRBACRoleBindingMatchesRole(t *testing.T) {
	path := rbacManifestPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	role := loadRBACRole(t)

	var binding *rbacv1.RoleBinding
	var serviceAccountName string
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &kind); err != nil {
			t.Fatalf("parse document kind in %s: %v", path, err)
		}
		switch kind.Kind {
		case "RoleBinding":
			binding = &rbacv1.RoleBinding{}
			if err := yaml.Unmarshal([]byte(doc), binding); err != nil {
				t.Fatalf("parse RoleBinding document in %s: %v", path, err)
			}
		case "ServiceAccount":
			var sa struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(doc), &sa); err != nil {
				t.Fatalf("parse ServiceAccount document in %s: %v", path, err)
			}
			serviceAccountName = sa.Metadata.Name
		}
	}
	if binding == nil {
		t.Fatalf("%s has no RoleBinding document", path)
	}
	if serviceAccountName == "" {
		t.Fatalf("%s has no ServiceAccount document", path)
	}

	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name {
		t.Errorf("RoleBinding.roleRef = %s/%s, want Role/%s",
			binding.RoleRef.Kind, binding.RoleRef.Name, role.Name)
	}

	foundSubject := false
	for _, s := range binding.Subjects {
		if s.Kind == "ServiceAccount" && s.Name == serviceAccountName {
			foundSubject = true
		}
	}
	if !foundSubject {
		t.Errorf("RoleBinding subjects %v do not include ServiceAccount %q",
			formatSubjects(binding.Subjects), serviceAccountName)
	}
}

func formatSubjects(subjects []rbacv1.Subject) string {
	parts := make([]string, len(subjects))
	for i, s := range subjects {
		parts[i] = fmt.Sprintf("%s/%s", s.Kind, s.Name)
	}
	return strings.Join(parts, ", ")
}
