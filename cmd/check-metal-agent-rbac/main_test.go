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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// TestCompareRules pins the comparison's edge cases: a missing rule, an extra
// rule, a resourceNames mismatch, and that rule order and grouping do not
// count as drift.
func TestCompareRules(t *testing.T) {
	isvc := rbacv1.PolicyRule{
		APIGroups: []string{"inference.llmkube.dev"},
		Resources: []string{"inferenceservices"},
		Verbs:     []string{"get", "list"},
	}
	manifest := []rbacv1.PolicyRule{
		isvc,
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
	}

	tests := []struct {
		name    string
		got     []rbacv1.PolicyRule
		want    []rbacv1.PolicyRule
		pins    []string
		wantSub string // "" means no problems
	}{
		{
			name: "identical",
			got:  manifest,
			want: manifest,
		},
		{
			name: "reordered and regrouped is not drift",
			got: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
				{
					APIGroups: []string{"inference.llmkube.dev"},
					Resources: []string{"inferenceservices"},
					Verbs:     []string{"list", "get"},
				},
			},
			want: manifest,
		},
		{
			name: "missing rule",
			got:  manifest[1:],
			want: manifest,
			// The chart is the copy that loses the grant.
			wantSub: "chart rules are missing: apiGroups=[inference.llmkube.dev]",
		},
		{
			name: "extra rule",
			got: append(append([]rbacv1.PolicyRule{}, manifest...),
				rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}}),
			want:    manifest,
			wantSub: "chart grants rules the manifest does not:",
		},
		{
			name: "one verb short",
			got: []rbacv1.PolicyRule{
				{
					APIGroups: []string{"inference.llmkube.dev"},
					Resources: []string{"inferenceservices"},
					Verbs:     []string{"get"},
				},
				manifest[1],
			},
			want:    manifest,
			wantSub: "chart rules are missing:",
		},
		{
			name: "unpinned Secret read where the manifest is pinned",
			got:  manifest,
			want: []rbacv1.PolicyRule{
				manifest[0],
				{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}, ResourceNames: []string{"a"}},
			},
			wantSub: "resourceNames",
		},
		{
			name: "pinned render compared against the pinned manifest form",
			got: []rbacv1.PolicyRule{
				manifest[0],
				{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}, ResourceNames: []string{"a"}},
			},
			want: withPins(manifest, []string{"a"}),
			pins: []string{"a"},
		},
		{
			name: "configured pin but the render is unpinned",
			got:  manifest,
			want: withPins(manifest, []string{"a"}),
			pins: []string{"a"},
			// The set comparison already flags the resourceNames difference;
			// the pin check names the cause.
			wantSub: "secrets rule resourceNames []",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := compareRules("test", tt.got, tt.want, tt.pins)
			if tt.wantSub == "" {
				if len(problems) != 0 {
					t.Fatalf("unexpected problems: %v", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("expected a problem containing %q, got none", tt.wantSub)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantSub) {
				t.Fatalf("problems %v do not contain %q", problems, tt.wantSub)
			}
		})
	}
}

// TestCheckRender pins the shape checks: a rendered Secret fails, a wrong
// ServiceAccount name fails, and the correct render passes.
func TestCheckRender(t *testing.T) {
	manifestRules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
	}
	r := render{name: "namespaced", scope: "namespaced", namespaces: []string{"ml"}}

	good := []map[string]any{
		{"kind": "ServiceAccount", "metadata": map[string]any{"name": "llmkube-metal-agent", "namespace": "ml"}},
		{"kind": "Role", "metadata": map[string]any{"name": "llmkube-metal-agent", "namespace": "ml"},
			"rules": []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"get"}}}},
		{"kind": "RoleBinding", "metadata": map[string]any{"name": "llmkube-metal-agent", "namespace": "ml"},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "llmkube-metal-agent"},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "llmkube-metal-agent", "namespace": "ml"}}},
	}

	if problems := checkRender(r, good, manifestRules, "llmkube-metal-agent"); len(problems) != 0 {
		t.Fatalf("correct render reported problems: %v", problems)
	}

	t.Run("rendered Secret", func(t *testing.T) {
		withSecret := append(append([]map[string]any{}, good...),
			map[string]any{"kind": "Secret", "metadata": map[string]any{"name": "tok", "namespace": "ml"}})
		problems := checkRender(r, withSecret, manifestRules, "llmkube-metal-agent")
		if !containsSub(problems, "renders a Secret") {
			t.Fatalf("expected a Secret problem, got: %v", problems)
		}
	})

	t.Run("renamed ServiceAccount", func(t *testing.T) {
		renamed := map[string]any{
			"kind": "ServiceAccount", "metadata": map[string]any{"name": "other", "namespace": "ml"},
		}
		problems := checkRender(r, []map[string]any{renamed, good[1], good[2]}, manifestRules, "llmkube-metal-agent")
		if !containsSub(problems, `ServiceAccount name "other"`) {
			t.Fatalf("expected a ServiceAccount-name problem, got: %v", problems)
		}
	})

	t.Run("missing namespace", func(t *testing.T) {
		r2 := render{name: "namespaced", scope: "namespaced", namespaces: []string{"ml", "elsewhere"}}
		problems := checkRender(r2, good, manifestRules, "llmkube-metal-agent")
		if !containsSub(problems, `no Role for namespace "elsewhere"`) {
			t.Fatalf("expected a missing-namespace problem, got: %v", problems)
		}
	})
}

// TestReadManifest parses the real standalone manifest and requires the Role
// and ServiceAccount the chart must reproduce.
func TestReadManifest(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deployment/macos/metal-agent-rbac.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	rules, sa, err := readManifest(raw)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if sa != "llmkube-metal-agent" {
		t.Fatalf("ServiceAccount name %q, want llmkube-metal-agent", sa)
	}
	if len(rules) != 8 {
		t.Fatalf("manifest has %d rules, want 8; update this and the chart if the audited set changed", len(rules))
	}
}

// TestRealRepo is the check's acceptance test: the committed chart must match
// the committed manifest. It needs helm and skips without it, so the unit suite
// stays runnable on a machine that only builds Go.
func TestRealRepo(t *testing.T) {
	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH; run make check-metal-agent-rbac in CI")
	}
	root := repoRoot(t)
	problems, err := run(root, "deployment/macos/metal-agent-rbac.yaml", "charts/llmkube", helmBin)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("chart and manifest diverge:\n%s", strings.Join(problems, "\n"))
	}
}

// repoRoot is the workspace root as seen from this package's test working dir.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(dir))
}

func containsSub(problems []string, sub string) bool {
	return strings.Contains(strings.Join(problems, "\n"), sub)
}
