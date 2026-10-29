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

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// foreignProxyFixture describes one of the seven router-proxy objects for the
// refusal table: how to build a foreign object holding the generated name,
// how to prove it was not touched, and which provisioning steps come after
// the refused one (they must not exist: nothing is provisioned after a
// refusal).
type foreignProxyFixture struct {
	kind    string   // as getProxyObject names it
	msgKind string   // how the refusal message names the kind
	absent  []string // kinds provisioned after this step, must stay absent
	build   func(name string) client.Object
	verify  func(t *testing.T, c client.Client, ns, name string)
}

// proxyFixtureName resolves a fixture kind to its object name: every
// router-proxy object uses the base name except the admin Service, which
// appends "-admin" (identical to routerProxyAdminResourceName(mr.Name)
// because base already is routerProxyResourceName(mr.Name)).
func proxyFixtureName(kind, base string) string {
	if kind == "admin Service" {
		return base + "-admin"
	}
	return base
}

// assertForeignLabels asserts the marker labels and the absence of owner
// references shared by every foreign fixture.
func assertForeignLabels(t *testing.T, obj client.Object, name string) {
	t.Helper()
	if obj.GetLabels()["foreign"] != "true" {
		t.Errorf("foreign object %s labels = %v, want the foreign marker preserved", name, obj.GetLabels())
	}
	if len(obj.GetOwnerReferences()) != 0 {
		t.Errorf("foreign object %s was adopted: owner refs = %v", name, obj.GetOwnerReferences())
	}
}

// getTypedProxyObject fetches one router-proxy object by kind with its real
// type, so tests can inspect spec fields and owner references.
func getTypedProxyObject(t *testing.T, c client.Client, ns, kind, name string) client.Object {
	t.Helper()
	var obj client.Object
	switch kind {
	case "ConfigMap":
		obj = &corev1.ConfigMap{}
	case "Deployment":
		obj = &appsv1.Deployment{}
	case "Service", "admin Service":
		obj = &corev1.Service{}
	case "ServiceAccount":
		obj = &corev1.ServiceAccount{}
	case "Role":
		obj = &rbacv1.Role{}
	case "RoleBinding":
		obj = &rbacv1.RoleBinding{}
	default:
		t.Fatalf("unknown router-proxy kind %q", kind)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, obj); err != nil {
		t.Fatalf("get router-proxy %s %s/%s: %v", kind, ns, name, err)
	}
	return obj
}

