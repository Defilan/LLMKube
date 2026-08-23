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

package reviewer

// Verdict-demotion markers.
//
// A harness rail may rewrite a reviewer's verdict. When one touches a
// verdict it stamps the reviewer's submit_result extra with verdictDemoted
// (true), verdictClaimed (the archived original verdict), demotionReason
// (prose), and verdictDemotedBy: the name of the rail that made the call.
//
// verdictDemotedBy exists because the bare flag cannot say WHICH rail acted,
// and the consumers do not want the same answer from each (#1636). The
// issueAsk rail demotes when it cannot prove the reviewer read the issue:
// that is a statement about verification confidence, and re-running the
// coder cannot change it. The scope-overlap rail demotes when the diff
// touches none of the files the issue names: that is a statement about the
// diff, and the coder CAN act on it. A consumer keying off verdictDemoted
// alone conflates the two.
//
// The names live here, in the leaf package that already defines the
// reviewer's submit_result.extra contract, because the producer
// (pkg/foreman/agent's rails) and the consumer (internal/foreman/controller)
// both need the same strings and neither imports the other.
const (
	// RailIssueAsk names enforceReviewerIssueAsk in
	// pkg/foreman/agent/executor_native.go.
	RailIssueAsk = "issueAsk"
	// RailScopeOverlap names enforceReviewerScopeOverlap in
	// pkg/foreman/agent/scope_overlap.go.
	RailScopeOverlap = "scope-overlap"
)
