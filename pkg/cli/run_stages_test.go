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

import "testing"

func TestNextStage(t *testing.T) {
	cases := []struct {
		name     string
		cur      Stage
		facts    Facts
		wantNext Stage
		wantPark string
	}{
		{"preflight skip ends the item", StagePreflight,
			Facts{SkipReason: "open PR #1234 references it"}, StageDone, ""},
		{"preflight clean dispatches", StagePreflight, Facts{}, StageDispatch, ""},
		{"dispatch always watches", StageDispatch, Facts{}, StageWatch, ""},
		{"a stalled run parks for escalation", StageWatch,
			Facts{Stalled: true}, StageParked, "escalate"},
		{"a terminal run verifies", StageWatch, Facts{}, StageVerify, ""},
		{"clean verify on the first attempt finalizes", StageVerify,
			Facts{VerifyClean: true, Attempts: 1}, StageFinalize, ""},
		{"dirty verify on the first attempt parks to adjudicate", StageVerify,
			Facts{VerifyClean: false, Attempts: 1}, StageParked, "adjudicate"},
		{"clean verify after the one feedback pass finalizes", StageVerify,
			Facts{VerifyClean: true, Attempts: 2}, StageFinalize, ""},
		{"finalize ends the item", StageFinalize, Facts{}, StageDone, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NextStage(tc.cur, tc.facts)
			if got.Next != tc.wantNext {
				t.Errorf("Next = %q, want %q", got.Next, tc.wantNext)
			}
			if got.Park != tc.wantPark {
				t.Errorf("Park = %q, want %q", got.Park, tc.wantPark)
			}
		})
	}
}

// The one-pass rule is the invariant, not a convention: after the single
// feedback attempt a dirty verify must escalate, never revise again.
func TestNextStage_NeverMakesASecondFeedbackAttempt(t *testing.T) {
	got := NextStage(StageVerify, Facts{VerifyClean: false, Attempts: 2})
	if got.Next != StageParked || got.Park != "escalate" {
		t.Fatalf("second dirty verify = (%q,%q), want (parked,escalate)", got.Next, got.Park)
	}
	// And no attempt count beyond that must always escalate, never adjudicate.
	for n := 3; n < 10; n++ {
		g := NextStage(StageVerify, Facts{VerifyClean: false, Attempts: n})
		if g.Next != StageParked || g.Park != "escalate" {
			t.Fatalf("attempts=%d = (%q,%q), want (parked,escalate)", n, g.Next, g.Park)
		}
	}
}
