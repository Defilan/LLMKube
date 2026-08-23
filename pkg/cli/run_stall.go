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

import "time"

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
func IsStalled(in StallInput, factor float64) bool {
	if in.BranchPushed {
		return false
	}
	base := in.Baseline
	if base <= 0 {
		base = DefaultBaseline
	}
	return in.Elapsed > time.Duration(float64(base)*factor)
}
