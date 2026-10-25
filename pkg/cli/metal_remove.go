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
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newMetalRemoveCommand() *cobra.Command {
	var namespace string
	cmd := &cobra.Command{
		Use:   "remove <mac-name>",
		Short: "Revoke a Mac and delete its objects",
		Long: `Revoke a Mac's access to the cluster and delete what 'llmkube metal token create'
made for it.

The Mac's ServiceAccount goes first, and the command waits until it is gone:
that revokes every token the Mac holds, including ones it minted for itself.
A token used in the last ~10 seconds can keep working for up to ~10 seconds
more (the API server caches successful authentications). Then the per-Mac
Role, both RoleBindings, the Lease, the join Secret and the MetalNode are
deleted. The shared llmkube-metal-agent Role stays. Objects that are already
gone are skipped, so running it twice is fine.

This does not stop the agent on the Mac; run 'llmkube metal leave' there.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMetalRemove(cmd, args[0], namespace)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace of the Mac's objects (default: the kubeconfig context's namespace, else default)")
	return cmd
}

func runMetalRemove(cmd *cobra.Command, mac, namespaceFlag string) error {
	if err := validateMacName(mac); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, namespace, err := loadMetalAdminConfig(namespaceFlag)
	if err != nil {
		return err
	}
	scheme, err := metalScheme()
	if err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	return metalRemove(ctx, c, namespace, mac, cmd.OutOrStdout())
}

// metalRemove deletes the Mac's objects, ServiceAccount first. It never
// deletes the shared Role.
func metalRemove(ctx context.Context, c client.Client, namespace, mac string, out io.Writer) error {
	n := newMetalNames(mac)
	objMeta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: namespace, Name: name} }

	removed := 0
	sa := &corev1.ServiceAccount{ObjectMeta: objMeta(n.ServiceAccount)}
	gone, err := deleteMetalObject(ctx, c, "ServiceAccount", sa)
	if err != nil {
		return err
	}
	if gone {
		if err := waitGone(ctx, c, sa); err != nil {
			return err
		}
		removed++
		_, _ = fmt.Fprintf(out, "removed ServiceAccount %s/%s: the Mac's tokens are revoked "+
			"(one used in the last ~10 s may work ~10 s longer)\n", namespace, n.ServiceAccount)
	}

	rest := []struct {
		kind string
		obj  client.Object
	}{
		{"Role", &rbacv1.Role{ObjectMeta: objMeta(n.Role)}},
		{"RoleBinding", &rbacv1.RoleBinding{ObjectMeta: objMeta(n.RoleBinding)}},
		{"RoleBinding", &rbacv1.RoleBinding{ObjectMeta: objMeta(n.SharedBinding)}},
		{"Lease", &coordinationv1.Lease{ObjectMeta: objMeta(n.Lease)}},
		{"Secret", &corev1.Secret{ObjectMeta: objMeta(n.JoinSecret)}},
		{"MetalNode", &inferencev1alpha1.MetalNode{ObjectMeta: objMeta(n.MetalNode)}},
	}
	for _, r := range rest {
		gone, err := deleteMetalObject(ctx, c, r.kind, r.obj)
		if err != nil {
			return err
		}
		if gone {
			removed++
			_, _ = fmt.Fprintf(out, "removed %s %s/%s\n", r.kind, namespace, r.obj.GetName())
		}
	}
	if removed == 0 {
		_, _ = fmt.Fprintf(out, "nothing to remove for Mac %s in namespace %s\n", mac, namespace)
	}
	return nil
}

// deleteMetalObject deletes obj and reports whether it existed. A missing
// object, or a missing MetalNode resource, is not an error.
func deleteMetalObject(ctx context.Context, c client.Client, kind string, obj client.Object) (bool, error) {
	err := c.Delete(ctx, obj)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	}
	return false, fmt.Errorf("delete %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
}
