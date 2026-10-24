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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The tripwire for #2057: with the default policy the generated pod templates
// must be identical to the ones this operator emitted before the policy flag
// existed. Rollouts key on desiredTemplateHash, so any byte that moves here
// rolls every cache-backed pod on upgrade.
//
// The hashes below were recorded against upstream/main (<base>) while the
// flag did not exist. A diff means a builder changed shape for the default
// configuration; that is either a bug in the policy gating or a deliberate
// template change that needs its own reason and its own upgrade note.

const (
	// cachePrepTripwireDeploymentHash is desiredTemplateHash of
	// constructDeployment for cachePrepTripwireIsvc and cachePrepTripwireModel.
	cachePrepTripwireDeploymentHash = "0a7017684ea68911"
	// cachePrepTripwireJobHash is desiredTemplateHash of buildPrefetchJob's
	// pod template for cachePrepTripwireModel.
	cachePrepTripwireJobHash = "f72f2d11ad1a66b3" //nolint:gosec // a SHA-256 prefix of a pod template, not a credential
)

func cachePrepTripwireModel() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "tripwire-model", Namespace: "tripwire-ns"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/models/tripwire.gguf",
			Format: "gguf",
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "tripwire-key", Path: "/models/tripwire-key/tripwire.gguf"},
	}
}

func cachePrepTripwireIsvc() *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "tripwire-isvc", Namespace: "tripwire-ns"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "tripwire-model",
			Runtime:  "llamacpp",
		},
	}
}

func cachePrepTripwireISvcReconciler() *InferenceServiceReconciler {
	return &InferenceServiceReconciler{
		ModelCachePath:     "/models",
		ModelCacheMode:     ModelCacheModeShared,
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		DefaultFSGroup:     102,
	}
}

func cachePrepTripwireModelReconciler() *ModelReconciler {
	return &ModelReconciler{
		InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
		DefaultFSGroup:       102,
		ModelCacheSize:       "10Gi",
		ModelCacheAccessMode: "ReadWriteOnce",
		ModelCacheMode:       ModelCacheModeShared,
		ModelCachePath:       "/models",
	}
}

func TestCachePrepAlwaysTemplatesUnchanged(t *testing.T) {
	r := cachePrepTripwireISvcReconciler()
	dep := r.constructDeployment(cachePrepTripwireIsvc(), cachePrepTripwireModel(), nil, 1, "", "")
	if got := desiredTemplateHash(dep.Spec.Template); got != cachePrepTripwireDeploymentHash {
		t.Errorf("cache-backed Deployment pod template drifted from base: desiredTemplateHash = %s, want %s\n"+
			"every cache-backed pod would roll on upgrade; the default policy must render exactly what base rendered",
			got, cachePrepTripwireDeploymentHash)
	}

	mr := cachePrepTripwireModelReconciler()
	m := cachePrepTripwireModel()
	seedPrefetchCacheKey(m)
	job, err := mr.buildPrefetchJob(t.Context(), m, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob: %v", err)
	}
	if got := desiredTemplateHash(job.Spec.Template); got != cachePrepTripwireJobHash {
		t.Errorf("prefetch Job pod template drifted from base: desiredTemplateHash = %s, want %s", got, cachePrepTripwireJobHash)
	}
}

// A reconciler built without the policy field (every test literal written
// before the flag, and any embedding that constructs one by hand) must render
// what it rendered before: the prep.
func TestCachePrepEmptyPolicyRendersPrep(t *testing.T) {
	cfg := buildCachedStorageConfig(cachePrepTripwireModel(), nil, "", "", "docker.io/curlimages/curl:8.18.0", 102, ModelCachePrepAlways, "")
	if len(cfg.initContainers) < 1 || cfg.initContainers[0].Name != "model-cache-prep" {
		t.Fatalf("empty policy rendered initContainers %v, want model-cache-prep first", containerNames(cfg.initContainers))
	}
}
