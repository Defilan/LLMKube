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

package codehost

import (
	"context"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/githubpr"
)

// fakeEnsurer is a minimal githubpr.Ensurer implementation for tests.
type fakeEnsurer struct {
	ensurePRFunc func(ctx context.Context, owner, repo, head, base, title, body, token string) (*githubpr.Result, error)
	updatePRFunc func(ctx context.Context, owner, repo, head, body, token string) (string, error)
	commitFunc   func(ctx context.Context, owner, repo, ref, token string) string
}

func (f *fakeEnsurer) EnsurePR(ctx context.Context, owner, repo, head,
	base, title, body, token string) (*githubpr.Result, error) {
	if f.ensurePRFunc != nil {
		return f.ensurePRFunc(ctx, owner, repo, head, base, title, body, token)
	}
	return nil, nil
}

func (f *fakeEnsurer) UpdatePR(ctx context.Context, owner, repo, head, body, token string) (string, error) {
	if f.updatePRFunc != nil {
		return f.updatePRFunc(ctx, owner, repo, head, body, token)
	}
	return "", nil
}

func (f *fakeEnsurer) HeadCommitSubject(ctx context.Context, owner, repo, ref, token string) string {
	if f.commitFunc != nil {
		return f.commitFunc(ctx, owner, repo, ref, token)
	}
	return ""
}

func TestNewGitHubCodeHost(t *testing.T) {
	fake := &fakeEnsurer{}
	g := NewGitHubCodeHost(fake)
	if g == nil {
		t.Fatal("NewGitHubCodeHost returned nil")
	}
	if g.Ensurer != fake {
		t.Errorf("NewGitHubCodeHost.Ensurer = %v, want %v", g.Ensurer, fake)
	}
}

func TestResolveCloneURL(t *testing.T) {
	g := &GitHubCodeHost{}

	tests := []struct {
		name string
		slug string
		want string
	}{
		{
			name: "valid slug",
			slug: "defilantech/llmkube",
			want: "https://github.com/defilantech/llmkube.git",
		},
		{
			name: "valid slug with dots and hyphens",
			slug: "my-org/my-repo-name",
			want: "https://github.com/my-org/my-repo-name.git",
		},
		{
			name: "multi-segment slug (GitLab subgroup)",
			slug: "group/subgroup/project",
			want: "https://github.com/group/subgroup/project.git",
		},
		{
			name: "deeply nested slug",
			slug: "a/b/c/d",
			want: "https://github.com/a/b/c/d.git",
		},
		{
			name: "empty slug",
			slug: "",
			want: "",
		},
		{
			name: "malformed slug - no slash",
			slug: "defilantech",
			want: "",
		},
		{
			name: "malformed slug - path traversal",
			slug: "defilantech/llmkube/..",
			want: "",
		},
		{
			name: "malformed slug - leading dot segment",
			slug: "../llmkube",
			want: "",
		},
		{
			name: "malformed slug - empty segment",
			slug: "defilantech//llmkube",
			want: "",
		},
		{
			name: "malformed slug - trailing slash",
			slug: "defilantech/llmkube/",
			want: "",
		},
		{
			name: "malformed slug - leading slash",
			slug: "/defilantech/llmkube",
			want: "",
		},
		{
			name: "malformed slug - whitespace in segment",
			slug: "defilantech/llm kube",
			want: "",
		},
		{
			name: "slug with trailing whitespace is trimmed",
			slug: "defilantech/llmkube ",
			want: "https://github.com/defilantech/llmkube.git",
		},
		{
			name: "slug with leading whitespace",
			slug: "  defilantech/llmkube",
			want: "https://github.com/defilantech/llmkube.git",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := g.ResolveCloneURL(tc.slug)
			if got != tc.want {
				t.Errorf("ResolveCloneURL(%q) = %q, want %q", tc.slug, got, tc.want)
			}
		})
	}
}

