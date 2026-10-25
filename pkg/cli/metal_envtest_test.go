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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metaljoin"
)

// These tests run `llmkube metal token create` and `metal remove` end to end
// against envtest's kube-apiserver, which signs ServiceAccount tokens and
// enforces RBAC for them, so the minted join token is a real one.

func metalEnvtest(t *testing.T) (*rest.Config, client.Client, string) {
	t.Helper()
	cfg := cacheListEnvtest(t)
	pointKubeconfigAt(t, cfg)
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ns := "metal-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	if len(ns) > 63 {
		// Subtests share a long prefix: keep them apart with a hash.
		h := fnv.New32a()
		_, _ = h.Write([]byte(ns))
		ns = fmt.Sprintf("%s-%08x", strings.TrimRight(ns[:50], "-"), h.Sum32())
	}
	ns = strings.TrimRight(strings.ReplaceAll(ns, "_", "-"), "-")
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return cfg, c, ns
}

// manifestRole reads the shared Role from the file on disk, independently of
// the embedded copy the command uses.
func manifestRole(t *testing.T) *rbacv1.Role {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deployment", "macos", "metal-agent-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(raw), "\n---") {
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			t.Fatal(err)
		}
		if probe.Kind == "Role" {
			role := &rbacv1.Role{}
			if err := yaml.Unmarshal([]byte(doc), role); err != nil {
				t.Fatal(err)
			}
			return role
		}
	}
	t.Fatal("no Role in metal-agent-rbac.yaml")
	return nil
}

func decodeJoinLine(t *testing.T, stdout string) metaljoin.Payload {
	t.Helper()
	first, _, _ := strings.Cut(stdout, "\n")
	str, ok := strings.CutPrefix(first, "llmkube metal join ")
	if !ok {
		t.Fatalf("first stdout line is not the join command: %.40q", first)
	}
	if strings.ContainsAny(str, " \t") {
		t.Fatalf("first stdout line carries more than the join string")
	}
	p, err := metaljoin.Decode(str)
	if err != nil {
		t.Fatalf("decode join string: %v", err)
	}
	return p
}

// jwtClaims decodes the token's claims without verifying it.
func jwtClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a JWT (%d parts)", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func whoAmI(t *testing.T, cfg *rest.Config, token string) (string, error) {
	t.Helper()
	kube, err := kubernetes.NewForConfig(scopedRestConfig(cfg, token))
	if err != nil {
		t.Fatal(err)
	}
	r, err := kube.AuthenticationV1().SelfSubjectReviews().Create(context.Background(),
		&authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return r.Status.UserInfo.Username, nil
}

func wantMacALabels(t *testing.T, kind string, obj metav1.Object) {
	t.Helper()
	l := obj.GetLabels()
	if l["app.kubernetes.io/managed-by"] != "llmkube-cli" || l[inferencev1alpha1.LabelMetalNode] != "mac-a" {
		t.Errorf("%s %s labels = %v, want managed-by=llmkube-cli and %s=mac-a", kind, obj.GetName(), l,
			inferencev1alpha1.LabelMetalNode)
	}
}

func TestMetalTokenCreateCreatesExactlyThePerMacObjects(t *testing.T) {
	cfg, c, ns := metalEnvtest(t)

	stdout, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("token create: %v\nstderr: %s", err, stderr)
	}

	// stdout: the join command alone, then the validity line, then the pin.
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout has %d lines, want 3", len(lines))
	}
	p := decodeJoinLine(t, stdout)
	if p.Namespace != ns || p.Mac != "mac-a" || p.Server != cfg.Host {
		t.Errorf("payload = %v, want namespace %s mac mac-a server %s", p, ns, cfg.Host)
	}
	if !strings.HasPrefix(lines[1], "valid for 30 minutes (until ") ||
		!strings.HasSuffix(lines[1], "), single use, Mac mac-a in namespace "+ns) {
		t.Errorf("validity line = %q", lines[1])
	}
	if lines[2] != "CA pin: "+p.Pin {
		t.Errorf("pin line = %q, want CA pin: %s", lines[2], p.Pin)
	}
	if strings.Contains(stderr, p.Token) {
		t.Error("stderr carries the token")
	}

	assertMacAIdentity(t, c, ns)
	secret := assertMacAObjects(t, c, ns)
	assertOnlyMacAObjectsLabeled(t, c, ns)
	assertMacAJoinToken(t, cfg, ns, p.Token, secret)
}

