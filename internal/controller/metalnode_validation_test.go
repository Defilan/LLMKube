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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestMetalNodeCRDValidation runs the MetalNode schema against the envtest
// API server: the markers on the Go type mean nothing until the generated CRD
// enforces them at admission.
func TestMetalNodeCRDValidation(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()

	valid := inferencev1alpha1.MetalNodeSpec{
		ServiceAccountName: "llmkube-metal-mac-a",
		LeaseName:          "mac-a",
	}
	cases := []struct {
		name    string
		mutate  func(*inferencev1alpha1.MetalNodeSpec)
		wantErr string
	}{
		{name: "minimal spec", mutate: func(*inferencev1alpha1.MetalNodeSpec) {}},
		{name: "IPv4 hostIP", mutate: func(s *inferencev1alpha1.MetalNodeSpec) { s.HostIP = "100.64.0.2" }},
		{name: "IPv6 hostIP", mutate: func(s *inferencev1alpha1.MetalNodeSpec) { s.HostIP = "fd7a:115c:a1e0::2" }},
		{
			name:    "serviceAccountName with uppercase",
			mutate:  func(s *inferencev1alpha1.MetalNodeSpec) { s.ServiceAccountName = "Mac_A" },
			wantErr: "spec.serviceAccountName",
		},
		{
			name:    "empty serviceAccountName",
			mutate:  func(s *inferencev1alpha1.MetalNodeSpec) { s.ServiceAccountName = "" },
			wantErr: "spec.serviceAccountName",
		},
		{
			name:    "leaseName with a slash",
			mutate:  func(s *inferencev1alpha1.MetalNodeSpec) { s.LeaseName = "ns/mac-a" },
			wantErr: "spec.leaseName",
		},
		// spec.hostIP has no CEL rule: isIP() needs Kubernetes 1.31 and the
		// chart installs this CRD everywhere. The controller reports a bad
		// value instead (TestMetalNodeInvalidHostIP); only the length is
		// enforced here.
		{name: "hostname as hostIP is left to the controller", mutate: func(s *inferencev1alpha1.MetalNodeSpec) { s.HostIP = "mac-a.local" }},
		{
			name:    "overlong hostIP",
			mutate:  func(s *inferencev1alpha1.MetalNodeSpec) { s.HostIP = strings.Repeat("1", 46) },
			wantErr: "spec.hostIP",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid
			tc.mutate(&spec)
			mn := &inferencev1alpha1.MetalNode{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "mn-", Namespace: testNS},
				Spec:       spec,
			}
			err := c.Create(ctx, mn)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("create rejected a valid spec: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("create admitted %+v, want a rejection naming %q", spec, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("create error = %q, want it to name %q", err, tc.wantErr)
			}
		})
	}
}
