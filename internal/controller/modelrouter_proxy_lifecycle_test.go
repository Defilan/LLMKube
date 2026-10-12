/*
Copyright 2025.

Licensed under the Apache License, Version 2.0
*/

package controller

import (
	"context"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// proxyObjectChecks lists every object the router-proxy path creates for a
// ModelRouter, as (kind, name) pairs, in the order removeRouterProxyObjects
// deletes them. The admin Service is a second object of the Service kind,
// named after routerProxyAdminResourceName.
func proxyObjectChecks(mrName string) []struct {
	kind string
	name string
} {
	base := routerProxyResourceName(mrName)
	return []struct {
		kind string
		name string
	}{
		{"ConfigMap", base},
		{"Deployment", base},
		{"Service", base},
		{"admin Service", base + "-admin"},
		{"ServiceAccount", base},
		{"Role", base},
		{"RoleBinding", base},
	}
}

// getProxyObject fetches one router-proxy object by kind name for the
// existence assertions, failing the test on anything but NotFound.
func getProxyObject(t *testing.T, c client.Client, ns, kind, name string) error {
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
	return c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, obj)
}

func assertProxyObjectsExist(t *testing.T, c client.Client, ns, mrName string) {
	t.Helper()
	for _, check := range proxyObjectChecks(mrName) {
		if err := getProxyObject(t, c, ns, check.kind, check.name); err != nil {
			t.Errorf("router-proxy %s %s/%s does not exist: %v", check.kind, ns, check.name, err)
		}
	}
}

func assertProxyObjectsGone(t *testing.T, c client.Client, ns, mrName string) {
	t.Helper()
	for _, check := range proxyObjectChecks(mrName) {
		err := getProxyObject(t, c, ns, check.kind, check.name)
		if err == nil {
			t.Errorf("router-proxy %s %s/%s still exists after leaving Proxy mode", check.kind, ns, check.name)
		}
	}
}

// externalProxyRouter builds a Proxy-path-valid router with one cloud backend
// and no pools, in the given data plane mode.
func externalProxyRouter(ns, name string, dataPlane inferencev1alpha1.ModelRouterDataPlane) *inferencev1alpha1.ModelRouter {
	return &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: dataPlane,
			Backends: []inferencev1alpha1.RouterBackend{
				{
					Name: "cloud-opus",
					External: &inferencev1alpha1.ExternalProvider{
						Provider: "anthropic",
						Model:    "claude-opus-4-7",
						URL:      "https://api.anthropic.com",
					},
					Tier: "cloud",
				},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "all", Route: inferencev1alpha1.RuleRoute{Backends: []string{"cloud-opus"}}},
			},
			DefaultRoute: "cloud-opus",
		},
	}
}

// makePooledBackend creates an InferenceService and a ModelPool that lists it
// as a member, so compileRouterConfig resolves the backend to a pool and the
// proxy path provisions the activation RBAC (HasPools).
func makePooledBackend(t *testing.T, c client.Client, ns, isvcName, poolName string) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: isvcName,
			Endpoint: &inferencev1alpha1.EndpointSpec{Port: 8080},
		},
	}
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create backend isvc %s: %v", isvcName, err)
	}
	pool := &inferencev1alpha1.ModelPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: ns},
		Spec: inferencev1alpha1.ModelPoolSpec{
			SwapPolicy: inferencev1alpha1.ModelPoolSwapPolicySticky,
			SwapBudget: &metav1.Duration{Duration: 6 * time.Minute},
			Members: []inferencev1alpha1.ModelPoolMember{
				{InferenceServiceRef: corev1.LocalObjectReference{Name: isvcName}},
			},
		},
	}
	if err := c.Create(context.Background(), pool); err != nil {
		t.Fatalf("create modelpool %s: %v", poolName, err)
	}
}