// foreignProxyFixtures builds one foreign object per router-proxy kind, in
// provisioning order (ConfigMap, then the activation RBAC, then the
// Deployment, then the Services). Each carries distinctive content, a
// "foreign" label, and no owner references, so an adopt-or-overwrite
// regression is visible in the spec, the labels, or the owner references.
func foreignProxyFixtures() []foreignProxyFixture {
	return []foreignProxyFixture{
		{
			kind:    "ConfigMap",
			msgKind: "ConfigMap",
			absent:  []string{"Deployment", "Service", "admin Service", "ServiceAccount", "Role", "RoleBinding"},
			build:   foreignConfigMap,
			verify: func(t *testing.T, c client.Client, ns, name string) {
				cm := &corev1.ConfigMap{}
				if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, cm); err != nil {
					t.Fatalf("get foreign ConfigMap %s/%s: %v", ns, name, err)
				}
				if cm.Data[routerProxyConfigKey] != "foreign-data" {
					t.Errorf("foreign ConfigMap data was overwritten: %v", cm.Data)
				}
				assertForeignLabels(t, cm, name)
			},
		},
		{
			kind:    "ServiceAccount",
			msgKind: "ServiceAccount",
			absent:  []string{"Role", "RoleBinding", "Deployment", "Service", "admin Service"},
			build: func(name string) client.Object {
				return &corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{
						Name:        name,
						Labels:      map[string]string{"foreign": "true"},
						Annotations: map[string]string{"foreign": "yes"},
					},
				}
			},
			verify: func(t *testing.T, c client.Client, ns, name string) {
				sa := &corev1.ServiceAccount{}
				if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, sa); err != nil {
					t.Fatalf("get foreign ServiceAccount %s/%s: %v", ns, name, err)
				}
				if sa.Annotations["foreign"] != "yes" {
					t.Errorf("foreign ServiceAccount annotations = %v, want untouched", sa.Annotations)
				}
				assertForeignLabels(t, sa, name)
			},
		},
		{
			kind:    "Role",
			msgKind: "Role",
			absent:  []string{"RoleBinding", "Deployment", "Service", "admin Service"},
			build: func(name string) client.Object {
				return &rbacv1.Role{
					ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"foreign": "true"}},
					Rules: []rbacv1.PolicyRule{
						{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"}},
					},
				}
			},
			verify: func(t *testing.T, c client.Client, ns, name string) {
				role := &rbacv1.Role{}
				if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, role); err != nil {
					t.Fatalf("get foreign Role %s/%s: %v", ns, name, err)
				}
				if len(role.Rules) != 1 || len(role.Rules[0].Resources) != 1 || role.Rules[0].Resources[0] != "pods" {
					t.Errorf("foreign Role rules were overwritten: %+v", role.Rules)
				}
				assertForeignLabels(t, role, name)
			},
		},
		{
			kind:    "RoleBinding",
			msgKind: "RoleBinding",
			absent:  []string{"Deployment", "Service", "admin Service"},
			build: func(name string) client.Object {
				return &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"foreign": "true"}},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "foreign-role"},
					Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"}},
				}
			},
			verify: func(t *testing.T, c client.Client, ns, name string) {
				binding := &rbacv1.RoleBinding{}
				if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, binding); err != nil {
					t.Fatalf("get foreign RoleBinding %s/%s: %v", ns, name, err)
				}
				if binding.RoleRef.Name != "foreign-role" {
					t.Errorf("foreign RoleBinding roleRef = %+v, want untouched", binding.RoleRef)
				}
				if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "alice" {
					t.Errorf("foreign RoleBinding subjects were overwritten: %+v", binding.Subjects)
				}
				assertForeignLabels(t, binding, name)
			},
		},
		{
			kind:    "Deployment",
			msgKind: "Deployment",
			absent:  []string{"Service", "admin Service"},
			build:   foreignDeployment,
			verify:  verifyForeignDeployment,
		},
		{
			kind:    "Service",
			msgKind: "Service",
			absent:  []string{"admin Service"},
			build:   foreignService,
			verify:  verifyForeignService,
		},
		{
			kind:    "admin Service",
			msgKind: "Service",
			absent:  nil,
			build:   foreignService,
			verify:  verifyForeignService,
		},
	}
}

// foreignConfigMap builds a foreign ConfigMap holding the generated name
// with data the proxy path would overwrite.
func foreignConfigMap(name string) client.Object {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"foreign": "true"}},
		Data:       map[string]string{routerProxyConfigKey: "foreign-data"},
	}
}

// foreignDeployment builds a foreign Deployment whose selector matches the
// proxy Deployment's immutable selector, so removing the ownership gate does
// not merely error on the write: the spec is genuinely overwritten and the
// object adopted. Replicas, image, and pod labels differ from everything the
// proxy path would write.
func foreignDeployment(name string) client.Object {
	routerName := strings.TrimSuffix(name, "-router-proxy")
	selector := map[string]string{
		"app":                                name,
		"inference.llmkube.dev/model-router": routerName,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"foreign": "true"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(5),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "registry.k8s.io/pause:3.10"}},
				},
			},
		},
	}
}

func verifyForeignDeployment(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	deploy := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, deploy); err != nil {
		t.Fatalf("get foreign Deployment %s/%s: %v", ns, name, err)
	}
	if *deploy.Spec.Replicas != 5 {
		t.Errorf("foreign Deployment replicas = %d, want 5 (not overwritten)", *deploy.Spec.Replicas)
	}
	if deploy.Spec.Template.Spec.Containers[0].Image != "registry.k8s.io/pause:3.10" {
		t.Errorf("foreign Deployment image = %q, want untouched",
			deploy.Spec.Template.Spec.Containers[0].Image)
	}
	assertForeignLabels(t, deploy, name)
}

// foreignService builds a foreign Service whose type and ports differ from
// everything the proxy path would write, so an overwrite is visible.
func foreignService(name string) client.Object {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"foreign": "true"}},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: "http", Port: 9999, Protocol: corev1.ProtocolTCP}},
		},
	}
}

