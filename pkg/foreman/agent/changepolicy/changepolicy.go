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

// Package changepolicy provides a provider-neutral work-class classification
// and human-review gate for AgenticTask verdicts. It mirrors the GitHub-specific
// logic in workclass.go and verdict_policy.go, extracted behind an interface
// so future providers (Forgejo, Linear, etc.) can supply their own policies.
package changepolicy

import "path/filepath"

// WorkClass buckets a changed file into the honest-verdict policy classes
// (proposal 1075, section 3.1).
type WorkClass string

const (
	workClassCIPolicy      WorkClass = "ci-policy"
	workClassReleasePolicy WorkClass = "release-policy"
	workClassPackaging     WorkClass = "packaging"
	workClassDocs          WorkClass = "docs"
	workClassConfig        WorkClass = "config"
	workClassCodeFix       WorkClass = "code-fix"
	workClassMixed         WorkClass = "mixed"
)

// footprintDominance is the changed-line share a class must reach for the
// diff to take that class; below it the footprint is mixed.
const footprintDominance = 0.70

// ClassRule maps a set of path globs to a work class. Rules are evaluated
// in order; the first matching glob wins. Globs match with path.Match
// against the full slash path and against the base name, plus a prefix
// form for directory trees (dir/**).
type ClassRule struct {
	Globs []string
	Class WorkClass
}

// DefaultClassRules is the default set of path-to-class rules, mirroring
// the GitHub-specific classification in workclass.go and the human-review
// gate in verdict_policy.go. NewDefaultPolicy uses these rules so existing
// behavior is unchanged.
var DefaultClassRules = []ClassRule{
	{[]string{".github/workflows/**", ".github/actions/**"}, workClassCIPolicy},
	{[]string{".goreleaser*", "release-please*"}, workClassReleasePolicy},
	{[]string{"Formula/**", "Dockerfile*", "charts/**", "*.spec",
		"hack/publish-*"}, workClassPackaging},
	{[]string{"*.md", "docs/**", "examples/**"}, workClassDocs},
	{[]string{"*.yaml", "*.yml", "*.toml", "*.json"}, workClassConfig},
}

func matchGlob(glob, path string) bool {
	if ok, _ := filepath.Match(glob, path); ok {
		return true
	}
	if ok, _ := filepath.Match(glob, filepath.Base(path)); ok {
		return true
	}
	// dir/** prefix form: filepath.Match does not cross separators.
	if len(glob) > 3 && glob[len(glob)-3:] == "/**" {
		prefix := glob[:len(glob)-2]
		return len(path) > len(prefix) && path[:len(prefix)] == prefix
	}
	return false
}

// ChangePolicy classifies changed paths into work classes and gates
// human review when a change falls outside the self-GO allowlist.
type ChangePolicy interface {
	// Classify returns the dominant work class for a set of changed paths
	// with their added+deleted line counts.
	Classify(changed map[string]int) WorkClass
	// RequiresHumanReview returns true when a change over the given paths
	// needs human review (falls outside the selfGO allowlist).
	RequiresHumanReview(changedPaths []string, selfGO []string) bool
	// NeedsVerification reports whether a change with the given footprint
	// (path -> changed-line count) needs human verification: its class is
	// outside selfGO, EXCEPT a "mixed" footprint whose every constituent
	// class is itself in selfGO (a change merely spanning several self-GO
	// classes, e.g. code plus its regenerated manifests, #1342).
	NeedsVerification(changed map[string]int, selfGO []string) bool
}

// NewDefaultPolicy returns the default ChangePolicy, which mirrors the
// GitHub-specific classification in workclass.go and the human-review
// gate in verdict_policy.go.
func NewDefaultPolicy() ChangePolicy {
	return NewPolicy(DefaultClassRules)
}

// NewPolicy returns a ChangePolicy configured with the given path-to-class
// rules. Pass DefaultClassRules (or use NewDefaultPolicy) for the GitHub
// defaults; pass a custom set to classify non-GitHub paths (e.g. a
// Woodpecker config at .woodpecker/**) as ci-policy without recompiling.
func NewPolicy(rules []ClassRule) ChangePolicy {
	return defaultPolicy{rules: rules}
}

