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

// Stage is a queue item's position in the run loop.
type Stage string

const (
	StagePreflight Stage = "preflight"
	StageDispatch  Stage = "dispatch"
	StageWatch     Stage = "watch"
	StageVerify    Stage = "verify"
	StageFeedback  Stage = "feedback"
	StageFinalize  Stage = "finalize"
	StageParked    Stage = "parked"
	StageDone      Stage = "done"
)

// maxAttempts is the structural one-pass rule: one original attempt plus at
// most one automatic feedback pass. A third is never reached by any input.
const maxAttempts = 2

// Facts are the observations the driver gathered for the current stage.
// NextStage is a pure function of (stage, facts) so the machine is table
// testable without a cluster.
type Facts struct {
	// SkipReason, when non-empty, ends the item at preflight.
	SkipReason string
	// Stalled reports the stall predicate's verdict during watch.
	Stalled bool
	// VerifyClean reports whether the independent verify stage found nothing.
	VerifyClean bool
	// Attempts counts coder attempts made so far, starting at 1.
	Attempts int
}

// Transition is the machine's output: the next stage, and when that stage is
// StageParked, which decision kind to park.
type Transition struct {
	Next   Stage
	Park   string
	Reason string
}

// NextStage advances one queue item. Parking is terminal for this pass: the
// driver records the decision and moves to the next item rather than blocking.
func NextStage(cur Stage, f Facts) Transition {
	switch cur {
	case StagePreflight:
		if f.SkipReason != "" {
			return Transition{Next: StageDone, Reason: f.SkipReason}
		}
		return Transition{Next: StageDispatch}
	case StageDispatch:
		return Transition{Next: StageWatch}
	case StageWatch:
		if f.Stalled {
			return Transition{Next: StageParked, Park: "escalate", Reason: "stalled"}
		}
		return Transition{Next: StageVerify}
	case StageVerify:
		if f.VerifyClean {
			return Transition{Next: StageFinalize}
		}
		if f.Attempts >= maxAttempts {
			return Transition{Next: StageParked, Park: "escalate", Reason: "verify failed after the feedback pass"}
		}
		return Transition{Next: StageParked, Park: "adjudicate", Reason: "verify found issues"}
	case StageFeedback:
		return Transition{Next: StageWatch}
	case StageFinalize:
		return Transition{Next: StageDone}
	}
	return Transition{Next: StageDone, Reason: "unknown stage"}
}
