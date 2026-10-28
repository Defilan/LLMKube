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
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// The attribution settings survive the compile into the bytes the proxy
// loads: the compiled ConfigMap JSON goes through the proxy's own LoadConfig
// and serves a request whose audit line names the caller (#2283).
func TestCompiledAuditAttributionReachesTheProxy(t *testing.T) {
	mr := canonicalModelRouter()
	mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{AuditLog: &inferencev1alpha1.AuditLogPolicy{
		LogClientAddress:  true,
		TrustedProxyCIDRs: []string{"127.0.0.0/8"},
		CallerHeader:      "X-LLMKube-Client",
	}}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "qwen3-coder"},
	}
	r := newRouterReconcilerForTest(t, mr, isvc)
	compiled, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compileRouterConfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, compiled.JSON, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := router.LoadConfig(path)
	if err != nil {
		t.Fatalf("the proxy refused the compiled config: %v", err)
	}

	// Point every backend at a stub; the attribution is under test, not the
	// address resolution.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer stub.Close()
	for i := range cfg.Backends {
		cfg.Backends[i].Address = stub.URL
		cfg.Backends[i].Endpoints = nil
		cfg.Backends[i].CredentialsEnv = ""
	}
	var logs bytes.Buffer
	mux := http.NewServeMux()
	router.NewProxy(cfg, slog.New(slog.NewTextHandler(&logs, nil))).Mount(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen3-coder"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-LLMKube-Client", "opencode")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	out := logs.String()
	if !strings.Contains(out, "client=203.0.113.9") || !strings.Contains(out, "caller=opencode") {
		t.Errorf("audit line = %s\nwant client=203.0.113.9 and caller=opencode from the compiled policy", out)
	}
}

func TestValidateModelRouterAuditAttribution(t *testing.T) {
	cases := []struct {
		name       string
		audit      inferencev1alpha1.AuditLogPolicy
		wantField  string
		wantSubstr string
	}{
		{
			name:  "valid",
			audit: inferencev1alpha1.AuditLogPolicy{LogClientAddress: true, TrustedProxyCIDRs: []string{"10.0.0.0/8", "192.0.2.7"}, CallerHeader: "X-LLMKube-Client"},
		},
		{
			name:       "bad CIDR",
			audit:      inferencev1alpha1.AuditLogPolicy{LogClientAddress: true, TrustedProxyCIDRs: []string{"10.0.0.0/8", "ingress"}},
			wantField:  "spec.policy.auditLog.trustedProxyCIDRs",
			wantSubstr: `trustedProxyCIDRs[1]: "ingress" is not a CIDR`,
		},
		{
			name:       "trusted proxies without client addresses",
			audit:      inferencev1alpha1.AuditLogPolicy{TrustedProxyCIDRs: []string{"10.0.0.0/8"}},
			wantField:  "spec.policy.auditLog.trustedProxyCIDRs",
			wantSubstr: "no effect unless logClientAddress",
		},
		{
			name:       "credential header",
			audit:      inferencev1alpha1.AuditLogPolicy{CallerHeader: "Authorization"},
			wantField:  "spec.policy.auditLog.callerHeader",
			wantSubstr: "carries a credential",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := validRouter()
			audit := tc.audit
			mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{AuditLog: &audit}
			errs := validateModelRouter(mr)
			if tc.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("want no errors, got: %s", formatValidationErrors(errs))
				}
				return
			}
			for _, e := range errs {
				if e.Field == tc.wantField && strings.Contains(e.Message, tc.wantSubstr) {
					return
				}
			}
			t.Errorf("want %s: %q, got: %s", tc.wantField, tc.wantSubstr, formatValidationErrors(errs))
		})
	}
}