func TestEnsureChangeRequest(t *testing.T) {
	tests := []struct {
		name        string
		repoSlug    string
		headBranch  string
		baseBranch  string
		title       string
		body        string
		ensurePR    func(ctx context.Context, owner, repo, head, base, title, body, token string) (*githubpr.Result, error)
		updatePR    func(ctx context.Context, owner, repo, head, body, token string) (string, error)
		wantURL     string
		wantCreated bool
		wantErr     bool
	}{
		{
			name:       "creates new PR",
			repoSlug:   "defilantech/llmkube",
			headBranch: "foreman/wl-x/issue-7",
			baseBranch: "main",
			title:      "Fix the thing",
			body:       "Fixes #7",
			ensurePR: func(ctx context.Context, owner, repo, head, base, title, body, token string) (*githubpr.Result, error) {
				return &githubpr.Result{URL: "https://github.com/defilantech/llmkube/pull/9", Created: true}, nil
			},
			wantURL:     "https://github.com/defilantech/llmkube/pull/9",
			wantCreated: true,
		},
		{
			name:       "reuses existing PR",
			repoSlug:   "defilantech/llmkube",
			headBranch: "foreman/wl-x/issue-7",
			baseBranch: "main",
			title:      "Fix the thing",
			body:       "Fixes #7",
			ensurePR: func(ctx context.Context, owner, repo, head, base, title, body, token string) (*githubpr.Result, error) {
				return &githubpr.Result{URL: "https://github.com/defilantech/llmkube/pull/4", Created: false}, nil
			},
			wantURL:     "https://github.com/defilantech/llmkube/pull/4",
			wantCreated: false,
		},
		{
			name:        "malformed repo slug returns empty",
			repoSlug:    "bad-slug",
			headBranch:  "foreman/wl-x/issue-7",
			baseBranch:  "main",
			title:       "Fix the thing",
			body:        "Fixes #7",
			ensurePR:    nil,
			wantURL:     "",
			wantCreated: false,
		},
		{
			name:       "multi-segment slug splits on last slash",
			repoSlug:   "group/subgroup/project",
			headBranch: "foreman/wl-x/issue-7",
			baseBranch: "main",
			title:      "Fix the thing",
			body:       "Fixes #7",
			ensurePR: func(ctx context.Context, owner, repo, head, base, title, body, token string) (*githubpr.Result, error) {
				if owner != "group/subgroup" || repo != "project" {
					t.Errorf("EnsurePR called with owner=%q repo=%q, want owner=%q repo=%q",
						owner, repo, "group/subgroup", "project")
				}
				return &githubpr.Result{URL: "https://github.com/group/subgroup/project/pull/1", Created: true}, nil
			},
			wantURL:     "https://github.com/group/subgroup/project/pull/1",
			wantCreated: true,
		},
		{
			name:       "refreshes body when PR already exists",
			repoSlug:   "defilantech/llmkube",
			headBranch: "foreman/wl-x/issue-7",
			baseBranch: "main",
			title:      "Fix the thing",
			body:       "Fixes #7",
			ensurePR: func(ctx context.Context, owner, repo, head, base, title, body, token string) (*githubpr.Result, error) {
				return &githubpr.Result{URL: "https://github.com/defilantech/llmkube/pull/4", Created: false}, nil
			},
			updatePR: func(ctx context.Context, owner, repo, head, b, token string) (string, error) {
				if b != "Fixes #7" {
					t.Errorf("UpdatePR body = %q, want %q", b, "Fixes #7")
				}
				return "https://github.com/defilantech/llmkube/pull/4", nil
			},
			wantURL:     "https://github.com/defilantech/llmkube/pull/4",
			wantCreated: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &GitHubCodeHost{
				Ensurer: &fakeEnsurer{
					ensurePRFunc: tc.ensurePR,
					updatePRFunc: tc.updatePR,
				},
			}

			url, created, err := g.EnsureChangeRequest(
				context.Background(), tc.repoSlug, tc.headBranch,
				tc.baseBranch, tc.title, tc.body)
			if (err != nil) != tc.wantErr {
				t.Errorf("EnsureChangeRequest() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
			if url != tc.wantURL {
				t.Errorf("EnsureChangeRequest() url = %q, want %q", url, tc.wantURL)
			}
			if created != tc.wantCreated {
				t.Errorf("EnsureChangeRequest() created = %v, want %v", created, tc.wantCreated)
			}
		})
	}
}

// TestPullRequestUpdate_CallsUpdatePR pins the #1567 refresh seam: the
// codehost forwards the body to the Ensurer's UpdatePR with the slug
// split into owner/repo, and a malformed slug is refused before it
// reaches the Ensurer.
func TestPullRequestUpdate_CallsUpdatePR(t *testing.T) {
	var gotOwner, gotRepo, gotHead, gotBody, gotToken string
	g := &GitHubCodeHost{
		Ensurer: &fakeEnsurer{
			updatePRFunc: func(_ context.Context, owner, repo, head, body, token string) (string, error) {
				gotOwner, gotRepo, gotHead, gotBody, gotToken = owner, repo, head, body, token
				return "https://github.com/defilantech/llmkube/pull/4", nil
			},
		},
		Token: "tok",
	}

	url, err := g.PullRequestUpdate(context.Background(),
		"defilantech/llmkube", "foreman/wl-x/issue-7", "Fixes #7 (revised)")
	if err != nil {
		t.Fatalf("PullRequestUpdate() error = %v", err)
	}
	if url != "https://github.com/defilantech/llmkube/pull/4" {
		t.Errorf("PullRequestUpdate() url = %q", url)
	}
	if gotOwner != "defilantech" || gotRepo != "llmkube" ||
		gotHead != "foreman/wl-x/issue-7" ||
		gotBody != "Fixes #7 (revised)" || gotToken != "tok" {
		t.Errorf("UpdatePR args wrong: owner=%q repo=%q head=%q body=%q token=%q",
			gotOwner, gotRepo, gotHead, gotBody, gotToken)
	}
}

func TestHeadCommitSubject(t *testing.T) {
	tests := []struct {
		name        string
		repoSlug    string
		headBranch  string
		commitFunc  func(ctx context.Context, owner, repo, ref, token string) string
		wantSubject string
	}{
		{
			name:       "valid subject",
			repoSlug:   "defilantech/llmkube",
			headBranch: "foreman/wl-x/issue-7",
			commitFunc: func(ctx context.Context, owner, repo, ref, token string) string {
				return "feat: add the thing"
			},
			wantSubject: "feat: add the thing",
		},
		{
			name:       "empty subject on failure",
			repoSlug:   "defilantech/llmkube",
			headBranch: "foreman/wl-x/issue-7",
			commitFunc: func(ctx context.Context, owner, repo, ref, token string) string {
				return ""
			},
			wantSubject: "",
		},
		{
			name:        "malformed repo slug returns empty",
			repoSlug:    "bad-slug",
			headBranch:  "foreman/wl-x/issue-7",
			commitFunc:  nil,
			wantSubject: "",
		},
		{
			name:       "multi-segment slug splits on last slash",
			repoSlug:   "group/subgroup/project",
			headBranch: "foreman/wl-x/issue-7",
			commitFunc: func(ctx context.Context, owner, repo, ref, token string) string {
				if owner != "group/subgroup" || repo != "project" {
					t.Errorf("HeadCommitSubject called with owner=%q repo=%q, want owner=%q repo=%q",
						owner, repo, "group/subgroup", "project")
				}
				return "feat: add the thing"
			},
			wantSubject: "feat: add the thing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &GitHubCodeHost{
				Ensurer: &fakeEnsurer{
					commitFunc: tc.commitFunc,
				},
			}

			subject, err := g.HeadCommitSubject(context.Background(), tc.repoSlug, tc.headBranch)
			if err != nil {
				t.Errorf("HeadCommitSubject() error = %v", err)
				return
			}
			if subject != tc.wantSubject {
				t.Errorf("HeadCommitSubject() = %q, want %q", subject, tc.wantSubject)
			}
		})
	}
}

