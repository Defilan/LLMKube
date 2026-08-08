/*
Copyright 2026.

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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Ephemeral model cache (#1451). An InferenceService can decline the cache PVC
// and download into an emptyDir instead, for two cases: a node where dynamic
// provisioning is unavailable (a NoSchedule taint the storage provisioner
// cannot follow), and a fast local origin where re-pulling on restart is
// cheaper than carrying a 100Gi volume on every GPU node.
//
// The two gates that must agree are the provisioning side (modelNeedsCachePVC,
// which decides whether to create the claim) and the mount side (the useCache
// value the deployment builder passes to buildModelStorageConfig). If they
// disagree the pod either mounts a claim nobody created or leaves an orphaned
// claim behind, so both are pinned here.

func ephemeralISvc() *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "m",
			ModelCache: &inferencev1alpha1.ModelCacheSpec{
				Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral,
			},
		},
	}
}

func cacheableModel() *inferencev1alpha1.Model {
	m := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "s3://models/org/repo/m.gguf"},
	}
	m.Status.CacheKey = "deadbeefdeadbeef"
	return m
}

func TestModelCachePersistence_EphemeralSkipsPVC(t *testing.T) {
	model := cacheableModel()

	// Control: the same model without the opt-out does want a cache PVC.
	// Without this the test could pass because the model was never cacheable.
	if !modelNeedsCachePVC(model, nil, "/models") {
		t.Fatal("control failed: a cacheable model with caching enabled should want a PVC")
	}

	if modelNeedsCachePVC(model, ephemeralISvc(), "/models") {
		t.Error("ephemeral persistence should skip the cache PVC, but the operator would provision one")
	}
}

func TestModelCachePersistence_DefaultAndCachedStillCache(t *testing.T) {
	model := cacheableModel()

	cases := []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
	}{
		{"nil isvc", nil},
		{"no modelCache block", &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		}},
		{"modelCache set but persistence empty", &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "user-pvc"},
			},
		}},
		{"persistence explicitly Cached", &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelCache: &inferencev1alpha1.ModelCacheSpec{
					Persistence: inferencev1alpha1.ModelCachePersistenceCached,
				},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !modelNeedsCachePVC(model, tc.isvc, "/models") {
				t.Error("caching should remain on; only an explicit Ephemeral opt-out disables it")
			}
		})
	}
}

// The mount side must reach the same verdict as the provisioning side. A
// mismatch is the failure worth guarding: the pod mounts a claim that was never
// created, and stays Pending on a volume that will never appear.
func TestModelCachePersistence_MountSideAgrees(t *testing.T) {
	model := cacheableModel()

	if !modelWantsCacheVolume(model, nil, "/models") {
		t.Fatal("control failed: a cacheable model should mount the cache volume")
	}
	if modelWantsCacheVolume(model, ephemeralISvc(), "/models") {
		t.Error("ephemeral persistence should mount an emptyDir, not the cache volume")
	}

	// Both gates must agree for every combination, since disagreement is what
	// produces an orphaned claim or an unschedulable pod.
	for _, isvc := range []*inferencev1alpha1.InferenceService{nil, ephemeralISvc()} {
		if got, want := modelWantsCacheVolume(model, isvc, "/models"), modelNeedsCachePVC(model, isvc, "/models"); got != want {
			t.Errorf("mount side (%v) and provisioning side (%v) disagree", got, want)
		}
	}
}

// Ephemeral must not resurrect caching when the operator has it switched off
// globally, and must not override the pvc:// exclusion.
func TestModelCachePersistence_DoesNotOverrideOtherExclusions(t *testing.T) {
	if modelNeedsCachePVC(cacheableModel(), ephemeralISvc(), "") {
		t.Error("caching disabled on the operator must stay disabled")
	}
	staged := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "pvc://weights/m.gguf"},
	}
	staged.Status.CacheKey = "deadbeefdeadbeef"
	if modelNeedsCachePVC(staged, ephemeralISvc(), "/models") {
		t.Error("pvc:// sources are pre-staged and never take a cache PVC")
	}
}