func verifyForeignService(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, svc); err != nil {
		t.Fatalf("get foreign Service %s/%s: %v", ns, name, err)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("foreign Service type = %q, want LoadBalancer (not overwritten)", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 9999 {
		t.Errorf("foreign Service ports = %+v, want untouched", svc.Spec.Ports)
	}
	assertForeignLabels(t, svc, name)
}

// TestModelRouterProxyRefusesForeignObject pins #2007 for each of the seven
// router-proxy objects separately: an object holding the generated name that
// this router does not control is refused with the RouterProxyNameTaken
// condition and a bounded requeue, is left byte-for-byte untouched, blocks
// every provisioning step after it, and stops blocking once deleted.
func TestModelRouterProxyRefusesForeignObject(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}

	for _, tc := range foreignProxyFixtures() {
		t.Run(tc.kind, func(t *testing.T) {
			ns := newTestNamespace(t, c)
			base := routerProxyResourceName("refuse-router")
			name := proxyFixtureName(tc.kind, base)

			foreign := tc.build(name)
			foreign.SetNamespace(ns)
			if err := c.Create(context.Background(), foreign); err != nil {
				t.Fatalf("create foreign %s %s/%s: %v", tc.kind, ns, name, err)
			}

			makePooledBackend(t, c, ns, "refuse-cuda", "refuse-pool")
			mr := pooledProxyRouter(ns, "refuse-router", "refuse-cuda", inferencev1alpha1.ModelRouterDataPlaneProxy)
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatalf("create modelrouter: %v", err)
			}

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
			})
			if err != nil {
				t.Fatalf("reconcile with foreign %s: %v (a refusal is not an error)", tc.kind, err)
			}
			if result.RequeueAfter != routerProxyNameTakenRequeueAfter {
				t.Errorf("RequeueAfter = %v, want %v (bounded requeue on the refusal)",
					result.RequeueAfter, routerProxyNameTakenRequeueAfter)
			}

			fresh := &inferencev1alpha1.ModelRouter{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
				t.Fatalf("get modelrouter: %v", err)
			}
			cond := findCondition(fresh.Status.Conditions, ConditionAvailable)
			if cond == nil {
				t.Fatalf("no Available condition after refusing %s %s/%s", tc.msgKind, ns, name)
			}
			if cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonRouterProxyNameTaken {
				t.Errorf("Available condition = {Status: %s, Reason: %s}, want False/%s",
					cond.Status, cond.Reason, ModelRouterReasonRouterProxyNameTaken)
			}
			if !strings.Contains(cond.Message, name) || !strings.Contains(cond.Message, tc.msgKind) {
				t.Errorf("refusal message %q does not name the object (%s %s/%s)", cond.Message, tc.msgKind, ns, name)
			}

			tc.verify(t, c, ns, name)

			for _, kind := range tc.absent {
				absentName := proxyFixtureName(kind, base)
				if err := getProxyObject(t, c, ns, kind, absentName); !apierrors.IsNotFound(err) {
					t.Errorf("router-proxy %s %s/%s after refusing %s: err = %v, want NotFound (nothing provisioned after a refusal)",
						kind, ns, absentName, tc.kind, err)
				}
			}

			if err := c.Delete(context.Background(), foreign); err != nil {
				t.Fatalf("delete foreign %s %s/%s: %v", tc.kind, ns, name, err)
			}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
			}); err != nil {
				t.Fatalf("reconcile after removing the foreign %s: %v", tc.kind, err)
			}
			assertProxyObjectsExist(t, c, ns, mr.Name)

			fresh = &inferencev1alpha1.ModelRouter{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
				t.Fatalf("get modelrouter after recovery: %v", err)
			}
			if cond := findCondition(fresh.Status.Conditions, ConditionAvailable); cond != nil && cond.Reason == ModelRouterReasonRouterProxyNameTaken {
				t.Errorf("RouterProxyNameTaken survives recovery: %s", cond.Message)
			}
		})
	}
}

// TestEnsureRouterProxyOwnedOwnerMessage pins the refusal message branches:
// a controller reference from another ModelRouter is named as the foreign
// owner, while a same-Kind controller from a different API group is not
// misreported as a ModelRouter.
func TestEnsureRouterProxyOwnedOwnerMessage(t *testing.T) {
	testScheme := builderTestScheme(t)
	mr := &inferencev1alpha1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns", UID: "uid-1"}}

	other := &inferencev1alpha1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "r2", UID: "uid-2"}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"}}
	if err := setControllerReferenceUnblocked(other, cm, testScheme); err != nil {
		t.Fatalf("set the other router's controller reference: %v", err)
	}
	err := ensureRouterProxyOwned(mr, cm, "ConfigMap")
	if err == nil {
		t.Fatal("ensureRouterProxyOwned returned nil, want a name-taken refusal")
	}
	if !strings.Contains(err.Error(), "another ModelRouter") || !strings.Contains(err.Error(), "uid-2") {
		t.Errorf("refusal message %q does not name the foreign ModelRouter owner (uid-2)", err)
	}

	impostor := true
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "other.example.com/v1", Kind: "ModelRouter",
			Name: "impostor", UID: "uid-9", Controller: &impostor,
		}}}}
	err = ensureRouterProxyOwned(mr, foreign, "ConfigMap")
	if err == nil {
		t.Fatal("ensureRouterProxyOwned returned nil, want a name-taken refusal")
	}
	if strings.Contains(err.Error(), "another ModelRouter") {
		t.Errorf("same-Kind controller from another API group misreported as a ModelRouter: %s", err)
	}
}