// defaultPolicy is the default implementation that mirrors the
// workclass.go/verdict_policy.go logic.
type defaultPolicy struct {
	rules []ClassRule
}

// Classify implements ChangePolicy.Classify.
func (p defaultPolicy) Classify(changed map[string]int) WorkClass {
	return p.classifyFootprint(changed)
}

// RequiresHumanReview implements ChangePolicy.RequiresHumanReview.
func (p defaultPolicy) RequiresHumanReview(changedPaths []string, selfGO []string) bool {
	// Each path counts as 1 line (callers with real line counts use
	// NeedsVerification directly). Delegate so the mixed-all-selfGO
	// relaxation (#1342) is applied consistently.
	changed := map[string]int{}
	for _, path := range changedPaths {
		changed[path] = 1
	}
	return p.NeedsVerification(changed, selfGO)
}

// NeedsVerification implements ChangePolicy.NeedsVerification. A change needs
// verification when its dominant class is outside selfGO, EXCEPT a "mixed"
// footprint whose every constituent class is itself in selfGO: that is a
// change merely spanning several self-GO classes (e.g. a doc-comment edit in a
// *.go file that regenerates its CRDs spans code-fix + packaging + config, all
// self-GO), not a genuinely unverifiable change, so it does not require
// verification (#1342). A zero-line/empty footprint yields no constituent
// classes and is never relaxed, so a rename-only change still needs review.
func (p defaultPolicy) NeedsVerification(changed map[string]int, selfGO []string) bool {
	class := p.classifyFootprint(changed)
	if workClassInList(class, selfGO) {
		return false
	}
	if class == workClassMixed {
		if cs := p.footprintClasses(changed); len(cs) > 0 {
			for _, c := range cs {
				if !workClassInList(c, selfGO) {
					return true
				}
			}
			return false
		}
	}
	return true
}

// classifyFile returns the work class for a single path, using the
// policy's rules. First matching glob wins; unmatched paths are code-fix.
func (p defaultPolicy) classifyFile(path string) WorkClass {
	for _, r := range p.rules {
		for _, g := range r.Globs {
			if matchGlob(g, path) {
				return r.Class
			}
		}
	}
	return workClassCodeFix
}

// classifyFootprint returns the dominant work class for a set of changed
// paths with their added+deleted line counts.
func (p defaultPolicy) classifyFootprint(changed map[string]int) WorkClass {
	if len(changed) == 0 {
		return workClassCodeFix
	}
	total, byClass := 0, map[WorkClass]int{}
	for f, n := range changed {
		total += n
		byClass[p.classifyFile(f)] += n
	}
	// Zero-line diffs (rename-only, permission-only) must not nondeterministically
	// classify as a self-GO-able class; mixed is the safe fallback.
	if total == 0 {
		return workClassMixed
	}
	for class, n := range byClass {
		if float64(n) >= footprintDominance*float64(total) {
			return class
		}
	}
	return workClassMixed
}

// footprintClasses returns the distinct work classes present in a footprint,
// counting only files with a positive changed-line count. It returns nil for
// an empty or zero-line footprint (rename-only / permission-only), so such a
// change is never treated as a set of self-GO classes.
func (p defaultPolicy) footprintClasses(changed map[string]int) []WorkClass {
	total := 0
	set := map[WorkClass]bool{}
	for f, n := range changed {
		total += n
		if n > 0 {
			set[p.classifyFile(f)] = true
		}
	}
	if total == 0 {
		return nil
	}
	out := make([]WorkClass, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	return out
}

// workClassInList reports whether class's string form appears in list.
// list is small (a handful of policy classes at most) so a linear scan
// is simplest; called at most a few times per GO verdict.
func workClassInList(class WorkClass, list []string) bool {
	for _, c := range list {
		if c == string(class) {
			return true
		}
	}
	return false
}
