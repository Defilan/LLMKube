/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"reflect"
	"testing"
)

func TestMergePreservingExternal(t *testing.T) {
	cases := []struct {
		name     string
		existing map[string]string
		desired  map[string]string
		want     map[string]string
	}{
		{
			name:     "both nil returns nil so we don't churn objects with empty maps",
			existing: nil,
			desired:  nil,
			want:     nil,
		},
		{
			name:     "desired only",
			existing: nil,
			desired:  map[string]string{"a": "1"},
			want:     map[string]string{"a": "1"},
		},
		{
			name:     "external only is preserved",
			existing: map[string]string{"sidecar.istio.io/inject": "true"},
			desired:  nil,
			want:     map[string]string{"sidecar.istio.io/inject": "true"},
		},
		{
			name: "desired wins on collision, external pass-through",
			existing: map[string]string{
				"inference.llmkube.dev/router-config-hash": "oldhash",
				"sidecar.istio.io/inject":                  "true",
				"kubectl.kubernetes.io/restartedAt":        "2026-05-13T20:00:00Z",
			},
			desired: map[string]string{
				"inference.llmkube.dev/router-config-hash": "newhash",
			},
			want: map[string]string{
				"inference.llmkube.dev/router-config-hash": "newhash",
				"sidecar.istio.io/inject":                  "true",
				"kubectl.kubernetes.io/restartedAt":        "2026-05-13T20:00:00Z",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergePreservingExternal(tc.existing, tc.desired)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got=%v want=%v", got, tc.want)
			}
		})
	}
}
