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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The specs below drive the InferenceService reconciler through its public
// entry point and read the result back from the API server. Nothing here names
// a helper the #2142 refactor introduces, so the same file compiles and runs on
// the branch and against the branch it replaced: they are the parity evidence
// that resolving the effective modelCache once renders what base rendered.
var _ = Describe("Model.spec.modelCache inheritance (#2142)", func() {
	var ns string

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "llmkube-2142-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
	})

	createCachePVC := func(name string) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, pvc))).To(Succeed())
	}

	readyModel := func(name, source string, mutate func(*inferencev1alpha1.Model)) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.ModelSpec{Source: source, Format: "gguf"},
		}
		if mutate != nil {
			mutate(m)
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		key := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		m.Status.Phase = PhaseReady
		if !isPVCSource(source) {
			m.Status.CacheKey = computeCacheKey(source)
		}
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type: ConditionAvailable, Status: metav1.ConditionTrue, Reason: "Test", Message: "ready",
		})
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		return m
	}

	newService := func(name, modelName string, cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelName, ModelCache: cache},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		return isvc
	}

	reconciler := func() *InferenceServiceReconciler {
		return &InferenceServiceReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			Recorder:             events.NewFakeRecorder(50),
			ModelCachePath:       "/models",
			ModelCacheMode:       ModelCacheModeShared,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
		}
	}

	reconcileOnce := func(r *InferenceServiceReconciler, name string) *inferencev1alpha1.InferenceService {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got)).To(Succeed())
		return got
	}

	claimOf := func(spec *corev1.PodSpec) string {
		for _, v := range spec.Volumes {
			if v.Name == "model-cache" && v.PersistentVolumeClaim != nil {
				return v.PersistentVolumeClaim.ClaimName
			}
		}
		return ""
	}

	It("keeps the Model's claim across a mid-reconcile status write", func() {
		// A guard, not a reproduction: it passes on the branch this change
		// replaced too, which re-applies the inherited block after that
		// write. It pins that a service inheriting claimName from its Model
		// keeps mounting that claim when a status write lands mid-reconcile
		// (a stale RolloutDeferred condition is the known one) (#2142).
		createCachePVC("midwrite-cache")
		model := readyModel("midwrite-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "midwrite-cache"}
		})
		isvc := newService("midwrite", model.Name, nil)
		key := types.NamespacedName{Name: isvc.Name, Namespace: ns}
		r := reconciler()

		By("the first pass creating the Deployment on the Model's claim")
		reconcileOnce(r, isvc.Name)
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())

		By("a stale RolloutDeferred condition and a template change, so the next pass defers and writes status")
		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		stored.Spec.RolloutPolicy = &inferencev1alpha1.RolloutPolicySpec{WaitForIdle: true, IdleTimeoutSeconds: 60}
		stored.Spec.Env = append(stored.Spec.Env, corev1.EnvVar{Name: "A_MIDWRITE_MARKER", Value: "1"})
		meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
			Type:               ConditionRolloutDeferred,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonPodsCrashLooping,
			LastTransitionTime: metav1.Now(),
		})
		stored.Status.Phase = PhaseReady
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())

		By("the second pass resolving the claim after that status write")
		reconcileOnce(r, isvc.Name)

		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(claimOf(&dep.Spec.Template.Spec)).To(Equal("midwrite-cache"),
			"the Deployment must keep mounting the Model's claim after the mid-reconcile status write")
	})

	It("verifies the Model's claim a service inherits, and mounts it for the draft too", func() {
		// The inherited claim is the service's cache volume, so the
		// provisioning side must verify it (a user-owned claim that is missing
		// is Degraded, not silently replaced by one the pods never mount), and
		// the draft's weights ride on that same claim.
		model := readyModel("inherited-claim-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "inherited-claim"}
		})
		readyModel("inherited-draft", "https://example.com/models/draft.gguf", nil)

		By("refusing to provision a replacement when the inherited claim is missing")
		missing := newService("inherited-missing", model.Name, nil)
		missing.Spec.SpeculativeDecoding = &inferencev1alpha1.SpeculativeDecodingSpec{Type: "draft-simple", DraftModelRef: "inherited-draft"}
		Expect(k8sClient.Update(ctx, missing)).To(Succeed())
		got := reconcileOnce(reconciler(), missing.Name)
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		Expect(got.Status.Conditions).NotTo(BeEmpty())
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)).NotTo(BeNil(),
			"the Model's claim is verified as the service's cache volume; a missing one is Degraded")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: ns},
			&corev1.PersistentVolumeClaim{}))).To(BeTrue(), "no shared claim is created for a service that mounts another")

		By("mounting the inherited claim for both the target and the draft once it exists")
		createCachePVC("inherited-claim")
		isvc := newService("inherited-draft-svc", model.Name, nil)
		isvc.Spec.SpeculativeDecoding = &inferencev1alpha1.SpeculativeDecodingSpec{Type: "draft-simple", DraftModelRef: "inherited-draft"}
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		reconcileOnce(reconciler(), isvc.Name)

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, dep)).To(Succeed())
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				Expect(v.PersistentVolumeClaim.ClaimName).To(Equal("inherited-claim"),
					"volume %q mounts the claim the service resolves, not the draft Model's own default", v.Name)
			}
		}
	})

	It("renders the same cache surface for an inherited block as for the same block written out", func() {
		// The parity claim: a service that inherits the Model's block must get
		// the pod base gave it, so the rendered cache surface has to match a
		// service that carries that block explicitly. Neither block asks for a
		// prefetch: prefetch is the service's own request, so neither side is
		// held for a staging, and both download at serve time as base did.
		createCachePVC("parity-cache")
		model := readyModel("parity-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "parity-cache"}
		})
		newService("parity-inherited", model.Name, nil)
		newService("parity-explicit", model.Name, model.Spec.ModelCache.DeepCopy())

		r := reconciler()
		read := func(name string) (*appsv1.Deployment, *inferencev1alpha1.InferenceService) {
			reconcileOnce(r, name)
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, dep)).To(Succeed())
			svc := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, svc)).To(Succeed())
			return dep, svc
		}
		inheritedDep, inheritedSvc := read("parity-inherited")
		explicitDep, _ := read("parity-explicit")

		Expect(inheritedSvc.Spec.ModelCache).To(BeNil(),
			"inheritance is never written back to the service's spec")
		Expect(claimOf(&inheritedDep.Spec.Template.Spec)).To(Equal("parity-cache"))
		Expect(claimOf(&explicitDep.Spec.Template.Spec)).To(Equal("parity-cache"))
		Expect(initContainerNames(&inheritedDep.Spec.Template.Spec)).To(Equal(initContainerNames(&explicitDep.Spec.Template.Spec)))
		Expect(cacheDirEnv(inheritedDep)).To(Equal(cacheDirEnv(explicitDep)))
		Expect(hasModelDownloader(inheritedDep)).To(BeTrue(),
			"the service asked for no prefetch of its own, so it downloads at serve time as base did")
	})
})

// cacheDirEnv reads CACHE_DIR off the model-downloader init container, the
// directory the storage builder told the downloader to write.
func cacheDirEnv(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.InitContainers {
		if c.Name != "model-downloader" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "CACHE_DIR" {
				return e.Value
			}
		}
	}
	return ""
}

// hasModelDownloader reports whether the pod downloads at serve time, which a
// service that asked for no prefetch of its own still does.
func hasModelDownloader(dep *appsv1.Deployment) bool {
	for _, c := range dep.Spec.Template.Spec.InitContainers {
		if c.Name == "model-downloader" {
			return true
		}
	}
	return false
}
