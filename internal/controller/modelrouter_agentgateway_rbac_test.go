package controller

import (
	"os"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// TestModelRouterAgentGateway_RBACCoversClientVerbs parses the generated
// ClusterRole and asserts that every resource/verb pair the AgentGateway
// reconciler exercises is granted. envtest runs as cluster-admin, so it cannot
// catch a missing +kubebuilder:rbac marker; this test is the cheap half of
// that guard (make test-e2e-rbac is the live half). Removing a marker and
// re-running make manifests must fail this test.
func TestModelRouterAgentGateway_RBACCoversClientVerbs(t *testing.T) {
	data, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatalf("read config/rbac/role.yaml: %v", err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, role); err != nil {
		t.Fatalf("parse config/rbac/role.yaml: %v", err)
	}

	granted := map[string]map[string]bool{}
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, res := range rule.Resources {
				key := group + "/" + res
				if granted[key] == nil {
					granted[key] = map[string]bool{}
				}
				for _, v := range rule.Verbs {
					granted[key][v] = true
				}
			}
		}
	}

	// Every client call the AgentGateway reconciler makes, grouped by
	// resource: the guard chain Gets the referenced Gateway and, for
	// allowedRoutes Selector matching, the router's namespace; backend
	// resolution Gets InferenceServices; the apply creates/updates pools, the
	// extraction policy, the HTTPRoute, and (routing: Model) the
	// AgentgatewayModels; the conflict detector Lists policies; the deletion,
	// leaving, consent-withdrawal, and mode-switch sweeps and the stale-model
	// prune List and Delete policies, pools, routes, and models; the pre-flight name check Gets each
	// generated object; status and finalizer writes update and patch the
	// router; the Recorder emits events.
	//
	// The list/watch pins on inferenceservices and namespaces are not direct
	// client calls: this reconciler Watches InferenceServices and reads the
	// router's namespace through the manager's shared cache, whose informers
	// List and Watch. Those verbs come from markers elsewhere in the package
	// (the ClusterRole is the union), so the pin holds the union honest -
	// envtest runs as cluster-admin and cannot catch the informer's List
	// being forbidden; only make test-e2e-rbac exercises it live.
	required := []struct {
		key   string
		verbs []string
	}{
		{"inference.llmkube.dev/modelrouters", []string{"get", "update", "patch"}},
		{"inference.llmkube.dev/modelrouters/status", []string{"get", "patch"}},
		{"inference.llmkube.dev/inferenceservices", []string{"get", "list", "watch"}},
		{"inference.networking.k8s.io/inferencepools", []string{"get", "list", "create", "update", "patch", "delete"}},
		{"gateway.networking.k8s.io/httproutes", []string{"get", "list", "create", "update", "patch", "delete"}},
		{"agentgateway.dev/agentgatewaypolicies", []string{"get", "list", "create", "update", "patch", "delete"}},
		{"agentgateway.dev/agentgatewaymodels", []string{"get", "list", "create", "update", "patch", "delete"}},
		{"gateway.networking.k8s.io/gateways", []string{"get"}},
		{"/namespaces", []string{"get", "list", "watch"}},
		{"/events", []string{"create", "patch"}},
		{"events.k8s.io/events", []string{"create", "patch"}},
	}
	for _, tc := range required {
		for _, verb := range tc.verbs {
			if !granted[tc.key][verb] {
				t.Errorf("generated ClusterRole does not grant %q on %s; add or restore the "+
					"+kubebuilder:rbac marker and re-run make manifests", verb, tc.key)
			}
		}
	}
}
