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
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("Model Prefetch", func() {
	ctx := context.Background()
	const ns = "prefetch-test"

	prefetchReconciler := func() *ModelReconciler {
		return &ModelReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
		}
	}

	newPrefetchModel := func(name string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/models/llama.gguf",
				Prefetch: true,
			},
		}
	}

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		err := k8sClient.Create(ctx, nsObj)
		if err != nil {
			Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
		}
	})

	Describe("prefetchEligible", func() {
		It("is false when the field is unset", func() {
			m := newPrefetchModel("x")
			m.Spec.Prefetch = false
			Expect(prefetchEligible(m)).To(BeFalse())
		})

		It("is false for pvc:// sources even with prefetch set", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "pvc://some-pvc/model.gguf"
			Expect(prefetchEligible(m)).To(BeFalse())
		})

		It("is true for hf:// sources (normalized to an https resolve URL, so prefetchable)", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "hf://org/repo"
			Expect(prefetchEligible(m)).To(BeTrue())
		})

		It("is true for https sources", func() {
			Expect(prefetchEligible(newPrefetchModel("x"))).To(BeTrue())
		})
	})

	Describe("buildPrefetchJob scheduling", func() {
		// #1621: a fleet whose GPU nodes are tainted needs the prefetch Job
		// to tolerate them, because the shared cache can live on node-local
		// storage pinned to exactly those nodes. The Model carries its own
		// tolerations: inheriting from an InferenceService is ill-defined
		// here, since prefetch runs before the first one exists by design.
		It("carries the Model's tolerations onto the Job pod", func() {
			m := newPrefetchModel("tolerated")
			m.Spec.PrefetchTolerations = []corev1.Toleration{{
				Key:      "nvidia.com/gpu",
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			}}
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(m, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Tolerations).To(HaveLen(1))
			Expect(job.Spec.Template.Spec.Tolerations[0].Key).To(Equal("nvidia.com/gpu"))
		})

		It("emits no tolerations when the Model declares none", func() {
			m := newPrefetchModel("plain")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(m, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Tolerations).To(BeEmpty())
		})
	})

	Describe("reconcilePrefetch", func() {
		It("does not handle models without prefetch", func() {
			m := newPrefetchModel("no-prefetch")
			m.Spec.Prefetch = false
			handled, _, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
		})

		It("creates the Job and shared cache PVC, then completes on Job success", func() {
			model := newPrefetchModel("model-prefetch-flow")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			r := prefetchReconciler()

			// First pass: creates PVC + Job, sets Downloading.
			handled, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			job := &batchv1.Job{}
			jobKey := types.NamespacedName{Name: "model-prefetch-flow-prefetch", Namespace: ns}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Kind).To(Equal("Model"))
			Expect(job.Spec.Template.Spec.InitContainers).NotTo(BeEmpty())
			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: ns}, pvc)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseDownloading))
			Expect(updated.Status.CacheKey).NotTo(BeEmpty())

			// Second pass with the Job still running: no duplicate, polls again.
			handled, result, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			// Mark the Job complete; next pass promotes the Model to Ready.
			// k8s >=1.31 validates the Job status subresource: a finished Job
			// needs startTime/completionTime, and Complete=True requires
			// SuccessCriteriaMet=True first.
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseReady))
			Expect(updated.Status.CacheKey).To(Equal(effectiveModelCacheKey(updated)))

			// Once Ready with a cache key, prefetch steps aside for the
			// ordinary remote-source reconcile.
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
		})

		It("marks the Model Failed when the Job fails", func() {
			model := newPrefetchModel("model-prefetch-fail")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			r := prefetchReconciler()
			handled, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			// Failed=True requires FailureTarget=True and a startTime under
			// k8s >=1.31 Job status validation.
			job := &batchv1.Job{}
			jobKey := types.NamespacedName{Name: "model-prefetch-fail-prefetch", Namespace: ns}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: "test"},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: "test"},
			}
			job.Status.Failed = 3
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseFailed))
		})
	})

	Describe("Reconcile integration", func() {
		It("intercepts a prefetch model before the remote download path", func() {
			model := newPrefetchModel("model-prefetch-hook")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			result, err := prefetchReconciler().Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: model.Name, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "model-prefetch-hook-prefetch", Namespace: ns}, job)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseDownloading))
		})
	})

	Describe("#1676 per-service cache claim", func() {
		// Prefetch must stage into the PVC the InferenceService will actually
		// read, not always the shared cache.

		newIsvc := func(modelName, name, claim string) *inferencev1alpha1.InferenceService {
			return &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					ModelCache: &inferencev1alpha1.ModelCacheSpec{
						ClaimName: claim,
					},
				},
			}
		}

		// cleanupNamespacedObjects drains the InferenceServices and PVCs this
		// block creates. It runs BOTH before each case (so a previous case
		// cannot skew a resolution) and after the block (so these objects do
		// not leak into the shared envtest cluster). The AfterEach is
		// load-bearing: federation_edge_controller.go lists InferenceServices
		// across ALL namespaces, and its spec asserts absolute counts, so a
		// leaked service here fails an unrelated test.
		cleanupNamespacedObjects := func() {
			list := &inferencev1alpha1.InferenceServiceList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
			for i := range list.Items {
				_ = k8sClient.Delete(ctx, &list.Items[i])
			}
			pvcList := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(ns))).To(Succeed())
			for i := range pvcList.Items {
				_ = k8sClient.Delete(ctx, &pvcList.Items[i])
			}
		}

		AfterEach(cleanupNamespacedObjects)

		BeforeEach(func() {
			// Clean any services left behind by a previous case.
			list := &inferencev1alpha1.InferenceServiceList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
			for i := range list.Items {
				_ = k8sClient.Delete(ctx, &list.Items[i])
			}
			// Drain PVCs created by prior cases in this namespace.
			pvcList := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(ns))).To(Succeed())
			for i := range pvcList.Items {
				_ = k8sClient.Delete(ctx, &pvcList.Items[i])
			}
		})

		It("mounts the single referencing service's claimName PVC", func() {
			m := newPrefetchModel("per-service-model")
			seedPrefetchCacheKey(m)
			Expect(k8sClient.Create(ctx, newIsvc("per-service-model", "svc-a", "llmkube-node-a-cache"))).To(Succeed())

			// The bug: buildPrefetchJob hardcodes a nil isvc and the shared
			// cache, so the per-service claim is never consulted.
			job, err := prefetchReconciler().buildPrefetchJob(m, nil)
			Expect(err).NotTo(HaveOccurred())
			sharedClaim := sharedClaimName(job)
			Expect(sharedClaim).To(Equal(ModelCachePVCName))

			// The fix: pass the resolved target, and the Job mounts the
			// service's PVC instead.
			target := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "per-service-model", Namespace: ns},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "per-service-model", ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "llmkube-node-a-cache"}},
			}
			goodJob, err := prefetchReconciler().buildPrefetchJob(m, target)
			Expect(err).NotTo(HaveOccurred())
			Expect(sharedClaimName(goodJob)).To(Equal("llmkube-node-a-cache"))
			Expect(sharedClaimName(goodJob)).NotTo(Equal(ModelCachePVCName))
		})

		It("resolves the single distinct claim among referencing services", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "resolve-model", Namespace: ns}}

			Expect(k8sClient.Create(ctx, newIsvc("resolve-model", "svc-a", "llmkube-cache-a"))).To(Succeed())
			target, reason := r.resolvePrefetchTarget(ctx, m)
			Expect(reason).To(BeEmpty())
			Expect(target).NotTo(BeNil())
			Expect(userModelCacheClaimName(target)).To(Equal("llmkube-cache-a"))
			Expect(target.Spec.ModelRef).To(Equal("resolve-model"))
		})

		It("falls back to the shared cache when services disagree on claimName", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "conflict-model", Namespace: ns}}

			Expect(k8sClient.Create(ctx, newIsvc("conflict-model", "svc-a", "llmkube-cache-a"))).To(Succeed())
			Expect(k8sClient.Create(ctx, newIsvc("conflict-model", "svc-b", "llmkube-cache-b"))).To(Succeed())

			target, reason := r.resolvePrefetchTarget(ctx, m)
			Expect(target).To(BeNil())
			Expect(reason).NotTo(BeEmpty())
		})

		It("falls back to the shared cache when no service references the model", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "no-svc-model", Namespace: ns}}

			// A service that references a different model must not match.
			Expect(k8sClient.Create(ctx, newIsvc("other-model", "svc-other", "llmkube-cache-a"))).To(Succeed())

			target, reason := r.resolvePrefetchTarget(ctx, m)
			Expect(target).To(BeNil())
			Expect(reason).NotTo(BeEmpty())
		})
	})
})

// #1621: a fleet whose GPU nodes are tainted needs the prefetch Job to
// tolerate them. Inheriting from an InferenceService is ill-defined here,
// because prefetch runs BEFORE the first InferenceService by design, so the
// Model carries its own tolerations and the Job inherits those.

// sharedClaimName returns the PersistentVolumeClaim name the prefetch Job's
// model-cache volume mounts, by reading the Job's volumes. It is the
// observable artefact of the #1676 fix: the bug mounts the shared cache PVC,
// the fix mounts the per-service claim.
func sharedClaimName(job *batchv1.Job) string {
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "model-cache" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}