// assertMacAIdentity checks the ServiceAccount, the per-Mac Role, the shared
// Role and both RoleBindings.
func assertMacAIdentity(t *testing.T, c client.Client, ns string) {
	t.Helper()
	ctx := context.Background()
	n := newMetalNames("mac-a")
	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }

	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, key(n.ServiceAccount), sa); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Errorf("ServiceAccount automountServiceAccountToken = %v, want false", sa.AutomountServiceAccountToken)
	}
	wantMacALabels(t, "ServiceAccount", sa)

	role := &rbacv1.Role{}
	if err := c.Get(ctx, key(n.Role), role); err != nil {
		t.Fatalf("get per-Mac Role: %v", err)
	}
	if want := wantMacARules(); !reflect.DeepEqual(role.Rules, want) {
		t.Errorf("per-Mac Role rules:\n got %+v\nwant %+v", role.Rules, want)
	}
	wantMacALabels(t, "Role", role)

	shared := &rbacv1.Role{}
	if err := c.Get(ctx, key("llmkube-metal-agent"), shared); err != nil {
		t.Fatalf("get shared Role: %v", err)
	}
	if want := manifestRole(t).Rules; !reflect.DeepEqual(shared.Rules, want) {
		t.Errorf("shared Role rules differ from deployment/macos/metal-agent-rbac.yaml:\n got %+v\nwant %+v",
			shared.Rules, want)
	}

	for name, roleName := range map[string]string{n.RoleBinding: n.Role, n.SharedBinding: "llmkube-metal-agent"} {
		rb := &rbacv1.RoleBinding{}
		if err := c.Get(ctx, key(name), rb); err != nil {
			t.Fatalf("get RoleBinding %s: %v", name, err)
		}
		wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleName}
		wantSubjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: n.ServiceAccount, Namespace: ns}}
		if rb.RoleRef != wantRef || !reflect.DeepEqual(rb.Subjects, wantSubjects) {
			t.Errorf("RoleBinding %s = %+v %+v, want %+v %+v", name, rb.RoleRef, rb.Subjects, wantRef, wantSubjects)
		}
		wantMacALabels(t, "RoleBinding", rb)
	}
}

// assertMacAObjects checks the Lease, the MetalNode and the join Secret, and
// returns the Secret.
func assertMacAObjects(t *testing.T, c client.Client, ns string) *corev1.Secret {
	t.Helper()
	ctx := context.Background()
	n := newMetalNames("mac-a")
	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }

	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, key(n.Lease), lease); err != nil {
		t.Fatalf("get Lease: %v", err)
	}
	if d := lease.Spec.LeaseDurationSeconds; d == nil || *d != 45 || lease.Spec.HolderIdentity != nil {
		t.Errorf("Lease spec = %+v, want leaseDurationSeconds 45 and no holder", lease.Spec)
	}
	wantMacALabels(t, "Lease", lease)

	mn := &inferencev1alpha1.MetalNode{}
	if err := c.Get(ctx, key(n.MetalNode), mn); err != nil {
		t.Fatalf("get MetalNode: %v", err)
	}
	if mn.Spec.ServiceAccountName != n.ServiceAccount || mn.Spec.LeaseName != n.Lease {
		t.Errorf("MetalNode spec = %+v", mn.Spec)
	}
	wantMacALabels(t, "MetalNode", mn)

	secret := &corev1.Secret{}
	if err := c.Get(ctx, key(n.JoinSecret), secret); err != nil {
		t.Fatalf("get join Secret: %v", err)
	}
	if secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != 0 {
		t.Errorf("join Secret type %q with %d data keys, want Opaque and empty", secret.Type, len(secret.Data))
	}
	wantMacALabels(t, "Secret", secret)
	return secret
}