// pooledProxyRouter builds a router whose single backend references the pooled
// InferenceService makePooledBackend created.
func pooledProxyRouter(ns, name, isvcName string, dataPlane inferencev1alpha1.ModelRouterDataPlane) *inferencev1alpha1.ModelRouter {
	mr := &inferencev1alpha1.ModelRouter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ModelRouterSpec{
			DataPlane: dataPlane,
			Backends: []inferencev1alpha1.RouterBackend{
				{Name: isvcName, InferenceServiceRef: corev1LocalRef(isvcName)},
			},
			Rules: []inferencev1alpha1.RouterRule{
				{Name: "all", Route: inferencev1alpha1.RuleRoute{Backends: []string{isvcName}}},
			},
			DefaultRoute: isvcName,
		},
	}
	return mr
}

// switchDataPlane updates the router's dataPlane mode the way a user would.
func switchDataPlane(t *testing.T, c client.Client, ns, name string, dataPlane inferencev1alpha1.ModelRouterDataPlane) {
	t.Helper()
	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	fresh.Spec.DataPlane = dataPlane
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("switch dataPlane to %s: %v", dataPlane, err)
	}
}

// TestModelRouterProxySkippedForAgentGateway pins the first half of #2004: a
// router in AgentGateway mode gets no router-proxy objects at all, and the
// skip costs no Lists - the sweep resolves each object by its known name
// through the cache, so a router that was never in Proxy mode never makes a
// List or a write.
func TestModelRouterProxySkippedForAgentGateway(t *testing.T) {
	c, cfg, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	mr := externalProxyRouter(ns, "agw-no-proxy", inferencev1alpha1.ModelRouterDataPlaneAgentGateway)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	raw, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	counter := &countingListClient{Client: raw}
	r := &ModelRouterReconciler{Client: counter, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
	for i := 1; i <= 3; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
		}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	assertProxyObjectsGone(t, c, ns, mr.Name)

	fresh := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, fresh); err != nil {
		t.Fatalf("get modelrouter: %v", err)
	}
	if len(fresh.Status.Conditions) != 0 {
		t.Errorf("non-Proxy reconcile wrote status conditions (%d); Proxy-mode status belongs to the sibling reconcilers", len(fresh.Status.Conditions))
	}
	if counter.lists != 0 {
		t.Errorf("the non-Proxy sweep made %d List calls across 3 reconciles, want 0 (Gets by known name)", counter.lists)
	}
}

// TestModelRouterProxySwitchedAwayCleansUp pins the second half of #2004 for
// both gateway modes: a pooled Proxy router has all seven router-proxy
// objects (including the activation RBAC and both Services); switching the
// data plane removes every one of them on the next reconcile.
func TestModelRouterProxySwitchedAwayCleansUp(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()

	for _, tc := range []struct {
		name      string
		dataPlane inferencev1alpha1.ModelRouterDataPlane
	}{
		{"to Gateway", inferencev1alpha1.ModelRouterDataPlaneGateway},
		{"to AgentGateway", inferencev1alpha1.ModelRouterDataPlaneAgentGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newTestNamespace(t, c)
			makePooledBackend(t, c, ns, "switch-cuda", "switch-pool")
			mr := pooledProxyRouter(ns, "switch-router", "switch-cuda", inferencev1alpha1.ModelRouterDataPlaneProxy)
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatalf("create modelrouter: %v", err)
			}

			r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
			}); err != nil {
				t.Fatalf("proxy-mode reconcile: %v", err)
			}
			assertProxyObjectsExist(t, c, ns, mr.Name)

			switchDataPlane(t, c, ns, mr.Name, tc.dataPlane)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
			}); err != nil {
				t.Fatalf("reconcile after switch: %v", err)
			}
			assertProxyObjectsGone(t, c, ns, mr.Name)

			// The switch must not have taken the router itself with it.
			if err := c.Get(context.Background(), types.NamespacedName{Name: mr.Name, Namespace: ns}, &inferencev1alpha1.ModelRouter{}); err != nil {
				t.Errorf("modelrouter vanished after the switch: %v", err)
			}
		})
	}
}

