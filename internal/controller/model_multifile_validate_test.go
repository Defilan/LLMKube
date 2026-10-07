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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The CEL rules already constrain spec.fileSha256 keys, but the reconcile-time
// check re-runs the membership test for a stale object or a client that
// bypassed admission. A client cannot be made to bypass admission through the
// envtest API server, so these drive validateMultiFileStagingSource directly on
// a constructed Model.
func TestValidateMultiFileStagingSource_FileSHA256Keys(t *testing.T) {
	const digest = "d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b"
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}

	cases := []struct {
		name        string
		files       []string
		mmproj      string
		keys        map[string]inferencev1alpha1.SHA256Digest
		wantHandled bool
		wantMsg     string
	}{
		{
			name:  "every key names a staged file",
			files: []string{"a.gguf", "b.gguf"},
			keys:  map[string]inferencev1alpha1.SHA256Digest{"a.gguf": digest},
		},
		{
			name:   "key names mmproj",
			files:  []string{"a.gguf"},
			mmproj: "proj.gguf",
			keys:   map[string]inferencev1alpha1.SHA256Digest{"proj.gguf": digest},
		},
		{
			name:        "key names no staged file",
			files:       []string{"a.gguf"},
			keys:        map[string]inferencev1alpha1.SHA256Digest{"c.gguf": digest},
			wantHandled: true,
			wantMsg:     `fileSha256 key "c.gguf" is not one of the staged files`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:     "hf://org/repo",
					Files:      tc.files,
					Mmproj:     tc.mmproj,
					FileSHA256: tc.keys,
				},
			}
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&inferencev1alpha1.Model{}).
				WithObjects(model).
				Build()
			r := &ModelReconciler{Client: c, Scheme: scheme}

			handled, _ := r.validateMultiFileStagingSource(context.Background(), model)
			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v", handled, tc.wantHandled)
			}
			if !tc.wantHandled {
				return
			}
			if model.Status.Phase != PhaseFailed {
				t.Errorf("phase = %q, want %q", model.Status.Phase, PhaseFailed)
			}
			var found bool
			for _, cond := range model.Status.Conditions {
				if strings.Contains(cond.Message, tc.wantMsg) {
					found = true
				}
			}
			if !found {
				t.Errorf("no condition carries %q: %+v", tc.wantMsg, model.Status.Conditions)
			}
		})
	}
}