func TestSplitRepoSlug(t *testing.T) {
	tests := []struct {
		name     string
		slug     string
		wantNS   string
		wantName string
		wantOK   bool
	}{
		{name: "owner/name", slug: "defilantech/llmkube", wantNS: "defilantech", wantName: "llmkube", wantOK: true},
		{name: "group/subgroup/project", slug: "group/subgroup/project",
			wantNS: "group/subgroup", wantName: "project", wantOK: true},
		{name: "deeply nested", slug: "a/b/c/d", wantNS: "a/b/c", wantName: "d", wantOK: true},
		{name: "no slash", slug: "defilantech", wantNS: "", wantName: "", wantOK: false},
		{name: "empty string", slug: "", wantNS: "", wantName: "", wantOK: false},
		{name: "trailing slash", slug: "defilantech/llmkube/", wantNS: "", wantName: "", wantOK: false},
		{name: "leading slash", slug: "/defilantech/llmkube", wantNS: "", wantName: "", wantOK: false},
		{name: "empty segment", slug: "defilantech//llmkube", wantNS: "", wantName: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns, name, ok := SplitRepoSlug(tc.slug)
			if ns != tc.wantNS || name != tc.wantName || ok != tc.wantOK {
				t.Errorf("SplitRepoSlug(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.slug, ns, name, ok, tc.wantNS, tc.wantName, tc.wantOK)
			}
		})
	}
}

func TestIsValidRepoSlug(t *testing.T) {
	tests := []struct {
		name string
		slug string
		want bool
	}{
		{"owner/name", "defilantech/llmkube", true},
		{"group/subgroup/project", "group/subgroup/project", true},
		{"deeply nested", "a/b/c/d", true},
		{"no slash", "defilantech", false},
		{"empty string", "", false},
		{"trailing slash", "defilantech/llmkube/", false},
		{"leading slash", "/defilantech/llmkube", false},
		{"empty segment", "defilantech//llmkube", false},
		{"path traversal", "defilantech/llmkube/..", false},
		{"leading dot segment", "../llmkube", false},
		{"whitespace in segment", "defilantech/llm kube", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsValidRepoSlug(tc.slug); got != tc.want {
				t.Errorf("IsValidRepoSlug(%q) = %v, want %v", tc.slug, got, tc.want)
			}
		})
	}
}
