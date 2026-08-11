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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Init-container crash diagnosis (#1437, following #1425).
//
// #1425 set TerminationMessagePolicy: FallbackToLogsOnError on the runtime
// container so a fatal engine error is self-describing. The init containers
// never got it, which is backwards: on a fresh deploy the downloader is the
// container most likely to fail (bad credentials, 404, disk full, evicted for
// exceeding an emptyDir sizeLimit), and it reported nothing.
//
// Setting it on every generated init container also removes the asymmetry that
// made normalizeContainers unable to stop stripping the field: with the
// operator setting it everywhere it generates a container, desired and live
// agree and there is nothing to normalise away.

func storageConfigModel(source string) *inferencev1alpha1.Model {
	m := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: source},
	}
	m.Status.CacheKey = "deadbeefdeadbeef"
	return m
}

// Every init container the storage builder emits, on every source path, must
// carry the policy. Table-driven over the paths rather than one case, because
// the builder has several branches and a new one must not silently miss it.
func TestInitContainersCarryTerminationMessagePolicy(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		useCache bool
	}{
		{"remote http, cached", "https://example.com/model.gguf", true},
		{"remote http, emptyDir", "https://example.com/model.gguf", false},
		{"s3, cached", "s3://bucket/key/model.gguf", true},
		{"s3, emptyDir", "s3://bucket/key/model.gguf", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := buildModelStorageConfig(
				storageConfigModel(tc.source), nil, "default", tc.useCache,
				ModelCacheModeShared, "", "docker.io/curlimages/curl:8.18.0", 102, nil)
			if len(cfg.initContainers) == 0 {
				t.Fatal("no init containers built; fixture does not exercise the path")
			}
			for _, c := range cfg.initContainers {
				if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
					t.Errorf("init container %q has TerminationMessagePolicy %q, want %q",
						c.Name, c.TerminationMessagePolicy,
						corev1.TerminationMessageFallbackToLogsOnError)
				}
			}
		})
	}
}

// normalizeContainers must stop blanking the field, so drift in something the
// operator now sets deliberately is comparable rather than normalised away.
func TestNormalizeContainersKeepsTerminationMessagePolicy(t *testing.T) {
	containers := []corev1.Container{{
		Name:                     "c",
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		TerminationMessagePath:   "/dev/termination-log",
		ImagePullPolicy:          corev1.PullAlways,
	}}
	normalizeContainers(containers)

	if got := containers[0].TerminationMessagePolicy; got != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("TerminationMessagePolicy = %q, want it preserved", got)
	}
	// The genuinely-defaulted neighbours must still be stripped; this is the
	// line between "the operator sets it" and "the API server defaults it".
	if containers[0].TerminationMessagePath != "" {
		t.Error("TerminationMessagePath should still be normalised away")
	}
	if containers[0].ImagePullPolicy != "" {
		t.Error("ImagePullPolicy should still be normalised away")
	}
}

// normalizeContainers must not strip SecurityContext fields the operator owns.
// Two containers differing only in an operator-owned SecurityContext field must
// still compare as different after normalization. Under the old code they
// normalised to equal (the bug); this test fails if the stripping block is
// restored. See #1462.
func TestNormalizeContainersKeepsSecurityContextDifferences(t *testing.T) {
	// inferContainerSecurityContext sets AllowPrivilegeEscalation and Capabilities;
	// initContainerSecurityContext also sets ReadOnlyRootFilesystem and
	// RunAsUser/RunAsGroup. Use those fields in the test.
	containers := []corev1.Container{
		{
			Name: "c1",
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(false),
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
			},
		},
		{
			Name: "c2",
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(true), // differs from c1
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
			},
		},
	}
	normalizeContainers(containers)

	if containers[0].SecurityContext.AllowPrivilegeEscalation == containers[1].SecurityContext.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation difference was normalised away: the operator-owned field was stripped")
	}
}

// The property that keeping the field comparable could plausibly break: a
// desired template compared against itself must report no drift. If the
// operator ever leaves the policy unset on a container it generates, the live
// object gets the API-server default and every reconcile sees a difference.
func TestPodTemplatesDoNotDifferFromThemselves(t *testing.T) {
	cfg := buildModelStorageConfig(
		storageConfigModel("https://example.com/model.gguf"), nil, "default", true,
		ModelCacheModeShared, "", "docker.io/curlimages/curl:8.18.0", 102, nil)

	// Live objects come back from the API server with the policy defaulted on
	// any container that did not set one. Simulate that on a copy.
	desired := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		InitContainers: append([]corev1.Container(nil), cfg.initContainers...),
		Containers:     []corev1.Container{{Name: "server", TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError}},
	}}
	live := *desired.DeepCopy()
	for i := range live.Spec.InitContainers {
		if live.Spec.InitContainers[i].TerminationMessagePolicy == "" {
			live.Spec.InitContainers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
		}
	}
	for i := range live.Spec.Containers {
		if live.Spec.Containers[i].TerminationMessagePolicy == "" {
			live.Spec.Containers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
		}
	}

	if podTemplatesDiffer(live, desired) {
		t.Error("a template differs from itself once the API server defaults " +
			"TerminationMessagePolicy: the operator left it unset on a container it generates")
	}
}