// TestModelRouterProxyRefusesSquattedNameAfterProvisioning pins the drift
// case the per-object table cannot reach: the router provisioned its full
// surface, then one generated name is squatted underneath the running proxy.
// The refusal stops the sequence, the router's own objects keep running and
// stay owned by the router (a refusal is a stop, not a rollback), and
// recovery restores the squatted object once the squatter is gone.
func TestModelRouterProxyRefusesSquattedNameAfterProvisioning(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}

	makePooledBackend(t, c, ns, "drift-cuda", "drift-pool")
	mr := pooledProxyRouter(ns, "drift-router", "drift-cuda", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	assertProxyObjectsExist(t, c, ns, mr.Name)

	// Squat the ConfigMap name the way an operator hit would: delete ours,
	// create someone else's under the same name.
	base := routerProxyResourceName(mr.Name)
	ours := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: base, Namespace: ns}, ours); err != nil {
		t.Fatalf("get our ConfigMap: %v", err)
	}
	if err := c.Delete(context.Background(), ours); err != nil {
		t.Fatalf("delete our ConfigMap: %v", err)
	}
	foreign := foreignConfigMap(base)
	foreign.SetNamespace(ns)
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create squatter ConfigMap: %v", err)
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile with squatted ConfigMap: %v (a refusal is not an error)", err)
	}
	if result.RequeueAfter != routerProxyNameTakenRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v (bounded requeue on the refusal)",
			result.RequeueAfter, routerProxyNameTakenRequeueAfter)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := findCondition(fresh.Status.Conditions, ConditionAvailable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonRouterProxyNameTaken {
		t.Errorf("Available condition = %+v, want False/%s", cond, ModelRouterReasonRouterProxyNameTaken)
	} else if !strings.Contains(cond.Message, base) {
		t.Errorf("refusal message %q does not name the squatted ConfigMap", cond.Message)
	}

	// Stop, not rollback: the Deployment keeps running and stays ours, and
	// the rest of the surface is untouched.
	deploy := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: base, Namespace: ns}, deploy); err != nil {
		t.Fatalf("the squatted-name refusal must not tear down the running Deployment: %v", err)
	}
	refs := deploy.OwnerReferences
	if len(refs) != 1 || refs[0].UID != fresh.UID {
		t.Errorf("Deployment owner refs = %v, want exactly this router's controller ref", refs)
	}
	for _, kind := range []string{"Service", "admin Service", "ServiceAccount", "Role", "RoleBinding"} {
		obj := getTypedProxyObject(t, c, ns, kind, proxyFixtureName(kind, base))
		refs := obj.GetOwnerReferences()
		if len(refs) != 1 || refs[0].UID != fresh.UID {
			t.Errorf("router-proxy %s owner refs = %v, want exactly this router's controller ref", kind, refs)
		}
	}
	for _, f := range foreignProxyFixtures() {
		if f.kind != "ConfigMap" {
			continue
		}
		f.verify(t, c, ns, base)
	}

	// Recovery: once the squatter is gone, the ConfigMap is ours again.
	if err := c.Delete(context.Background(), foreign); err != nil {
		t.Fatalf("delete squatter ConfigMap: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile after removing the squatter: %v", err)
	}
	assertProxyObjectsExist(t, c, ns, mr.Name)
	cm := getTypedProxyObject(t, c, ns, "ConfigMap", base)
	refs = cm.GetOwnerReferences()
	if len(refs) != 1 || refs[0].UID != fresh.UID {
		t.Errorf("restored ConfigMap owner refs = %v, want exactly this router's controller ref", refs)
	}
	fresh = &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter after recovery: %v", err)
	}
	if cond := findCondition(fresh.Status.Conditions, ConditionAvailable); cond != nil && cond.Reason == ModelRouterReasonRouterProxyNameTaken {
		t.Errorf("RouterProxyNameTaken survives recovery: %s", cond.Message)
	}
}

