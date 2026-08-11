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

// Package githubpr opens the pull request a completed Workload earned.
// The pipeline used to end one hop short of its artifact: a coder-GO,
// gate-PASS, review-GO branch sat on the remote, reviewed and unopened,
// until a human found it (#937). EnsurePR closes that hop, idempotently.
package githubpr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds each API call.
const DefaultTimeout = 10 * time.Second

// Result reports what EnsurePR did.
type Result struct {
	// URL is the pull request's html_url (existing or just created).
	URL string
	// Created is false when a PR for the head branch already existed
	// (including losing a create race to a concurrent reviewer task).
	Created bool
}

// prInfo carries the data returned by findByHead.
type prInfo struct {
	Number  int
	HTMLURL string
	State   string // "open" or "closed"
}

// Ensurer is the interface the executor consumes; tests substitute it.
type Ensurer interface {
	// EnsurePR's head is a bare branch name for a same-repo PR, or a
	// "forkOwner:branch" qualified head for a cross-fork PR (the coder
	// pushed to a fork of owner/repo). owner/repo always name the base.
	EnsurePR(ctx context.Context, owner, repo, head, base, title, body, token string) (*Result, error)
	// HeadCommitSubject returns the branch head's commit subject for use
	// as the PR title; "" on any failure (callers fall back).
	HeadCommitSubject(ctx context.Context, owner, repo, ref, token string) string
}

// Client is the production Ensurer. BaseURL is overridable so tests can
// point at an httptest server; production leaves it empty and the
// client uses https://api.github.com.
type Client struct {
	HTTPClient *http.Client
	BaseURL    string // empty defaults to https://api.github.com
}

// NewClient constructs a Client with a 10s timeout.
func NewClient() *Client {
	return &Client{HTTPClient: &http.Client{Timeout: DefaultTimeout}}
}

// EnsurePR makes sure a pull request exists for head → base, creating
// it if absent. Idempotent: an existing open PR for the head branch is
// returned rather than duplicated, and losing a create race (GitHub 422
// "already exists") resolves to the winner's PR. A closed unmerged PR
// is reopened; if reopen fails, a new PR is created.
func (c *Client) EnsurePR(
	ctx context.Context, owner, repo, head, base, title, body, token string,
) (*Result, error) {
	if owner == "" || repo == "" || head == "" || base == "" {
		return nil, fmt.Errorf("githubpr: owner, repo, head, and base are required")
	}
	if title == "" {
		return nil, fmt.Errorf("githubpr: title is required")
	}

	if existing, err := c.findByHead(ctx, owner, repo, head, token); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.State == "open" {
			return &Result{URL: existing.HTMLURL, Created: false}, nil
		}
		// Closed unmerged PR: try to reopen it.
		reopened, reopenErr := c.reopen(ctx, owner, repo, existing.Number, token)
		if reopenErr == nil {
			return &Result{URL: reopened, Created: false}, nil
		}
		// Reopen failed (e.g. branch deleted); fall through to create.
	}

	created, err := c.create(ctx, owner, repo, head, base, title, body, token)
	if err == nil {
		return &Result{URL: created, Created: true}, nil
	}
	// Create race: a concurrent reviewer task (multiple reviewers GO on
	// the same Workload) created it between our list and our POST.
	// GitHub answers 422; resolve to the winner's PR.
	var httpErr *HTTPError
	if isAlreadyExists(err, &httpErr) {
		if existing, findErr := c.findByHead(ctx, owner, repo, head, token); findErr == nil && existing != nil {
			return &Result{URL: existing.HTMLURL, Created: false}, nil
		}
	}
	return nil, err
}

// HeadCommitSubject returns the first line of the branch head's commit
// message — the natural PR title (the coder writes a conventional
// subject). Empty on any failure so callers can fall back.
func (c *Client) HeadCommitSubject(ctx context.Context, owner, repo, ref, token string) string {
	target := fmt.Sprintf("%s/repos/%s/%s/commits/%s", c.base(), owner, repo, ref)
	var out struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := c.getJSON(ctx, target, token, &out); err != nil {
		return ""
	}
	subject, _, _ := strings.Cut(out.Commit.Message, "\n")
	return strings.TrimSpace(subject)
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.github.com"
}

