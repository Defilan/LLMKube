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

package agent

import (
	"context"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// usesGenericGate reports whether a task's GateProfile should run the
// language-agnostic generic gate (RunGenericGate) instead of the
// specialized Go gate (RunCoderGate). A nil profile, an empty language, or
// an explicit "go" language all use the Go path, so existing Go tasks are
// unaffected and the Go gate stays byte-identical.
func usesGenericGate(profile *foremanv1alpha1.GateProfile) bool {
	return profile != nil &&
		profile.Language != "" &&
		profile.Language != foremanv1alpha1.GateLanguageGo
}

// RunGenericGate runs a language-agnostic fast gate by executing the
// resolved profile's commands in the workspace via `sh -c`. Each non-empty
// command (format, lint, build, test, codegen) is one check, and a non-zero
// exit is a failure. Like RunCoderGate, every check runs regardless of
// earlier failures so the feedback reports everything wrong at once.
//
// Non-Go GateProfiles use this path. The Go profile keeps the specialized
// RunCoderGate, which carries the Go-specific semantics (gofmt's
// output-not-error signal, golangci-lint's env and binary path, the
// changed-package test tier, and the controller-gen codegen-drift check)
// that do not generalize to other toolchains.
func RunGenericGate(
	ctx context.Context,
	workspace string,
	gate foremanv1alpha1.ResolvedGate,
	run commandRunner,
) (pass bool, feedback string) {
	checks := []struct {
		name string
		cmd  string
	}{
		{"format", gate.Format},
		{"lint", gate.Lint},
		{"build", gate.Build},
		{"test", gate.Test},
		{"codegen", gate.CodegenCheck},
	}

	var failures []checkFailure
	for _, c := range checks {
		cmd := strings.TrimSpace(c.cmd)
		if cmd == "" {
			continue
		}
		if out, err := run(ctx, workspace, nil, "sh", "-c", cmd); err != nil {
			failures = append(failures, checkFailure{name: c.name + ": " + cmd, output: out})
		}
	}

	if len(failures) == 0 {
		return true, ""
	}
	return false, buildFeedback(failures)
}
