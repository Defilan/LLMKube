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
	"bytes"
	"context"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// foreignObject is an object with one of Mac "mac-a"'s derived names that
// is not mac-a's: unlabeled, or labeled for another Mac.
type foreignObject struct {
	kind string
	make func(ns string, labels map[string]string) client.Object
}

func foreignObjects() []foreignObject {
	meta := func(ns, name string, labels map[string]string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}
	}
	someoneElse := func(ns string) []rbacv1.Subject {
		return []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "someone-else", Namespace: ns}}
	}
	return []foreignObject{
		{"ServiceAccount", func(ns string, l map[string]string) client.Object {
			return &corev1.ServiceAccount{ObjectMeta: meta(ns, "llmkube-metal-mac-a", l)}
		}},
		{"Role", func(ns string, l map[string]string) client.Object {
			return &rbacv1.Role{ObjectMeta: meta(ns, "llmkube-metal-mac-a", l), Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"},
			}}}
		}},
		{"RoleBinding", func(ns string, l map[string]string) client.Object {
			return &rbacv1.RoleBinding{
				ObjectMeta: meta(ns, "llmkube-metal-mac-a", l),
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
				Subjects:   someoneElse(ns),
			}
		}},
		{"shared RoleBinding", func(ns string, l map[string]string) client.Object {
			return &rbacv1.RoleBinding{
				ObjectMeta: meta(ns, "llmkube-metal-mac-a-shared", l),
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
				Subjects:   someoneElse(ns),
			}
		}},
		{"Lease", func(ns string, l map[string]string) client.Object {
			holder := "someone-else"
			return &coordinationv1.Lease{
				ObjectMeta: meta(ns, "mac-a", l),
				Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
			}
		}},
		{"Secret", func(ns string, l map[string]string) client.Object {
			return &corev1.Secret{ObjectMeta: meta(ns, "llmkube-metal-mac-a-join", l), Data: map[string][]byte{"k": []byte("v")}}
		}},
	}
}

var foreignLabelings = []struct {
	name   string
	labels map[string]string
}{
	{"unlabeled", nil},
	{"other-mac", map[string]string{inferencev1alpha1.LabelMetalNode: "mac-b"}},
}

// assertUntouched fails unless obj still exists exactly as created.
func assertUntouched(t *testing.T, c client.Client, created client.Object) {
	t.Helper()
	got, ok := created.DeepCopyObject().(client.Object)
	if !ok {
		t.Fatal("copy object")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(created), got); err != nil {
		t.Fatalf("foreign %T %s: %v", created, created.GetName(), err)
	}
	if got.GetUID() != created.GetUID() || got.GetResourceVersion() != created.GetResourceVersion() {
		t.Errorf("foreign %T %s was changed (uid %s->%s, rv %s->%s)", created, created.GetName(),
			created.GetUID(), got.GetUID(), created.GetResourceVersion(), got.GetResourceVersion())
	}
}

// TestMetalTokenCreateRefusesForeignObjectsBeforeWriting: any derived-name
// object that is not this Mac's makes token create refuse before its first
// write, so the cluster is left as it was.
func TestMetalTokenCreateRefusesForeignObjectsBeforeWriting(t *testing.T) {
	for _, fo := range foreignObjects() {
		for _, lab := range foreignLabelings {
			t.Run(fo.kind+"/"+lab.name, func(t *testing.T) {
				_, c, ns := metalEnvtest(t)
				ctx := context.Background()
				foreign := fo.make(ns, lab.labels)
				if err := c.Create(ctx, foreign); err != nil {
					t.Fatal(err)
				}
				_, _, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
				if err == nil || !strings.Contains(err.Error(), foreign.GetName()) {
					t.Fatalf("token create: err = %v, want a refusal naming %s", err, foreign.GetName())
				}
				assertUntouched(t, c, foreign)
				assertNothingWritten(t, c, ns, foreign)
			})
		}
	}
}

// assertNothingWritten: no object of the Mac's exists besides the foreign
// one, and the shared Role was not created either.
func assertNothingWritten(t *testing.T, c client.Client, ns string, foreign client.Object) {
	t.Helper()
	ctx := context.Background()
	n := newMetalNames("mac-a")
	for _, o := range []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.ServiceAccount}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.Role}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.RoleBinding}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.SharedBinding}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.Lease}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.JoinSecret}},
		&inferencev1alpha1.MetalNode{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n.MetalNode}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: metalSharedRoleName}},
	} {
		if sameKindAndName(o, foreign) {
			continue
		}
		err := c.Get(ctx, client.ObjectKeyFromObject(o), o)
		if !apierrors.IsNotFound(err) {
			t.Errorf("a refused token create left %T %s behind (get: %v)", o, o.GetName(), err)
		}
	}
}

func sameKindAndName(a, b client.Object) bool {
	return a.GetName() == b.GetName() && strings.EqualFold(typeName(a), typeName(b))
}

func typeName(o client.Object) string {
	switch o.(type) {
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	case *corev1.Secret:
		return "Secret"
	case *rbacv1.Role:
		return "Role"
	case *rbacv1.RoleBinding:
		return "RoleBinding"
	case *coordinationv1.Lease:
		return "Lease"
	case *inferencev1alpha1.MetalNode:
		return "MetalNode"
	}
	return ""
}

// TestMetalStepGuardsRefuseForeignObjects drives each create step on its own,
// past the preflight, so the per-step guards (which close the window between
// the preflight and the write) are tested too.
func TestMetalStepGuardsRefuseForeignObjects(t *testing.T) {
	steps := map[string]func(context.Context, metalApply) error{
		"ServiceAccount":     ensureMetalServiceAccount,
		"Role":               ensureMetalRole,
		"RoleBinding":        ensureMetalRoleBindings,
		"shared RoleBinding": ensureMetalRoleBindings,
		"Lease":              ensureMetalLease,
		"Secret": func(ctx context.Context, a metalApply) error {
			_, err := recreateJoinSecret(ctx, a)
			return err
		},
	}
	for _, fo := range foreignObjects() {
		for _, lab := range foreignLabelings {
			t.Run(fo.kind+"/"+lab.name, func(t *testing.T) {
				_, c, ns := metalEnvtest(t)
				ctx := context.Background()
				foreign := fo.make(ns, lab.labels)
				if err := c.Create(ctx, foreign); err != nil {
					t.Fatal(err)
				}
				a := metalApply{c: c, namespace: ns, names: newMetalNames("mac-a"), log: &bytes.Buffer{}}
				err := steps[fo.kind](ctx, a)
				if err == nil || !strings.Contains(err.Error(), inferencev1alpha1.LabelMetalNode) {
					t.Fatalf("%s step: err = %v, want an ownership refusal", fo.kind, err)
				}
				assertUntouched(t, c, foreign)
			})
		}
	}
}

// TestMetalTokenCreateRefusesAnInvalidJoinStringBeforeWriting: a value the
// TLS check accepts but the join string does not (certificate host names
// match without regard to case, a join string's tlsServerName must be a
// lower-case DNS name) is refused before the first write. Found only by
// Encode at the end, it used to leave the Mac's objects and a live join
// token behind, and kill an earlier unused string.
func TestMetalTokenCreateRefusesAnInvalidJoinStringBeforeWriting(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	_, _, err := runMetal(t, "token", "create", "mac-a", "-n", ns, "--tls-server-name", "LOCALHOST")
	if err == nil || !strings.Contains(err.Error(), "tlsServerName") {
		t.Fatalf("token create: err = %v, want a refusal naming tlsServerName", err)
	}
	assertNothingWritten(t, c, ns, &corev1.ConfigMap{})
}