// TestModelRouterProxySwitchDoesNotTouchForeignObjects pins the ownership
// gate: an object that happens to carry the generated router-proxy name but
// is not controlled by this router is left alone and never adopted. The
// router is never reconciled in Proxy mode first - the Proxy upsert path
// adopts unowned same-name objects (a separate, known gap) - so the first
// reconcile runs after the switch to Gateway, with only the sweep in play.
func TestModelRouterProxySwitchDoesNotTouchForeignObjects(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	mr := externalProxyRouter(ns, "foreign-router", inferencev1alpha1.ModelRouterDataPlaneProxy)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}
	proxyName := routerProxyResourceName(mr.Name)

	foreignCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: proxyName, Namespace: ns},
		Data:       map[string]string{"config": "not-the-operators"},
	}
	if err := c.Create(context.Background(), foreignCM); err != nil {
		t.Fatalf("create foreign configmap: %v", err)
	}
	foreignDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: proxyName, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foreign"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "foreign"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "registry.k8s.io/pause:3.10"}},
				},
			},
		},
	}
	if err := c.Create(context.Background(), foreignDeploy); err != nil {
		t.Fatalf("create foreign deployment: %v", err)
	}

	switchDataPlane(t, c, ns, mr.Name, inferencev1alpha1.ModelRouterDataPlaneGateway)
	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: proxyName, Namespace: ns}, cm); err != nil {
		t.Fatalf("foreign ConfigMap %s/%s was deleted by the sweep", ns, proxyName)
	}
	if cm.Data["config"] != "not-the-operators" || len(cm.OwnerReferences) != 0 {
		t.Errorf("foreign ConfigMap was modified: data=%v ownerRefs=%v", cm.Data, cm.OwnerReferences)
	}
	deploy := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: proxyName, Namespace: ns}, deploy); err != nil {
		t.Fatalf("foreign Deployment %s/%s was deleted by the sweep", ns, proxyName)
	}
	if *deploy.Spec.Replicas != 1 || deploy.Spec.Template.Labels["app"] != "foreign" || len(deploy.OwnerReferences) != 0 {
		t.Errorf("foreign Deployment was modified: replicas=%d labels=%v ownerRefs=%v",
			deploy.Spec.Replicas, deploy.Spec.Template.Labels, deploy.OwnerReferences)
	}
}

// TestModelRouterProxyReturnsOnSwitchBack pins the fix's boundary: the
// non-Proxy path must not swallow Proxy mode. A router that went
// Gateway -> Proxy gets its full router-proxy surface back, activation RBAC
// included.
func TestModelRouterProxyReturnsOnSwitchBack(t *testing.T) {
	c, _, stop := startBudgetTestEnv(t)
	defer stop()
	ns := newTestNamespace(t, c)

	makePooledBackend(t, c, ns, "back-cuda", "back-pool")
	mr := pooledProxyRouter(ns, "back-router", "back-cuda", inferencev1alpha1.ModelRouterDataPlaneGateway)
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create modelrouter: %v", err)
	}

	r := &ModelRouterReconciler{Client: c, Scheme: scheme.Scheme, RouterProxyImage: "test-image"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("gateway-mode reconcile: %v", err)
	}
	assertProxyObjectsGone(t, c, ns, mr.Name)

	switchDataPlane(t, c, ns, mr.Name, inferencev1alpha1.ModelRouterDataPlaneProxy)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: ns},
	}); err != nil {
		t.Fatalf("proxy-mode reconcile after switching back: %v", err)
	}
	assertProxyObjectsExist(t, c, ns, mr.Name)
}

// TestModelRouterProxyRBACCoversDelete parses the generated ClusterRole and
// pins get and delete on the six kinds the router-proxy sweep can delete.
// The kind e2e never switches a router's data plane, so this is the only
// RBAC guard for the delete path; envtest runs as cluster-admin and cannot
// catch a missing marker on its own.
func TestModelRouterProxyRBACCoversDelete(t *testing.T) {
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

	required := []struct {
		key   string
		verbs []string
	}{
		{"apps/deployments", []string{"get", "delete"}},
		{"/services", []string{"get", "delete"}},
		{"/configmaps", []string{"get", "delete"}},
		{"/serviceaccounts", []string{"get", "delete"}},
		{"rbac.authorization.k8s.io/roles", []string{"get", "delete"}},
		{"rbac.authorization.k8s.io/rolebindings", []string{"get", "delete"}},
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
