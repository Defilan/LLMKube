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
	"errors"
	"strings"
	"testing"
)

type fakeProbe struct {
	pr        string
	prErr     error
	branch    bool
	branchErr error
}

func (f fakeProbe) OpenPRForIssue(_ context.Context, _ string, _ int32) (string, error) {
	return f.pr, f.prErr
}
func (f fakeProbe) BranchExists(_ context.Context, _, _ string) (bool, error) {
	return f.branch, f.branchErr
}

const (
	preflightBranch = "foreman/wl/issue-1602"
	preflightPRURL  = "https://github.com/x/y/pull/9"
)

func TestPreflight(t *testing.T) {
	item := QueueItem{Issue: 1602, Repo: "defilantech/LLMKube", IntentPath: "x.md"}
	cases := []struct {
		name     string
		probe    fakeProbe
		wantSkip string
		// wantDetail is the identifier the human needs in order to go look
		// at what already exists. A reason that omits it is not actionable.
		wantDetail string
	}{
		{"clean issue proceeds", fakeProbe{}, "", ""},
		{"open PR skips", fakeProbe{pr: preflightPRURL}, "open PR", preflightPRURL},
		{"existing branch skips", fakeProbe{branch: true}, "branch", preflightBranch},
		// The open PR is decided before the branch is probed, so a broken
		// branch probe cannot turn a settled skip into a run-ending error.
		{"open PR short circuits a broken branch probe",
			fakeProbe{pr: preflightPRURL, branchErr: errors.New("api down")},
			"open PR", preflightPRURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Preflight(context.Background(), tc.probe, item, preflightBranch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantSkip == "" {
				if got != "" {
					t.Errorf("skipReason = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSkip) {
				t.Errorf("skipReason = %q, want it to mention %q", got, tc.wantSkip)
			}
			if !strings.Contains(got, tc.wantDetail) {
				t.Errorf("skipReason = %q, want it to name %q", got, tc.wantDetail)
			}
		})
	}
}

// Preflight must fail closed: a probe error is not "no open PR". Dispatching
// on a failed lookup risks duplicating work already in flight.
func TestPreflight_ProbeErrorIsAnErrorNotAGreenLight(t *testing.T) {
	item := QueueItem{Issue: 1602, Repo: "defilantech/LLMKube", IntentPath: "x.md"}
	_, err := Preflight(context.Background(),
		fakeProbe{prErr: errors.New("api down")}, item, preflightBranch)
	if err == nil {
		t.Fatal("want an error when the PR probe fails")
	}
}

// The branch probe fails closed for the same reason: a lookup that did not
// answer is not an answer of "no such branch".
func TestPreflight_BranchProbeErrorIsAnErrorNotAGreenLight(t *testing.T) {
	item := QueueItem{Issue: 1602, Repo: "defilantech/LLMKube", IntentPath: "x.md"}
	_, err := Preflight(context.Background(),
		fakeProbe{branchErr: errors.New("api down")}, item, preflightBranch)
	if err == nil {
		t.Fatal("want an error when the branch probe fails")
	}
}
