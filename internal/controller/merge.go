/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"sort"
	"strings"
)

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

// appliedPodLabelsAnnotation and appliedPodAnnotationsAnnotation record, on a
// Deployment, which of the user's podLabels and podAnnotations keys the
// operator put on its pod template. mergePreservingExternal keeps every key
// the desired template drops, so without the record a key removed from the
// spec would stay on the pods forever (#2287). The record names only the
// operator's own writes, so keys other tools add are never pruned. It sits on
// the Deployment, not the template, so writing it does not roll the pods.
const (
	appliedPodLabelsAnnotation      = "inference.llmkube.dev/applied-pod-labels"
	appliedPodAnnotationsAnnotation = "inference.llmkube.dev/applied-pod-annotations"
)

// appliedKeys is the sorted, comma-separated list of the keys in user that
// operator does not own, or "" when there are none. Label and annotation keys
// cannot contain a comma.
func appliedKeys(user, operator map[string]string) string {
	keys := make([]string, 0, len(user))
	for k := range user {
		if _, owned := operator[k]; !owned {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// appliedKeyRecords is the Deployment annotations recording the user keys a
// pod template carries: the user's labels and annotations minus the keys the
// operator's own maps take. Pass nil for an operator map the user's keys win
// over. Nil when the user set no keys.
func appliedKeyRecords(userLabels, operatorLabels, userAnnotations, operatorAnnotations map[string]string) map[string]string {
	var out map[string]string
	for k, v := range map[string]string{
		appliedPodLabelsAnnotation:      appliedKeys(userLabels, operatorLabels),
		appliedPodAnnotationsAnnotation: appliedKeys(userAnnotations, operatorAnnotations),
	} {
		if v == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// withoutUnwantedKeys returns a copy of live without the keys in the recorded
// applied list that desired no longer sets. live itself is not modified.
func withoutUnwantedKeys(live map[string]string, applied string, desired map[string]string) map[string]string {
	if applied == "" || len(live) == 0 {
		return live
	}
	out := make(map[string]string, len(live))
	for k, v := range live {
		out[k] = v
	}
	for _, k := range strings.Split(applied, ",") {
		if _, ok := desired[k]; !ok {
			delete(out, k)
		}
	}
	return out
}

// syncAppliedKeyRecords copies the applied-key records from desired onto the
// live Deployment's annotations, and removes a record desired no longer has.
func syncAppliedKeyRecords(live *map[string]string, desired map[string]string) {
	for _, k := range []string{appliedPodLabelsAnnotation, appliedPodAnnotationsAnnotation} {
		if v, ok := desired[k]; ok {
			if *live == nil {
				*live = map[string]string{}
			}
			(*live)[k] = v
		} else {
			delete(*live, k)
		}
	}
}
