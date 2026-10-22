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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
)

// The two cache-access pods differ by the access they need: the inspector
// reads, the purge writes. A kind that mixed them up would either fail on a
// strict RWO claim or delete entries it was only asked to list.
func TestCacheAccessPodKinds(t *testing.T) {
	if !inspectorPodKind.readOnly {
		t.Error("the inspector pod mounts the claim read-write")
	}
	if purgePodKind.readOnly {
		t.Error("the purge pod mounts the claim read-only")
	}
	if inspectorPodKind.component == purgePodKind.component {
		t.Errorf("both kinds carry the component label %q", inspectorPodKind.component)
	}
	if inspectorPodName("claim-a") == purgePodName("claim-a") {
		t.Error("both kinds name their pod the same")
	}

	clientset := fakeclientset.NewClientset()
	if _, err := createInspectorPodForPVC(context.Background(), clientset, "test-ns",
		modelCachePVCName); err != nil {
		t.Fatalf("create inspector pod: %v", err)
	}
	pod, err := clientset.CoreV1().Pods("test-ns").Get(context.Background(),
		inspectorPodName(modelCachePVCName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get inspector pod: %v", err)
	}
	if !pod.Spec.Containers[0].VolumeMounts[0].ReadOnly || !pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Error("the inspector pod spec mounts the claim read-write")
	}
	if pod.Labels[modelCacheLabel] != "cache-inspector" {
		t.Errorf("inspector component label = %q, want %q", pod.Labels[modelCacheLabel], "cache-inspector")
	}
}