// TestModelRouterProxyRefusesObjectOwnedByOtherRouter pins the second refusal
// shape: the generated name is held by an object controlled by a different
// ModelRouter. The refusal must name the foreign owner, and the other
// router's object must survive untouched.
func TestModelRouterProxyRefusesObjectOwnedByOtherRouter(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
	base := routerProxyResourceName("owned-router")

	other := externalProxyRouter(ns, "other-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), other); err != nil {
		t.Fatalf("create the owning modelrouter: %v", err)
	}
	// Re-fetch so the controller reference carries the server-assigned UID.
	if err := c.Get(context.Background(), types.NamespacedName{Name: other.Name, Namespace: ns}, other); err != nil {
		t.Fatalf("get the owning modelrouter: %v", err)
	}

	foreign := foreignDeployment(base)
	foreign.SetNamespace(ns)
	if err := setControllerReferenceUnblocked(other, foreign, scheme.Scheme); err != nil {
		t.Fatalf("set the other router's controller reference: %v", err)
	}
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create the other router's Deployment: %v", err)
	}

	makePooledBackend(t, c, ns, "owned-cuda", "owned-pool")
	mr := pooledProxyRouter(ns, "owned-router", "owned-cuda", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile against the other router's Deployment: %v", err)
	}
	if result.RequeueAfter != routerProxyNameTakenRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v (bounded requeue on the refusal)",
			result.RequeueAfter, routerProxyNameTakenRequeueAfter)
	}

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	cond := findCondition(fresh.Status.Conditions, ConditionAvailable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ModelRouterReasonRouterProxyNameTaken {
		t.Errorf("Available condition = %+v, want False/%s", cond, ModelRouterReasonRouterProxyNameTaken)
	} else if !strings.Contains(cond.Message, "another ModelRouter") {
		t.Errorf("refusal message %q does not name the foreign owner", cond.Message)
	}

	deploy := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: base, Namespace: ns}, deploy); err != nil {
		t.Fatalf("get the other router's Deployment: %v", err)
	}
	if *deploy.Spec.Replicas != 5 {
		t.Errorf("the other router's Deployment replicas = %d, want 5 (not overwritten)", *deploy.Spec.Replicas)
	}
	if len(deploy.OwnerReferences) != 1 || deploy.OwnerReferences[0].UID != other.UID {
		t.Errorf("the other router's Deployment owner refs = %v, want exactly its own controller ref", deploy.OwnerReferences)
	}

	for _, kind := range []string{"Service", "admin Service"} {
		if err := getProxyObject(t, c, ns, kind, proxyFixtureName(kind, base)); !apierrors.IsNotFound(err) {
			t.Errorf("router-proxy %s after refusing the Deployment: err = %v, want NotFound", kind, err)
		}
	}
}

// TestModelRouterProxyOwnObjectsStillUpdate pins the upgrade-compatibility
// half of #2007: objects a router created itself (the way every operator
// version created them, through setControllerReferenceUnblocked) carry its
// controller reference, so the ownership gate lets them keep updating.
func TestModelRouterProxyOwnObjectsStillUpdate(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}

	makePooledBackend(t, c, ns, "own-cuda", "own-pool")
	mr := pooledProxyRouter(ns, "own-router", "own-cuda", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	assertProxyObjectsExist(t, c, ns, mr.Name)

	// A spec change must still flow through to the router's own objects.
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	mr.UID = fresh.UID
	fresh.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: 9100}
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("set endpoint port: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: routerProxyResourceName(mr.Name), Namespace: ns}, svc); err != nil {
		t.Fatalf("get data-plane Service: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 9100 {
		t.Errorf("data-plane Service ports = %+v, want port 9100 (own objects must keep updating)", svc.Spec.Ports)
	}

	for _, check := range proxyObjectChecks(mr.Name) {
		obj := getTypedProxyObject(t, c, ns, check.kind, check.name)
		refs := obj.GetOwnerReferences()
		if len(refs) != 1 || refs[0].Kind != "ModelRouter" || refs[0].UID != mr.UID ||
			refs[0].Controller == nil || !*refs[0].Controller {
			t.Errorf("router-proxy %s %s/%s owner refs = %v, want exactly one controller ref to %s",
				check.kind, ns, check.name, refs, mr.Name)
		}
	}

	fresh = &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter after update: %v", err)
	}
	if cond := findCondition(fresh.Status.Conditions, ConditionAvailable); cond != nil && cond.Reason == ModelRouterReasonRouterProxyNameTaken {
		t.Errorf("own objects were refused: %s", cond.Message)
	}
}
