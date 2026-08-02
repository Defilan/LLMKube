package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestResolveExternalBackend covers #1395: external backends are compiled into
// the Gateway data plane instead of failing the whole reconcile.
func TestResolveExternalBackend(t *testing.T) {
	ext := func(u string) inferencev1alpha1.RouterBackend {
		return inferencev1alpha1.RouterBackend{
			Name:     "ext",
			External: &inferencev1alpha1.ExternalProvider{URL: u},
		}
	}

	cases := []struct {
		name     string
		url      string
		wantHost string
		wantPort int64
		wantIsIP bool
		wantErr  bool
	}{
		{name: "ip with explicit port", url: "http://192.168.1.47:8083/v1", wantHost: "192.168.1.47", wantPort: 8083, wantIsIP: true},
		{name: "hostname with explicit port", url: "http://mac.lan:8083/v1", wantHost: "mac.lan", wantPort: 8083},
		{name: "https defaults to 443", url: "https://api.example.com/v1", wantHost: "api.example.com", wantPort: 443},
		{name: "http defaults to 80", url: "http://api.example.com/v1", wantHost: "api.example.com", wantPort: 80},
		{name: "empty url is an error", url: "", wantErr: true},
		{name: "relative url has no host", url: "/v1/chat", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveExternalBackend(ext(tc.url))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q, got %+v", tc.url, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
			if got.FQDN != tc.wantHost || got.Port != tc.wantPort || got.IsIP != tc.wantIsIP {
				t.Errorf("resolveExternalBackend(%q) = host=%q port=%d isIP=%v, want host=%q port=%d isIP=%v",
					tc.url, got.FQDN, got.Port, got.IsIP, tc.wantHost, tc.wantPort, tc.wantIsIP)
			}
			// External backends have no InferenceService to read readiness from,
			// so they must compile as Healthy or the ejection pass drops them.
			if !got.Healthy {
				t.Errorf("external backend %q compiled unhealthy; it would be ejected from every route", tc.url)
			}
		})
	}
}

// TestNewRouterBackendIPUsesIPEndpoint pins the endpoint-type split: Envoy
// Gateway rejects a literal address supplied as an fqdn hostname (#1395).
func TestNewRouterBackendIPUsesIPEndpoint(t *testing.T) {
	mr := &inferencev1alpha1.ModelRouter{}
	mr.SetName("r")
	mr.SetNamespace("default")

	u := newRouterBackend(mr, routerBackendResource{Name: "ext", FQDN: "192.168.1.47", Port: 8083, Healthy: true, IsIP: true})
	eps, _, _ := unstructured.NestedSlice(u.Object, "spec", "endpoints")
	if len(eps) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(eps))
	}
	ep := eps[0].(map[string]interface{})
	if _, ok := ep["ip"]; !ok {
		t.Errorf("IP backend must use the ip endpoint type, got %v", ep)
	}
	if _, ok := ep["fqdn"]; ok {
		t.Errorf("IP backend must not use fqdn, got %v", ep)
	}

	u2 := newRouterBackend(mr, routerBackendResource{Name: "in", FQDN: "svc.default.svc.cluster.local", Port: 8080, Healthy: true})
	eps2, _, _ := unstructured.NestedSlice(u2.Object, "spec", "endpoints")
	ep2 := eps2[0].(map[string]interface{})
	if _, ok := ep2["fqdn"]; !ok {
		t.Errorf("hostname backend must use fqdn, got %v", ep2)
	}
}

// TestExternalModelOverrideReachesBackendRef covers #1397: an external
// backend's external.model must reach the upstream as modelNameOverride,
// otherwise the upstream receives the ModelRouter rule key and rejects it.
func TestExternalModelOverrideReachesBackendRef(t *testing.T) {
	mr := &inferencev1alpha1.ModelRouter{}
	mr.Spec.Backends = []inferencev1alpha1.RouterBackend{
		{Name: "ext", External: &inferencev1alpha1.ExternalProvider{
			URL: "http://192.168.1.47:8083/v1", Model: "/models/qwopus-fusion-mxfp4"}},
		{Name: "ext-no-model", External: &inferencev1alpha1.ExternalProvider{
			URL: "http://192.168.1.47:8083/v1"}},
		{Name: "in-cluster", InferenceServiceRef: &corev1.LocalObjectReference{Name: "svc"}},
	}

	got := externalModelOverrides(mr)
	if got["ext"] != "/models/qwopus-fusion-mxfp4" {
		t.Errorf("external backend with a model: got %q, want the upstream identifier", got["ext"])
	}
	// Absent, not empty-string: an external backend that set no model must keep
	// today's pass-through, since some providers accept the router key directly.
	if _, ok := got["ext-no-model"]; ok {
		t.Errorf("external backend without a model must not get an override, got %q", got["ext-no-model"])
	}
	if _, ok := got["in-cluster"]; ok {
		t.Errorf("in-cluster backend must never get an override, got %q", got["in-cluster"])
	}

	refs := compileRuleBackendRefs([]routerBackendRef{
		{Name: "ext", ModelNameOverride: "/models/qwopus-fusion-mxfp4"},
		{Name: "in-cluster"},
	})
	first := refs[0].(map[string]interface{})
	if first["modelNameOverride"] != "/models/qwopus-fusion-mxfp4" {
		t.Errorf("backendRef missing modelNameOverride: %v", first)
	}
	second := refs[1].(map[string]interface{})
	if _, ok := second["modelNameOverride"]; ok {
		t.Errorf("in-cluster backendRef must omit modelNameOverride entirely, got %v", second)
	}
}
