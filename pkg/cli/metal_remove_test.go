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
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestMetalRemoveLeavesObjectsNotLabeledForTheMac: remove deletes by
// derived name only what is labeled for this Mac. Anything else (another
// app's Lease, another Mac's binding) is left and reported by name.
func TestMetalRemoveLeavesObjectsNotLabeledForTheMac(t *testing.T) {
	for _, fo := range foreignObjects() {
		for _, lab := range foreignLabelings {
			t.Run(fo.kind+"/"+lab.name, func(t *testing.T) {
				_, c, ns := metalEnvtest(t)
				foreign := fo.make(ns, lab.labels)
				if err := c.Create(context.Background(), foreign); err != nil {
					t.Fatal(err)
				}
				stdout, _, err := runMetal(t, "remove", "mac-a", "-n", ns)
				assertUntouched(t, c, foreign)
				if !strings.Contains(stdout, "skipped") || !strings.Contains(stdout, foreign.GetName()) {
					t.Errorf("remove output does not report the skipped %s %s: %q", fo.kind, foreign.GetName(), stdout)
				}
				// Leaving the ServiceAccount means nothing was revoked: that
				// must not look like success.
				if fo.kind == "ServiceAccount" && (err == nil || !strings.Contains(err.Error(), "NOT revoked")) {
					t.Errorf("remove with a foreign ServiceAccount: err = %v, want a not-revoked error", err)
				}
				if fo.kind != "ServiceAccount" && err != nil {
					t.Errorf("remove: %v", err)
				}
			})
		}
	}
}

// handMadeMac applies Mac "mac-a" by hand the way the guide's "Creating the
// objects" section did before token create existed: same names, no labels.
func handMadeMac(t *testing.T, c client.Client, ns string) []client.Object {
	t.Helper()
	n := newMetalNames("mac-a")
	om := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	sub := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: n.ServiceAccount, Namespace: ns}}
	objs := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: om(n.ServiceAccount)},
		&rbacv1.Role{ObjectMeta: om(n.Role), Rules: metalPerMacRules(n)},
		&rbacv1.RoleBinding{ObjectMeta: om(n.RoleBinding), Subjects: sub,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: n.Role}},
		&rbacv1.RoleBinding{ObjectMeta: om(n.SharedBinding), Subjects: sub,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: metalSharedRoleName}},
		&inferencev1alpha1.MetalNode{ObjectMeta: om(n.MetalNode),
			Spec: inferencev1alpha1.MetalNodeSpec{ServiceAccountName: n.ServiceAccount, LeaseName: n.Lease}},
	}
	for _, o := range objs {
		if err := c.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	return objs
}

func TestMetalRemoveLeavesAHandAppliedMetalNode(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	if _, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns); err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	// The MetalNode was applied by hand before: token create adopted it, but
	// strip the labels again to get the hand-applied shape.
	mn := &inferencev1alpha1.MetalNode{}
	key := types.NamespacedName{Namespace: ns, Name: "mac-a"}
	if err := c.Get(context.Background(), key, mn); err != nil {
		t.Fatal(err)
	}
	mn.Labels = nil
	if err := c.Update(context.Background(), mn); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runMetal(t, "remove", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := c.Get(context.Background(), key, &inferencev1alpha1.MetalNode{}); err != nil {
		t.Errorf("remove deleted the hand-applied MetalNode: %v", err)
	}
	if !strings.Contains(stdout, "skipped MetalNode "+ns+"/mac-a") {
		t.Errorf("remove does not say it left the MetalNode: %q", stdout)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a"},
		&corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("the labeled ServiceAccount survived remove: %v", err)
	}
}

func TestMetalRemoveIncludeUnlabeled(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	objs := handMadeMac(t, c, ns)
	otherMacs := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "llmkube-metal-mac-a-join",
		Labels: map[string]string{inferencev1alpha1.LabelMetalNode: "mac-b"}}}
	if err := c.Create(context.Background(), otherMacs); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runMetal(t, "remove", "mac-a", "-n", ns, "--include-unlabeled")
	if err != nil {
		t.Fatalf("remove --include-unlabeled: %v\n%s", err, stderr)
	}
	for _, o := range objs {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(o), o); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s survived remove --include-unlabeled (get: %v)", o, o.GetName(), err)
		}
		if !strings.Contains(stderr, o.GetName()) || !strings.Contains(stderr, "unlabeled") {
			t.Errorf("no warning for deleting unlabeled %T %s: %q", o, o.GetName(), stderr)
		}
	}
	// Labeled for another Mac is never this Mac's, flag or not.
	assertUntouched(t, c, otherMacs)
	if !strings.Contains(stdout, "skipped Secret "+ns+"/llmkube-metal-mac-a-join") {
		t.Errorf("remove does not report the other Mac's Secret: %q", stdout)
	}
}

// TestMetalRemoveWaitsForTheServiceAccount: nothing else is deleted while the
// ServiceAccount still exists (here held by a finalizer), because deleting it
// is the revocation.
func TestMetalRemoveWaitsForTheServiceAccount(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	ctx := context.Background()
	if _, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns); err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	saKey := types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a"}
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, saKey, sa); err != nil {
		t.Fatal(err)
	}
	sa.Finalizers = []string{"test.llmkube.dev/hold"}
	if err := c.Update(ctx, sa); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- metalRemove(ctx, c, ns, "mac-a", false, &bytes.Buffer{}, &bytes.Buffer{}) }()

	// The ServiceAccount is now terminating; give remove time to move on if
	// it did not wait.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a"}, &rbacv1.Role{}); err != nil {
			t.Fatalf("the per-Mac Role went while the ServiceAccount still existed (get: %v)", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := c.Get(ctx, saKey, sa); err != nil || sa.DeletionTimestamp == nil {
		t.Fatalf("ServiceAccount should be terminating: %v %v", err, sa.DeletionTimestamp)
	}
	sa.Finalizers = nil
	if err := c.Update(ctx, sa); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("remove: %v", err)
	}
	roleKey := types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a"}
	if err := c.Get(ctx, roleKey, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Errorf("per-Mac Role survived remove: %v", err)
	}
}
