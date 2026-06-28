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

package repo

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// BranchPrefix is the namespace under which foreman branches live.
// Keeping it consistent makes it trivial to write a fork-side cleanup
// job that prunes abandoned foreman branches.
const BranchPrefix = "foreman"

// maxSlugLen caps the issue-title slug inside the branch name so the
// branch fits comfortably in dashboards + git refs.
const maxSlugLen = 32

// BranchNameForIssue returns "foreman/issue-<N>-<slug>" where slug is
// the lowercase kebab-cased issue title, truncated to 32 chars and
// stripped of trailing dashes. The shape mirrors the autofix pipeline's
// convention so reviewers recognize foreman-authored branches at a
// glance.
func BranchNameForIssue(issueNumber int, issueTitle string) string {
	slug := slugify(issueTitle, maxSlugLen)
	if slug == "" {
		// Defensive: empty title still produces a stable branch name.
		return fmt.Sprintf("%s/issue-%d", BranchPrefix, issueNumber)
	}
	return fmt.Sprintf("%s/issue-%d-%s", BranchPrefix, issueNumber, slug)
}

// slugify lowercases s, replaces non-alphanumeric runs with a single
// '-', trims dashes from the ends, and truncates to maxLen.
func slugify(s string, maxLen int) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	return out
}

// CreateAndCheckoutBranch creates a new branch at HEAD and switches to
// it. Equivalent to "git checkout -b <branch>".
func CreateAndCheckoutBranch(ctx context.Context, workspace, branch string) error {
	if branch == "" {
		return fmt.Errorf("CreateAndCheckoutBranch: branch is required")
	}
	if _, err := runGit(ctx, workspace, baseEnv(), "checkout", "-b", branch); err != nil {
		return err
	}
	return nil
}

// UpstreamBranchOptions configures CreateBranchFromUpstream.
type UpstreamBranchOptions struct {
	// Workspace is the cloned fork working tree to create the branch in.
	Workspace string
	// Branch is the new task branch name.
	Branch string
	// UpstreamURL is the upstream project's git URL (e.g.
	// https://github.com/<owner>/<name>.git). The base ref is fetched from here.
	UpstreamURL string
	// BaseBranch is the upstream ref to branch from. Empty defaults to "main".
	BaseBranch string
	// Auth, when non-nil, provides the GIT_ASKPASS scaffolding for the fetch.
	// A public upstream can leave it nil.
	Auth *Auth
}

// CreateBranchFromUpstream fetches BaseBranch from UpstreamURL and creates
// Branch at that fetched tip, so the task branch is cut from the CURRENT
// upstream base regardless of the cloned fork's sync state (#813). The fork
// stays origin (the branch still pushes there for the PR); only the base moves
// to upstream. The fetched tip is referenced via FETCH_HEAD, so no extra
// remote is left in the workspace and the helper is safe to re-run.
func CreateBranchFromUpstream(ctx context.Context, opts UpstreamBranchOptions) error {
	if opts.Workspace == "" {
		return fmt.Errorf("CreateBranchFromUpstream: Workspace is required")
	}
	if opts.Branch == "" {
		return fmt.Errorf("CreateBranchFromUpstream: Branch is required")
	}
	if opts.UpstreamURL == "" {
		return fmt.Errorf("CreateBranchFromUpstream: UpstreamURL is required")
	}
	base := opts.BaseBranch
	if base == "" {
		base = "main"
	}

	env := baseEnv()
	if opts.Auth != nil {
		env = append(env, opts.Auth.Env()...)
	}

	// Fetch the current upstream base into FETCH_HEAD. The fork clone is full
	// (no --depth), so the new branch carries upstream history.
	if _, err := runGit(ctx, opts.Workspace, env, "fetch", opts.UpstreamURL, base); err != nil {
		return fmt.Errorf("CreateBranchFromUpstream: fetch %s %s: %w", opts.UpstreamURL, base, err)
	}

	// Create (or reset) the task branch at the fetched upstream tip and switch
	// to it. -B is used so the helper is idempotent.
	if _, err := runGit(ctx, opts.Workspace, baseEnv(), "checkout", "-B", opts.Branch, "FETCH_HEAD"); err != nil {
		return fmt.Errorf("CreateBranchFromUpstream: checkout -B %s FETCH_HEAD: %w", opts.Branch, err)
	}
	return nil
}

// baseEnv is the minimal env for read/local-only git ops that do not
// need GIT_ASKPASS (branch, status, log). HOME is carried through so
// git can read ~/.gitconfig if present.
func baseEnv() []string {
	return []string{"HOME=" + envOr("HOME", "/tmp")}
}