func (c *Client) findByHead(ctx context.Context, owner, repo, head, token string) (*prInfo, error) {
	// A cross-fork head arrives pre-qualified ("forkOwner:branch") and is
	// used verbatim; a same-repo head is qualified with the base owner so
	// the filter cannot match another fork's identically-named branch.
	filter := head
	if !strings.Contains(head, ":") {
		filter = owner + ":" + head
	}
	target := fmt.Sprintf("%s/repos/%s/%s/pulls?state=all&head=%s",
		c.base(), owner, repo, url.QueryEscape(filter))
	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
	}
	if err := c.getJSON(ctx, target, token, &prs); err != nil {
		return nil, fmt.Errorf("githubpr: list by head: %w", err)
	}
	// state=all carries no open-first ordering guarantee; when a closed
	// PR and an open one share the head, the open one is the artifact.
	for _, pr := range prs {
		if pr.State == "open" {
			return &prInfo{Number: pr.Number, HTMLURL: pr.HTMLURL, State: "open"}, nil
		}
	}
	if len(prs) == 0 {
		return nil, nil
	}
	// Only closed PRs exist for this head; return the first one so
	// EnsurePR can attempt to reopen it.
	return &prInfo{Number: prs[0].Number, HTMLURL: prs[0].HTMLURL, State: "closed"}, nil
}

func (c *Client) create(ctx context.Context, owner, repo, head, base, title, body, token string) (string, error) {
	target := fmt.Sprintf("%s/repos/%s/%s/pulls", c.base(), owner, repo)
	payload, err := json.Marshal(map[string]string{
		"title": title, "head": head, "base": base, "body": body,
	})
	if err != nil {
		return "", fmt.Errorf("githubpr: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("githubpr: build request: %w", err)
	}
	c.headers(req, token)
	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("githubpr: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("githubpr: read body: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", &HTTPError{StatusCode: resp.StatusCode, Body: string(raw)}
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		return "", fmt.Errorf("githubpr: decode created PR: %w", err)
	}
	return pr.HTMLURL, nil
}

// reopen sends a PATCH to GitHub to reopen a closed PR and returns its
// html_url. On any failure it returns an error so the caller can fall
// through to create a new PR.
func (c *Client) reopen(ctx context.Context, owner, repo string, number int, token string) (string, error) {
	target := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.base(), owner, repo, number)
	payload, err := json.Marshal(map[string]string{"state": "open"})
	if err != nil {
		return "", fmt.Errorf("githubpr: marshal reopen: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, target, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("githubpr: build reopen request: %w", err)
	}
	c.headers(req, token)
	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("githubpr: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("githubpr: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &HTTPError{StatusCode: resp.StatusCode, Body: string(raw)}
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		return "", fmt.Errorf("githubpr: decode reopened PR: %w", err)
	}
	return pr.HTMLURL, nil
}

func (c *Client) getJSON(ctx context.Context, target, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	c.headers(req, token)
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(raw)}
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) headers(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if req.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
}

func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// HTTPError carries a non-2xx response so callers can distinguish
// validation failures (422) from auth (401/403) and transport errors.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("githubpr: HTTP %d: %s", e.StatusCode, e.Body)
}

// isAlreadyExists reports whether err is GitHub's 422 for "a pull
// request already exists" (the create race).
func isAlreadyExists(err error, target **HTTPError) bool {
	if !asHTTPError(err, target) {
		return false
	}
	return (*target).StatusCode == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower((*target).Body), "already exists")
}

func asHTTPError(err error, target **HTTPError) bool {
	e, ok := err.(*HTTPError) //nolint:errorlint // constructed locally, never wrapped
	if ok {
		*target = e
	}
	return ok
}
