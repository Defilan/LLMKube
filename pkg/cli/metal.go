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

package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/deployment/macos"
)

const (
	// metalSharedRoleName is the audited agent Role in
	// deployment/macos/metal-agent-rbac.yaml, shared by every Mac in a
	// namespace.
	metalSharedRoleName = "llmkube-metal-agent"
	// metalNamePrefix starts the names of a Mac's ServiceAccount, Role,
	// RoleBindings and join Secret.
	metalNamePrefix = "llmkube-metal-"
	// metalSharedBindingSuffix ends the name of the Mac's RoleBinding to the
	// shared Role.
	metalSharedBindingSuffix = "-shared"
	// metalManagedBy is the app.kubernetes.io/managed-by value on everything
	// `llmkube metal token create` creates.
	metalManagedBy = "llmkube-cli"
	labelManagedBy = "app.kubernetes.io/managed-by"
	// metalLeaseDurationSeconds matches the duration the agent writes on
	// every renewal (pkg/agent metalNodeLeaseDurationSeconds).
	metalLeaseDurationSeconds = int32(45)
)

// metalNames are the objects that make up one Mac in a namespace, all
// derived from the Mac's name.
type metalNames struct {
	Mac            string
	ServiceAccount string
	Role           string
	RoleBinding    string
	SharedBinding  string
	Lease          string
	MetalNode      string
	JoinSecret     string
}

func newMetalNames(mac string) metalNames {
	base := metalNamePrefix + mac
	return metalNames{
		Mac:            mac,
		ServiceAccount: base,
		Role:           base,
		RoleBinding:    base,
		SharedBinding:  base + metalSharedBindingSuffix,
		Lease:          mac,
		MetalNode:      mac,
		JoinSecret:     base + "-join",
	}
}

// validateMacName checks <mac-name> before any API call. It is a DNS-1123
// label (it is also a label value), and it must not derive names that
// belong to something else: "agent" would make the Mac's Role and
// RoleBinding the shared llmkube-metal-agent ones, and "x-shared" would make
// them Mac x's shared RoleBinding.
func validateMacName(mac string) error {
	if errs := validation.IsDNS1123Label(mac); len(errs) > 0 {
		return fmt.Errorf("invalid Mac name %q: %s", mac, strings.Join(errs, "; "))
	}
	if metalNamePrefix+mac == metalSharedRoleName {
		return fmt.Errorf("invalid Mac name %q: it would reuse the shared %s Role's name", mac, metalSharedRoleName)
	}
	if strings.HasSuffix(mac, metalSharedBindingSuffix) {
		return fmt.Errorf("invalid Mac name %q: a name ending in %q collides with another Mac's shared RoleBinding",
			mac, metalSharedBindingSuffix)
	}
	return nil
}

func metalLabels(mac string) map[string]string {
	return map[string]string{
		labelManagedBy:                   metalManagedBy,
		inferencev1alpha1.LabelMetalNode: mac,
	}
}

// metalPerMacRules is the per-Mac Role: only the Mac's own objects, all by
// resourceNames. Minting its own token (self-renewal), reading its
// MetalNode, renewing its Lease, and deleting its join Secret once (which
// kills the join token). The audited shared Role carries everything else.
func metalPerMacRules(n metalNames) []rbacv1.PolicyRule {
	rule := func(group, resource string, verbs []string, name string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{
			APIGroups:     []string{group},
			Resources:     []string{resource},
			Verbs:         verbs,
			ResourceNames: []string{name},
		}
	}
	return []rbacv1.PolicyRule{
		rule("", "serviceaccounts/token", []string{"create"}, n.ServiceAccount),
		rule(inferencev1alpha1.GroupVersion.Group, "metalnodes", []string{"get"}, n.MetalNode),
		rule("coordination.k8s.io", "leases", []string{"get", "update"}, n.Lease),
		rule("", "secrets", []string{"delete"}, n.JoinSecret),
	}
}

// sharedMetalAgentRole returns the Role document of the embedded
// deployment/macos/metal-agent-rbac.yaml.
func sharedMetalAgentRole() (*rbacv1.Role, error) {
	for _, doc := range strings.Split(string(macos.MetalAgentRBAC), "\n---") {
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			return nil, fmt.Errorf("parse embedded metal-agent-rbac.yaml: %w", err)
		}
		if probe.Kind != "Role" {
			continue
		}
		role := &rbacv1.Role{}
		if err := yaml.UnmarshalStrict([]byte(doc), role); err != nil {
			return nil, fmt.Errorf("parse embedded metal-agent-rbac.yaml Role: %w", err)
		}
		if role.Name != metalSharedRoleName {
			return nil, fmt.Errorf("embedded metal-agent-rbac.yaml Role is %q, want %q", role.Name, metalSharedRoleName)
		}
		return role, nil
	}
	return nil, errors.New("embedded metal-agent-rbac.yaml has no Role")
}

func metalScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// errNotMetalManaged refuses to take over an object with a derived name that
// was not created for this Mac.
func errNotMetalManaged(kind string, obj client.Object, mac string) error {
	if other := obj.GetLabels()[inferencev1alpha1.LabelMetalNode]; other != "" {
		return fmt.Errorf("%s %s/%s already exists and belongs to Mac %q (labeled %s=%s), not %q; "+
			"llmkube will not take it over", kind, obj.GetNamespace(), obj.GetName(), other,
			inferencev1alpha1.LabelMetalNode, other, mac)
	}
	return fmt.Errorf("%s %s/%s already exists and is not labeled %s=%s, so llmkube will not take it over; "+
		"delete it, or label it if it really is this Mac's (kubectl label -n %s %s %s %s=%s)",
		kind, obj.GetNamespace(), obj.GetName(), inferencev1alpha1.LabelMetalNode, mac,
		obj.GetNamespace(), strings.ToLower(kind), obj.GetName(), inferencev1alpha1.LabelMetalNode, mac)
}

// NewMetalCommand is `llmkube metal`: joining Macs to the cluster as Metal
// inference nodes.
func NewMetalCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metal",
		Short: "Join Macs to the cluster as Metal inference nodes",
		Long: `Manage the Macs that run the LLMKube Metal agent.

An admin creates a short-lived, single-use join string for a Mac with
'llmkube metal token create <mac>'. 'llmkube metal join <string>' on the Mac
uses it to set up the agent. 'llmkube metal remove <mac>' revokes the Mac.`,
	}
	token := &cobra.Command{
		Use:   "token",
		Short: "Create join strings for Macs",
	}
	token.AddCommand(newMetalTokenCreateCommand())
	cmd.AddCommand(token)
	cmd.AddCommand(newMetalRemoveCommand())
	return cmd
}
