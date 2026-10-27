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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A cache pod at a name this run does not use is deleted only by the run that
// stamped it. One a live run stamps goes away when that run ends, so this run
// waits for it, whatever its kind; any other is a leftover this run does not
// delete, so it is refused at once.
func TestSecondMountBlockerOnACachePodAtAnotherName(t *testing.T) {
	// Created now: a pod's own clock starts at zero, which reads as abandoned.
	created := metav1.NewTime(time.Now())
	pod := func(labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "llmkube-cache-purge-deadbeef", Labels: labels, CreationTimestamp: created,
		}}
	}
	for _, c := range []struct {
		name    string
		labels  map[string]string
		wantErr string
	}{
		{
			name: "unstamped pod of this kind",
			labels: map[string]string{
				managedByLabel: managedByValue, modelCacheLabel: purgePodKind.component,
			},
			wantErr: "this run does not delete it",
		},
		{
			name: "unstamped pod of the other kind",
			labels: map[string]string{
				managedByLabel: managedByValue, modelCacheLabel: inspectorPodKind.component,
			},
			wantErr: "this run does not delete it",
		},
		{
			name: "pod of this kind a live run stamps",
			labels: map[string]string{
				managedByLabel: managedByValue, modelCacheLabel: purgePodKind.component,
				cachePurgeRunLabel: "another-run",
			},
		},
		{
			name: "pod of the other kind a live run stamps",
			labels: map[string]string{
				managedByLabel: managedByValue, modelCacheLabel: inspectorPodKind.component,
				cachePurgeRunLabel: "another-run",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := secondMountBlocker(pod(c.labels), modelCachePVCName, "llmkube-cache-purge-0123",
				purgePodKind, "this-run", true, nil)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("expected the run to wait for the pod, got %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("error = %v, want %q", err, c.wantErr)
			}
		})
	}
}

// A pod this run stamped at its own name is replaced too: it is this run's
// leftover, not a concurrent run's.
func TestSecondMountBlockerReplacesThisRunsOwnStamp(t *testing.T) {
	holder := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:              "llmkube-cache-purge-0123",
		CreationTimestamp: metav1.NewTime(time.Now()),
		Labels: map[string]string{
			managedByLabel: managedByValue, modelCacheLabel: purgePodKind.component,
			cachePurgeRunLabel: "this-run",
		},
	}}
	if err := secondMountBlocker(holder, modelCachePVCName, "llmkube-cache-purge-0123",
		purgePodKind, "this-run", true, nil); err != nil {
		t.Fatalf("this run's own leftover must be replaced: %v", err)
	}
}

// claimShareable mirrors singleWriterClaimHolder in internal/controller: any
// ReadWriteMany or ReadOnlyMany mode makes a claim shareable, even listed
// beside a single-writer mode, and claimSinglePod holds for ReadWriteOncePod
// alone. An unreadable claim is neither.
func TestClaimAccessModes(t *testing.T) {
	for _, c := range []struct {
		name                 string
		modes                []corev1.PersistentVolumeAccessMode
		shareable, singlePod bool
	}{
		{"RWO", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, false, false},
		{"RWOP", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, false, true},
		{"RWX", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, true, false},
		{"ROX", []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}, true, false},
		{"RWO and RWX", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteMany}, true, false},
		{"RWO and ROX", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadOnlyMany}, true, false},
		{"no modes", nil, false, false},
	} {
		pvc := dynamicClaim(c.modes...)
		if got := claimShareable(pvc); got != c.shareable {
			t.Errorf("%s: claimShareable = %v, want %v", c.name, got, c.shareable)
		}
		if got := claimSinglePod(pvc); got != c.singlePod {
			t.Errorf("%s: claimSinglePod = %v, want %v", c.name, got, c.singlePod)
		}
	}
	if claimShareable(nil) || claimSinglePod(nil) {
		t.Error("an unreadable claim must count as neither shareable nor single-pod")
	}
}

// Whitespace around an image reference, from a quoted flag or a variable in a
// shell profile, is not part of it, and a value of only whitespace is unset.
func TestCacheAccessImageNameTrimsWhitespace(t *testing.T) {
	for _, c := range []struct {
		name, flag, env, want string
	}{
		{"padded flag", "  registry.local/busybox:1.37.0\n", "", "registry.local/busybox:1.37.0"},
		{"padded environment", "", "\tregistry.local/busybox:1.37.0 ", "registry.local/busybox:1.37.0"},
		{"blank flag falls through to the environment", "   ", "registry.local/busybox:1.37.0",
			"registry.local/busybox:1.37.0"},
		{"blank flag and environment", " ", " ", defaultCacheAccessImage},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(cacheImageEnv, c.env)
			var got string
			_ = withCacheImage(c.flag, func() error {
				got = cacheAccessImageName()
				return nil
			})
			if got != c.want {
				t.Errorf("image = %q, want %q", got, c.want)
			}
		})
	}
}
