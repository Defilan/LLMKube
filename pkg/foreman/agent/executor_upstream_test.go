/*
Copyright 2025.

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

package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

func TestUpstreamURLForRepo(t *testing.T) {
	cases := []struct {
		repo string
		want string
	}{
		{"defilantech/LLMKube", "https://github.com/defilantech/LLMKube.git"},
		{"  defilantech/LLMKube  ", "https://github.com/defilantech/LLMKube.git"},
		{"group/subgroup/project", "https://github.com/group/subgroup/project.git"},
		{"", ""},
		{"   ", ""},
		// malformed / unsafe slugs derive no URL (caller falls back to fork HEAD).
		{"defilantech", ""},            // no slash
		{"../../etc/passwd", ""},       // path traversal
		{"defilantech/LLMKube/..", ""}, // traversal segment
		{"defilan tech/LLMKube", ""},   // whitespace
		{"defilantech//LLMKube", ""},   // empty segment
	}
	for _, tc := range cases {
		if got := upstreamURLForRepo(tc.repo); got != tc.want {
			t.Errorf("upstreamURLForRepo(%q) = %q, want %q", tc.repo, got, tc.want)
		}
	}
}

func TestBaseBranchOrDefault(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "main"},
		{"   ", "main"},
		{"main", "main"},
		{"release-1.2", "release-1.2"},
		{" develop ", "develop"},
	}
	for _, tc := range cases {
		if got := baseBranchOrDefault(tc.in); got != tc.want {
			t.Errorf("baseBranchOrDefault(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildDeterministicArgs_ThreadsUpstreamURL verifies every deterministic
// gate run carries the canonical upstream repo URL, so the gate's bite check
// fetches baseBranch from the true upstream and reverts to the real
// merge-base instead of the fork's possibly-stale ref tip (#1259). A task
// with no repo slug (freeform tasks) must yield an absent/empty upstreamURL
// so the shell script's origin-fallback path stays intact.
func TestBuildDeterministicArgs_ThreadsUpstreamURL(t *testing.T) {
	cases := []struct {
		name string
		repo string
		want string
	}{
		{"repo set", "defilantech/LLMKube", "https://github.com/defilantech/LLMKube.git"},
		{"repo empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &foremanv1alpha1.AgenticTask{
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Repo:  tc.repo,
						Issue: 1259,
					},
				},
			}
			raw := buildDeterministicArgs(task, "foreman/issue-1259", "https://github.com/Defilan/LLMKube.git",
				upstreamURLForRepo(task.Spec.Payload.Repo))
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal args: %v", err)
			}
			got, _ := m["upstreamURL"].(string)
			if got != tc.want {
				t.Errorf("args[upstreamURL] = %q, want %q", got, tc.want)
			}
			if tc.want != "" && got != upstreamURLForRepo(tc.repo) {
				t.Errorf("args[upstreamURL] = %q, want it to equal upstreamURLForRepo(%q) = %q",
					got, tc.repo, upstreamURLForRepo(tc.repo))
			}
		})
	}
}

// TestBuildDeterministicArgs_ThreadsBaseBranch verifies the verify-gate args
// carry the task's base branch (defaulting to main), so the bite check diffs
// against the same base the coder branched from (#813).
func TestBuildDeterministicArgs_ThreadsBaseBranch(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"explicit base", "release-2", "release-2"},
		{"default base", "", "main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &foremanv1alpha1.AgenticTask{
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
					Payload: foremanv1alpha1.AgenticTaskPayload{
						Repo:       "defilantech/LLMKube",
						Issue:      813,
						BaseBranch: tc.base,
					},
				},
			}
			raw := buildDeterministicArgs(task, "foreman/issue-813", "https://github.com/Defilan/LLMKube.git",
				upstreamURLForRepo(task.Spec.Payload.Repo))
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal args: %v", err)
			}
			if got, _ := m["baseBranch"].(string); got != tc.want {
				t.Errorf("args[baseBranch] = %q, want %q", got, tc.want)
			}
		})
	}
}

// recordingCodeHost answers with a non-GitHub URL so a github.com result
// proves the injected seam was bypassed.
type recordingCodeHost struct{ asked []string }

func (r *recordingCodeHost) ResolveCloneURL(repoSlug string) string {
	r.asked = append(r.asked, repoSlug)
	return "https://forge.example.org/" + repoSlug + ".git"
}

func (r *recordingCodeHost) EnsureChangeRequest(
	context.Context, string, string, string, string, string, bool,
) (string, bool, error) {
	return "", false, nil
}

func (r *recordingCodeHost) PullRequestUpdate(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (r *recordingCodeHost) HeadCommitSubject(context.Context, string, string) (string, error) {
	return "", nil
}

// TestResolveUpstreamPrefersInjectedCodeHost pins the clone-URL half of #1298.
// Execute used to bind resolveUpstream to upstreamURLForRepo, which reads a
// package-level GitHub resolver, so an injected Forgejo/GitLab CodeHost
// compiled, satisfied the interface, was injected, and still produced
// https://github.com/<slug>.git.
//
// It calls the executor's own resolver, which Execute and every later run
// path use: injected seam wins, package default otherwise.
func TestResolveUpstreamPrefersInjectedCodeHost(t *testing.T) {
	ch := &recordingCodeHost{}
	e := &NativeAgentLoopExecutor{CodeHost: ch}
	resolve := e.upstreamURL

	got := resolve("group/sub/project")
	if want := "https://forge.example.org/group/sub/project.git"; got != want {
		t.Errorf("clone URL = %q, want %q (injected CodeHost bypassed)", got, want)
	}
	if len(ch.asked) != 1 {
		t.Errorf("injected CodeHost was not consulted: %v", ch.asked)
	}

	// With no seam injected the GitHub default must be unchanged.
	e.CodeHost = nil
	if got := resolve("owner/name"); got != "https://github.com/owner/name.git" {
		t.Errorf("default path changed: %q", got)
	}
}

// The paths after the clone (the envtest and scan gates, the self-commit and
// claim-evidence base SHAs, the reviewer diff) resolve the upstream through
// resolveUpstreamForRun, which read the package GitHub default and ignored the
// injected CodeHost (#1298).
func TestResolveUpstreamForRunUsesInjectedCodeHost(t *testing.T) {
	ch := &recordingCodeHost{}
	e := &NativeAgentLoopExecutor{CodeHost: ch}
	task := &foremanv1alpha1.AgenticTask{Spec: foremanv1alpha1.AgenticTaskSpec{
		Payload: foremanv1alpha1.AgenticTaskPayload{Repo: "group/sub/project"},
	}}
	if got, want := e.resolveUpstreamForRun(task), "https://forge.example.org/group/sub/project.git"; got != want {
		t.Errorf("resolveUpstreamForRun = %q, want %q (injected CodeHost bypassed)", got, want)
	}

	e.UpstreamURLForRepo = func(string) string { return "https://override.example/x.git" }
	if got := e.resolveUpstreamForRun(task); got != "https://override.example/x.git" {
		t.Errorf("resolveUpstreamForRun = %q, want the test override to win", got)
	}
}

// argsRegistry records the args of the one tool a deterministic run
// dispatches.
type argsRegistry struct{ args json.RawMessage }

func (r *argsRegistry) Schemas() []oai.Tool { return nil }

func (r *argsRegistry) Dispatch(_ context.Context, _ string, args json.RawMessage) (*ToolResult, error) {
	r.args = args
	return &ToolResult{Output: map[string]any{}}, nil
}

// A deterministic gate run hands the gate the upstream URL its bite check
// fetches the base from. buildDeterministicArgs built it from the package
// GitHub default, so a gate behind an injected CodeHost fetched from
// github.com (#1298).
func TestExecuteDeterministicPassesCodeHostUpstream(t *testing.T) {
	reg := &argsRegistry{}
	e := &NativeAgentLoopExecutor{CodeHost: &recordingCodeHost{}}
	task := &foremanv1alpha1.AgenticTask{Spec: foremanv1alpha1.AgenticTaskSpec{
		Kind:    foremanv1alpha1.AgenticTaskKindIssueFix,
		Payload: foremanv1alpha1.AgenticTaskPayload{Repo: "group/sub/project", Issue: 1},
	}}
	ag := &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{Tools: []string{"run_gate_job"}}}
	e.executeDeterministic(context.Background(), task, ag, "foreman/issue-1", reg, "", time.Now())

	var m map[string]any
	if err := json.Unmarshal(reg.args, &m); err != nil {
		t.Fatalf("dispatched args: %v (%s)", err, reg.args)
	}
	if got, want := m["upstreamURL"], "https://forge.example.org/group/sub/project.git"; got != want {
		t.Errorf("args[upstreamURL] = %v, want %q from the injected CodeHost", got, want)
	}
}