// assertOnlyMacAObjectsLabeled: exactly these objects carry the Mac's label.
func assertOnlyMacAObjectsLabeled(t *testing.T, c client.Client, ns string) {
	t.Helper()
	sel := client.MatchingLabels{inferencev1alpha1.LabelMetalNode: "mac-a"}
	counts := map[string]int{}
	for kind, list := range map[string]client.ObjectList{
		"ServiceAccount": &corev1.ServiceAccountList{}, "Role": &rbacv1.RoleList{},
		"RoleBinding": &rbacv1.RoleBindingList{}, "Lease": &coordinationv1.LeaseList{},
		"Secret": &corev1.SecretList{}, "MetalNode": &inferencev1alpha1.MetalNodeList{},
	} {
		if err := c.List(context.Background(), list, client.InNamespace(ns), sel); err != nil {
			t.Fatal(err)
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			t.Fatal(err)
		}
		counts[kind] = len(items)
	}
	want := map[string]int{"ServiceAccount": 1, "Role": 1, "RoleBinding": 2, "Lease": 1, "Secret": 1, "MetalNode": 1}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("labeled objects = %v, want %v", counts, want)
	}
}

// assertMacAJoinToken: the token is real, belongs to the Mac's
// ServiceAccount, lives 30 minutes and is bound to the join Secret.
func assertMacAJoinToken(t *testing.T, cfg *rest.Config, ns, token string, secret *corev1.Secret) {
	t.Helper()
	user, err := whoAmI(t, cfg, token)
	if err != nil {
		t.Fatalf("SelfSubjectReview with the join token: %v", err)
	}
	if want := "system:serviceaccount:" + ns + ":llmkube-metal-mac-a"; user != want {
		t.Errorf("join token user = %q, want %q", user, want)
	}
	claims := jwtClaims(t, token)
	exp, _ := claims["exp"].(float64)
	iat, _ := claims["iat"].(float64)
	if exp-iat != 1800 {
		t.Errorf("token lifetime = %v s, want 1800", exp-iat)
	}
	k8sClaims, _ := claims["kubernetes.io"].(map[string]any)
	bound, _ := k8sClaims["secret"].(map[string]any)
	if bound["name"] != secret.Name || bound["uid"] != string(secret.UID) {
		t.Errorf("token bound to %v, want Secret %s uid %s", bound, secret.Name, secret.UID)
	}
}

// TestMetalJoinTokenRBAC authenticates as the join token against the RBAC
// enforcing API server: the per-Mac Role lets it reach only its own objects.
func TestMetalJoinTokenRBAC(t *testing.T) {
	cfg, c, ns := metalEnvtest(t)
	ctx := context.Background()
	if _, stderr, err := runMetal(t, "token", "create", "mac-b", "-n", ns, "--ttl", "10m"); err != nil {
		t.Fatalf("token create mac-b: %v\n%s", err, stderr)
	}
	stdout, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("token create mac-a: %v\n%s", err, stderr)
	}
	p := decodeJoinLine(t, stdout)

	scoped, err := client.New(scopedRestConfig(cfg, p.Token), client.Options{Scheme: c.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(scopedRestConfig(cfg, p.Token))
	if err != nil {
		t.Fatal(err)
	}
	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }

	if err := scoped.Get(ctx, key("mac-a"), &inferencev1alpha1.MetalNode{}); err != nil {
		t.Errorf("get own MetalNode: %v", err)
	}
	assertForbidden(t, scoped.Get(ctx, key("mac-b"), &inferencev1alpha1.MetalNode{}), "get other MetalNode")
	own := &inferencev1alpha1.MetalNode{}
	if err := c.Get(ctx, key("mac-a"), own); err != nil {
		t.Fatal(err)
	}
	own.Spec.HostIP = "10.0.0.9"
	assertForbidden(t, scoped.Update(ctx, own), "update own MetalNode")

	lease := &coordinationv1.Lease{}
	if err := scoped.Get(ctx, key("mac-a"), lease); err != nil {
		t.Fatalf("get own Lease: %v", err)
	}
	now := metav1.NewMicroTime(time.Now())
	lease.Spec.RenewTime = &now
	if err := scoped.Update(ctx, lease); err != nil {
		t.Errorf("update own Lease: %v", err)
	}
	assertForbidden(t, scoped.Get(ctx, key("mac-b"), &coordinationv1.Lease{}), "get other Lease")

	ttl := int64(600)
	mint := func(sa string) error {
		_, err := kube.CoreV1().ServiceAccounts(ns).CreateToken(ctx, sa, &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &ttl},
		}, metav1.CreateOptions{})
		return err
	}
	if err := mint("llmkube-metal-mac-a"); err != nil {
		t.Errorf("mint own token: %v", err)
	}
	assertForbidden(t, mint("llmkube-metal-mac-b"), "mint other Mac's token")

	joinSecret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	assertForbidden(t, scoped.Delete(ctx, joinSecret("llmkube-metal-mac-b-join")), "delete other Mac's join Secret")
	if err := scoped.Delete(ctx, joinSecret("llmkube-metal-mac-a-join")); err != nil {
		t.Errorf("delete own join Secret: %v", err)
	}
}

