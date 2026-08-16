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
// Setting it on every generated init container makes the policy self-describing
// everywhere, so a failed downloader is diagnosable the same way the runtime
// container is.

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
