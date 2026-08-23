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
	"math"
	"time"
)

const (
	// DefaultStallFactor multiplies the baseline to get the stall threshold.
	DefaultStallFactor = 2.5
	// DefaultBaseline applies when there is no audit history to median.
	DefaultBaseline = 60 * time.Minute
)

// StallInput is what the stall predicate needs. Turn counts are deliberately
// absent: reconstructing them today means parsing llama.cpp slot logs, which
// is forensics rather than a signal. #1628 would make turn accounting a real
// metric and would sharpen this, but elapsed-vs-baseline with no branch
// pushed caught both real stalls in the reference batch.
type StallInput struct {
	Elapsed      time.Duration
	Baseline     time.Duration
	BranchPushed bool
}

// IsStalled reports whether a run should be killed: nothing pushed, and
// elapsed past factor x baseline. A run that has pushed a branch is making
// progress regardless of how long it is taking.
//
// A factor that is not a positive finite number falls back to the default, the
// same way a missing baseline does. It is not a theoretical input: the value
// comes from --stall-factor, which the flag layer checks, but IsStalled is
// exported and DriveItem forwards whatever factor its caller passed, so the
// predicate that decides whether to destroy a run in progress does not take
// that on trust. Zero or negative puts the threshold at or below zero and NaN
// converts to a Duration below every elapsed time, so both make the first watch
// tick kill the run. NaN is tested separately because NaN <= 0 is false.
// Falling back beats returning false: a bogus factor should not quietly disable
// stall detection either.
func IsStalled(in StallInput, factor float64) bool {
	if in.BranchPushed {
		return false
	}
	if math.IsNaN(factor) || math.IsInf(factor, 0) || factor <= 0 {
		factor = DefaultStallFactor
	}
	base := in.Baseline
	if base <= 0 {
		base = DefaultBaseline
	}
	return in.Elapsed > time.Duration(float64(base)*factor)
}