func TestMetalTokenCreateLeavesACustomizedSharedRole(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	ctx := context.Background()
	custom := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "llmkube-metal-agent"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"},
			ResourceNames: []string{"llmkube-metal-relay"},
		}},
	}
	if err := c.Create(ctx, custom); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "llmkube-metal-agent") || !strings.Contains(strings.ToLower(stderr), "warning") {
		t.Errorf("stderr does not warn about the customized shared Role: %q", stderr)
	}
	got := &rbacv1.Role{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "llmkube-metal-agent"}, got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Rules, custom.Rules) {
		t.Errorf("shared Role was rewritten: %+v", got.Rules)
	}
}

// TestMetalTokenCreateReissueKillsThePreviousString: re-running for the same
// Mac recreates the join Secret, which invalidates the previous token.
func TestMetalTokenCreateReissueKillsThePreviousString(t *testing.T) {
	cfg, c, ns := metalEnvtest(t)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a-join"}

	stdout1, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("first token create: %v\n%s", err, stderr)
	}
	first := &corev1.Secret{}
	if err := c.Get(ctx, key, first); err != nil {
		t.Fatal(err)
	}
	// The agent has renewed its Lease since: a re-issue must not reset it.
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mac-a"}, lease); err != nil {
		t.Fatal(err)
	}
	holder := "mac-a"
	lease.Spec.HolderIdentity = &holder
	if err := c.Update(ctx, lease); err != nil {
		t.Fatal(err)
	}

	stdout2, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("second token create: %v\n%s", err, stderr)
	}
	second := &corev1.Secret{}
	if err := c.Get(ctx, key, second); err != nil {
		t.Fatal(err)
	}
	if second.UID == first.UID {
		t.Errorf("join Secret UID unchanged (%s): it was not recreated", first.UID)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mac-a"}, lease); err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		t.Errorf("re-issue reset the Lease: %+v", lease.Spec)
	}

	// The first token was never used, so no authenticator cache keeps it
	// alive: its bound Secret is gone and it is rejected outright.
	if _, err := whoAmI(t, cfg, decodeJoinLine(t, stdout1).Token); !apierrors.IsUnauthorized(err) {
		t.Errorf("first join token after re-issue: err = %v, want 401", err)
	}
	if _, err := whoAmI(t, cfg, decodeJoinLine(t, stdout2).Token); err != nil {
		t.Errorf("second join token: %v", err)
	}
}

func TestMetalTokenCreateRefusesForeignObjects(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	ctx := context.Background()
	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "llmkube-metal-mac-a"}}
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	_, _, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err == nil || !strings.Contains(err.Error(), "llmkube-metal-mac-a") ||
		!strings.Contains(err.Error(), inferencev1alpha1.LabelMetalNode) {
		t.Fatalf("token create over an unlabeled ServiceAccount: err = %v, want a refusal naming it and the label", err)
	}
	rbs := &rbacv1.RoleBindingList{}
	if err := c.List(ctx, rbs, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(rbs.Items) != 0 {
		t.Errorf("token create bound roles to a ServiceAccount it does not manage: %d RoleBindings", len(rbs.Items))
	}
}

