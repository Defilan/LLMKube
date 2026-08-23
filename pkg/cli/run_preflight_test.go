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

const (
	preflightRepo   = "defilantech/LLMKube"
	preflightBranch = "foreman/wl/issue-1602"
	preflightPRURL  = "https://github.com/x/y/pull/9"
)

const preflightIssue = int32(1602)

// errProbeDown is the transport failure a probe reports when the forge is
// unreachable. Preflight must wrap it rather than flatten it, so a caller can
// tell a forge outage from a policy decision.
var errProbeDown = errors.New("api down")

func preflightItem() QueueItem {
	return QueueItem{Issue: preflightIssue, Repo: preflightRepo, IntentPath: "x.md"}
}

// fakeProbe answers only when Preflight forwards the arguments it expects. A
// mismatch yields the zero value with a nil error, which is what a real forge
// returns when asked about a repo, issue, or branch that does not exist. That
// makes argument forwarding observable through Preflight's return value: a
// dropped or blanked argument turns a skip into a proceed, which the ordinary
// assertions already catch. No call recording, and nothing asserts on the
// fake's own state.
type fakeProbe struct {
	wantRepo   string
	wantIssue  int32
	wantBranch string

	pr        string
	prErr     error
	branch    bool
	branchErr error
}

// probeExpecting fills in the arguments Preflight must forward, so each case
// declares only the answers it wants back.
func probeExpecting(answers fakeProbe) fakeProbe {
	answers.wantRepo = preflightRepo
	answers.wantIssue = preflightIssue
	answers.wantBranch = preflightBranch
	return answers
}

func (f fakeProbe) OpenPRForIssue(ctx context.Context, repoSlug string, issue int32) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if repoSlug != f.wantRepo || issue != f.wantIssue {
		return "", nil
	}
	return f.pr, f.prErr
}

func (f fakeProbe) BranchExists(ctx context.Context, repoSlug, branch string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if repoSlug != f.wantRepo || branch != f.wantBranch {
		return false, nil
	}
	return f.branch, f.branchErr
}

func TestPreflight(t *testing.T) {
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
			fakeProbe{pr: preflightPRURL, branchErr: errProbeDown},
			"open PR", preflightPRURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Preflight(context.Background(), probeExpecting(tc.probe),
				preflightItem(), preflightBranch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantSkip == "" {
				if got != "" {
					t.Errorf("skipReason = %q, want empty", got)
				}
				return
			}
			// Guard against a future edit reintroducing a vacuous case:
			// strings.Contains is trivially true for an empty needle.
			if tc.wantDetail == "" {
				t.Fatal("test bug: a skip case must name the detail its reason has to carry")
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
	got, err := Preflight(context.Background(),
		probeExpecting(fakeProbe{prErr: errProbeDown}), preflightItem(), preflightBranch)
	if err == nil {
		t.Fatal("want an error when the PR probe fails")
	}
	if !errors.Is(err, errProbeDown) {
		t.Errorf("err = %v, want it to wrap the probe's own error", err)
	}
	// A caller that logs the reason before checking err would otherwise
	// report a skip that never happened.
	if got != "" {
		t.Errorf("skipReason = %q, want empty alongside an error", got)
	}
}

// The branch probe fails closed for the same reason: a lookup that did not
// answer is not an answer of "no such branch".
func TestPreflight_BranchProbeErrorIsAnErrorNotAGreenLight(t *testing.T) {
	got, err := Preflight(context.Background(),
		probeExpecting(fakeProbe{branchErr: errProbeDown}), preflightItem(), preflightBranch)
	if err == nil {
		t.Fatal("want an error when the branch probe fails")
	}
	if !errors.Is(err, errProbeDown) {
		t.Errorf("err = %v, want it to wrap the probe's own error", err)
	}
	if got != "" {
		t.Errorf("skipReason = %q, want empty alongside an error", got)
	}
}

// Probes must run on the caller's context. The run loop cancels on its stall
// deadline, and a probe handed context.Background() outlives the run it
// belongs to.
func TestPreflight_ProbesRunOnTheCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Answers that would otherwise be a clean proceed, so cancellation
	// reaching the probe is the only thing that can produce an error.
	got, err := Preflight(ctx, probeExpecting(fakeProbe{}), preflightItem(), preflightBranch)
	if err == nil {
		t.Fatal("want the caller's cancellation to reach the probe")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if got != "" {
		t.Errorf("skipReason = %q, want empty alongside an error", got)
	}
}
