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
	"fmt"
	"math"
	"testing"
	"time"
)

func TestIsStalled(t *testing.T) {
	base := 60 * time.Minute
	cases := []struct {
		name string
		in   StallInput
		want bool
	}{
		// The two real cases from the 2026-08-22 batch.
		{"1628: 4h, nothing pushed", StallInput{4 * time.Hour, base, false}, true},
		{"1601-revise: 3h51m, nothing pushed", StallInput{231 * time.Minute, base, false}, true},
		// The contrast case that must NOT trip: slow but productive.
		{"slow run that pushed a branch", StallInput{4 * time.Hour, base, true}, false},
		{"under the factor", StallInput{2 * time.Hour, base, false}, false},
		{"exactly at the factor is not yet stalled", StallInput{150 * time.Minute, base, false}, false},
		{"just past the factor", StallInput{151 * time.Minute, base, false}, true},
		{"zero baseline falls back to the default", StallInput{4 * time.Hour, 0, false}, true},
		{"zero baseline uses the default threshold, not zero", StallInput{100 * time.Minute, 0, false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStalled(tc.in, DefaultStallFactor); got != tc.want {
				t.Errorf("IsStalled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// A factor that is not a positive finite number puts the stall threshold at or
// below zero, and NaN converts to a Duration below every elapsed time, so every
// run is declared stalled on its first watch tick and killed. --stall-factor is
// checked at the flag, but IsStalled is exported and DriveItem forwards
// whatever factor its caller handed it.
//
// Both arms are load-bearing. The "not stalled" arm alone would still pass if
// the guard just returned false for a bad factor, which turns stall detection
// off rather than correcting it; the "stalled" arm pins that the fallback is
// the default factor and the predicate still does its job.
func TestIsStalled_ABadFactorFallsBackToTheDefault(t *testing.T) {
	base := 60 * time.Minute
	for _, factor := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprintf("factor %g", factor), func(t *testing.T) {
			// Inside the default threshold of 2.5 x 60m.
			if IsStalled(StallInput{100 * time.Minute, base, false}, factor) {
				t.Errorf("100m into a 60m baseline reads as stalled at factor %g: "+
					"every run would be killed on its first watch tick", factor)
			}
			// And well past it.
			if !IsStalled(StallInput{4 * time.Hour, base, false}, factor) {
				t.Errorf("4h into a 60m baseline does not read as stalled at factor %g: "+
					"stall detection is off, not corrected", factor)
			}
		})
	}
}