func TestMetalRemoveDeletesTheServiceAccountFirst(t *testing.T) {
	cfg, c, ns := metalEnvtest(t)
	ctx := context.Background()
	stdout, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	token := decodeJoinLine(t, stdout).Token
	if _, stderr, err := runMetal(t, "token", "create", "mac-b", "-n", ns); err != nil {
		t.Fatalf("token create mac-b: %v\n%s", err, stderr)
	}

	saKey := types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a"}
	var order []string
	wc, err := client.NewWithWatch(cfg, client.Options{Scheme: c.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	recording := interceptor.NewClient(wc, interceptor.Funcs{
		Delete: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			kind := reflect.TypeOf(obj).Elem().Name()
			order = append(order, kind+"/"+obj.GetName())
			if kind != "ServiceAccount" {
				if err := inner.Get(ctx, saKey, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
					t.Errorf("deleting %s/%s while the ServiceAccount still exists (get: %v)", kind, obj.GetName(), err)
				}
			}
			return inner.Delete(ctx, obj, opts...)
		},
	})
	var out bytes.Buffer
	if err := metalRemove(ctx, recording, ns, "mac-a", &out); err != nil {
		t.Fatalf("metalRemove: %v", err)
	}
	if len(order) == 0 || order[0] != "ServiceAccount/llmkube-metal-mac-a" {
		t.Errorf("delete order = %v, want the ServiceAccount first", order)
	}
	for _, kind := range []string{"Role", "RoleBinding", "Lease", "Secret", "MetalNode"} {
		if !strings.Contains(out.String(), kind) {
			t.Errorf("output does not report the removed %s: %q", kind, out.String())
		}
	}

	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }
	for name, obj := range map[string]client.Object{
		"llmkube-metal-mac-a":        &corev1.ServiceAccount{},
		"llmkube-metal-mac-a-join":   &corev1.Secret{},
		"mac-a":                      &coordinationv1.Lease{},
		"llmkube-metal-mac-a-shared": &rbacv1.RoleBinding{},
	} {
		if err := c.Get(ctx, key(name), obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s still exists after remove (err %v)", obj, name, err)
		}
	}
	if err := c.Get(ctx, key("llmkube-metal-mac-a"), &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Errorf("per-Mac Role still exists after remove (err %v)", err)
	}
	if err := c.Get(ctx, key("llmkube-metal-mac-a"), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("per-Mac RoleBinding still exists after remove (err %v)", err)
	}
	if err := c.Get(ctx, key("mac-a"), &inferencev1alpha1.MetalNode{}); !apierrors.IsNotFound(err) {
		t.Errorf("MetalNode still exists after remove (err %v)", err)
	}
	// The shared Role and the other Mac stay.
	if err := c.Get(ctx, key("llmkube-metal-agent"), &rbacv1.Role{}); err != nil {
		t.Errorf("remove deleted the shared Role: %v", err)
	}
	if err := c.Get(ctx, key("mac-b"), &inferencev1alpha1.MetalNode{}); err != nil {
		t.Errorf("remove touched another Mac: %v", err)
	}
	if _, err := whoAmI(t, cfg, token); !apierrors.IsUnauthorized(err) {
		t.Errorf("join token after remove: err = %v, want 401", err)
	}

	// A second run finds nothing and succeeds.
	stdout, stderr, err = runMetal(t, "remove", "mac-a", "-n", ns)
	if err != nil {
		t.Fatalf("second remove: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "nothing to remove") {
		t.Errorf("second remove output = %q, want it to say nothing was removed", stdout)
	}
}

// TestMetalTokenCreateRepointsItsOwnRoleBinding: roleRef is immutable, so a
// binding of this Mac's that points at another Role is replaced.
func TestMetalTokenCreateRepointsItsOwnRoleBinding(t *testing.T) {
	_, c, ns := metalEnvtest(t)
	ctx := context.Background()
	stale := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "llmkube-metal-mac-a",
			Labels: map[string]string{inferencev1alpha1.LabelMetalNode: "mac-a"},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "something-else"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "llmkube-metal-mac-a", Namespace: ns}},
	}
	if err := c.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runMetal(t, "token", "create", "mac-a", "-n", ns); err != nil {
		t.Fatalf("token create: %v\n%s", err, stderr)
	}
	got := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "llmkube-metal-mac-a"}, got); err != nil {
		t.Fatal(err)
	}
	if got.RoleRef.Name != "llmkube-metal-mac-a" || got.UID == stale.UID {
		t.Errorf("RoleBinding roleRef = %s (uid %s, stale uid %s), want it recreated for llmkube-metal-mac-a",
			got.RoleRef.Name, got.UID, stale.UID)
	}
}
