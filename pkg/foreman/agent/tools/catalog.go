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

package tools

// CanonicalToolNames returns the sorted set of every tool name the v0.1
// surface exposes: read_file, write_file, str_replace, grep, bash,
// submit_result, run_gate_job, fetch_issue.
//
// It is the single source of truth for "which tool names are valid in an
// Agent.spec.tools whitelist." The Agent admission webhook consumes this
// to reject typos at apply time, matching the runtime Filter() check the
// foreman-agent's makeRegistryFactory performs (a whitelist name not in
// the registry fails the loud-not-silent contract).
//
// The list is derived from a names-only Registry built with the SAME tool
// constructors makeRegistryFactory uses, so a tool added there is picked
// up here without a second hand-maintained copy. Each tool's Name() is a
// pure method that does not touch the workspace, so zero-value structs are
// safe to construct here for the sole purpose of reading Name().
func CanonicalToolNames() []string {
	r, err := New(canonicalToolSet()...)
	if err != nil {
		// New() only errors on a duplicate Name(); that is a programmer
		// error in canonicalToolSet, not a runtime condition. Panicking
		// here keeps the function signature clean for callers (the
		// webhook wants a []string, not a (slice, error)); the duplicate
		// would be caught by the tools package unit tests well before any
		// webhook runs.
		panic("tools.CanonicalToolNames: duplicate tool name in canonical set: " + err.Error())
	}
	return r.Names()
}

// canonicalToolSet constructs one of every tool the v0.1 surface exposes,
// mirroring makeRegistryFactory in cmd/foreman-agent. The constructors
// take a zero workspace + nil collaborators on purpose: this set exists
// only to read Name(), never to Dispatch().
func canonicalToolSet() []Tool {
	return []Tool{
		&ReadFileTool{},
		&WriteFileTool{},
		&StrReplaceTool{},
		&GrepTool{},
		&BashTool{},
		SubmitResultTool{},
		&RunGateJobTool{},
		&FetchIssueTool{},
	}
}
