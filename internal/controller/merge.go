/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// mergePreservingExternal returns a fresh map containing every key from
// existing that is not also present in desired, plus every key from
// desired. Desired wins on collision. Use this whenever an operator
// reconciler wants to assert the keys it owns while leaving foreign
// keys (sidecar-injector annotations, kubectl rollout-restart's
// restartedAt, GitOps-tool sync labels) untouched.
//
// Without this, reconcilers that do a wholesale assignment of
// metadata.labels or pod template annotations strip external keys on
// every reconcile, which manifests as flapping ReplicaSets and
// truncated in-flight requests when paired with `kubectl rollout
// restart` or any sidecar injector. See #456.
//
// Returns nil only when both inputs are empty; otherwise a non-nil
// map (the empty case matters because Kubernetes treats an empty map
// the same as nil on Patch, but Go's reflect comparisons don't).
func mergePreservingExternal(existing, desired map[string]string) map[string]string {
	if len(existing) == 0 && len(desired) == 0 {
		return nil
	}
	out := make(map[string]string, len(existing)+len(desired))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range desired {
		out[k] = v
	}
	return out
}
