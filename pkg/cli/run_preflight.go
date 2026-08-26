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

package cli

import (
	"context"
	"fmt"
)

// PreflightProbe is the forge-side reads preflight needs. Kept as an
// interface so the loop is testable without network and so a non-GitHub
// forge can satisfy it later (#1158).
//
// Implementations must return an error, never a zero value, when the repo
// slug is empty or unresolvable or the query otherwise could not be answered.
// Preflight reads a zero value as "nothing found" and dispatches, so an
// implementation that folds a bad slug into ("", nil) or (false, nil)
// silently defeats the fail-closed guarantee below: an empty slug that
// answers "no open PR" is how #1625 duplicated work already in flight.
// Preflight will not hand an implementation an empty slug or branch, but
// implementations still owe this for every other unresolvable query.
type PreflightProbe interface {
	// OpenPRForIssue returns the URL of an open PR referencing the issue,
	// or "" when there is none.
	OpenPRForIssue(ctx context.Context, repoSlug string, issue int32) (string, error)
	// BranchExists reports whether branch exists on the push remote.
	BranchExists(ctx context.Context, repoSlug, branch string) (bool, error)
}

// Preflight returns a non-empty skip reason when the item should not be
// dispatched.
//
// The item's repo slug and the branch must both be non-empty, and Preflight
// enforces that before making any forge call: an empty slug or branch probes
// nothing, and "nothing found" reads as a green light to dispatch (#1625).
//
// It fails CLOSED: a probe error is returned as an error, never treated as
// "nothing found", because dispatching on a failed lookup risks duplicating
// work already in flight.
func Preflight(ctx context.Context, p PreflightProbe, item QueueItem, branch string) (string, error) {
	if item.Repo == "" {
		return "", fmt.Errorf("preflight: issue %d has no repo slug", item.Issue)
	}
	if branch == "" {
		return "", fmt.Errorf("preflight: issue %d has no branch", item.Issue)
	}
	url, err := p.OpenPRForIssue(ctx, item.Repo, item.Issue)
	if err != nil {
		return "", fmt.Errorf("preflight: open-PR probe for issue %d: %w", item.Issue, err)
	}
	if url != "" {
		return fmt.Sprintf("an open PR already references it: %s", url), nil
	}
	exists, err := p.BranchExists(ctx, item.Repo, branch)
	if err != nil {
		return "", fmt.Errorf("preflight: branch probe for %s: %w", branch, err)
	}
	if exists {
		return fmt.Sprintf("branch %s already exists", branch), nil
	}
	return "", nil
}
