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
	commitFunc   func(ctx context.Context, owner, repo, ref, token string) string
}

func (f *fakeEnsurer) EnsurePR(ctx context.Context, owner, repo, head,
	base, title, body, token string) (*githubpr.Result, error) {
	if f.ensurePRFunc != nil {
		return f.ensurePRFunc(ctx, owner, repo, head, base, title, body, token)
	}
	return nil, nil
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
			name: "valid multi-segment slug (GitLab subgroup)",
			slug: "group/subgroup/project",
			want: "https://github.com/group/subgroup/project.git",
		},
		{
			name: "valid deep multi-segment slug",
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
			name: "slug with trailing whitespace is trimmed",
			slug: "defilantech/llmkube ",
			want: "https://github.com/defilantech/llmkube.git",
		},
		{
			name: "slug with leading whitespace",
			slug: "  defilantech/llmkube",
			want: "https://github.com/defilantech/llmkube.git",
		},
		{
			name: "path traversal rejected (.. segment)",
			slug: "group/../project",
			want: "",
		},
		{
			name: "path traversal rejected (.. in middle)",
			slug: "a/b/../c",
			want: "",
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &GitHubCodeHost{
				Ensurer: &fakeEnsurer{
					ensurePRFunc: tc.ensurePR,
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

func TestRepoSlugValid(t *testing.T) {
	tests := []struct {
		name string
		slug string
		want bool
	}{
		{
			name: "valid two-segment slug",
			slug: "defilantech/llmkube",
			want: true,
		},
		{
			name: "valid multi-segment slug",
			slug: "group/subgroup/project",
			want: true,
		},
		{
			name: "valid deep multi-segment slug",
			slug: "a/b/c/d",
			want: true,
		},
		{
			name: "valid slug with dots and hyphens",
			slug: "my-org/my-repo-name",
			want: true,
		},
		{
			name: "empty slug rejected",
			slug: "",
			want: false,
		},
		{
			name: "no slash rejected",
			slug: "defilantech",
			want: false,
		},
		{
			name: "path traversal rejected (.. segment)",
			slug: "group/../project",
			want: false,
		},
		{
			name: "path traversal rejected (.. in middle)",
			slug: "a/b/../c",
			want: false,
		},
		{
			name: "whitespace rejected",
			slug: "owner/ repo",
			want: false,
		},
		{
			name: "absolute path rejected",
			slug: "/owner/repo",
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RepoSlugValid(tc.slug)
			if got != tc.want {
				t.Errorf("RepoSlugValid(%q) = %v, want %v", tc.slug, got, tc.want)
			}
		})
	}
}

func TestSplitRepoSlug(t *testing.T) {
	tests := []struct {
		name  string
		slug  string
		wantO string
		wantN string
	}{
		{
			name:  "two-segment slug",
			slug:  "defilantech/llmkube",
			wantO: "defilantech",
			wantN: "llmkube",
		},
		{
			name:  "three-segment slug (GitLab subgroup)",
			slug:  "group/subgroup/project",
			wantO: "group/subgroup",
			wantN: "project",
		},
		{
			name:  "four-segment slug",
			slug:  "a/b/c/d",
			wantO: "a/b/c",
			wantN: "d",
		},
		{
			name:  "no slash returns empty",
			slug:  "noslash",
			wantO: "",
			wantN: "",
		},
		{
			name:  "leading slash returns empty owner",
			slug:  "/project",
			wantO: "",
			wantN: "project",
		},
		{
			name:  "trailing slash returns empty name",
			slug:  "owner/",
			wantO: "owner",
			wantN: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, n := SplitRepoSlug(tc.slug)
			if o != tc.wantO || n != tc.wantN {
				t.Errorf("SplitRepoSlug(%q) = (%q, %q), want (%q, %q)",
					tc.slug, o, n, tc.wantO, tc.wantN)
			}
		})
	}
}
