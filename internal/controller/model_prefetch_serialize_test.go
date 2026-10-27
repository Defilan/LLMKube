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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// #2250: the cache directory is keyed on the source alone, so two Models on
// one source share it, and on a FUSE claim a prefetch stager writes it without
// flock. Two stagers for the same directory and claim must never run at once:
// the second Model waits, names what it waits for, and starts after.
var _ = Describe("Prefetch stager serialization (#2250)", func() {
	ctx := context.Background()
	const ns = "prefetch-serialize-test"
	const claim = "fuse-cache"

	reconciler := func() *ModelReconciler {
		return &ModelReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteMany",
			ModelCacheMode:       ModelCacheModeShared,
			ModelCachePath:       "/models",
		}
	}
	newModel := func(name, source, claimName string) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:     source,
				Prefetch:   true,
				ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: claimName},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, m) })
		return m
	}
	prefetch := func(r *ModelReconciler, name string) (*inferencev1alpha1.Model, time.Duration) {
		m := &inferencev1alpha1.Model{}
		key := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		_, result, err := r.reconcilePrefetch(ctx, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		return m, result.RequeueAfter
	}
	jobOf := func(m *inferencev1alpha1.Model) (*batchv1.Job, error) {
		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}, job)
		return job, err
	}
	// stagersOn lists the prefetch Jobs that write claim, by name.
	stagersOn := func(claimName string) []string {
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns),
			client.MatchingLabels{"app.kubernetes.io/component": "model-prefetch"})).To(Succeed())
		var names []string
		for i := range jobs.Items {
			if c, _ := prefetchWriteTarget(&jobs.Items[i].Spec.Template.Spec); c == claimName {
				names = append(names, jobs.Items[i].Name)
			}
		}
		return names
	}

	BeforeEach(func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))).To(Succeed())
		createCachePVC(ctx, ns, claim)
		createCachePVC(ctx, ns, "other-cache")
	})
	AfterEach(func() { collectPrefetchJobs(ctx, ns) })

	It("starts the second stager for a source and claim only after the first has finished", func() {
		const source = "https://example.com/models/shared.gguf"
		first := newModel("first", source, claim)
		second := newModel("second", source, claim)
		r := reconciler()

		By("the first Model starting its stager")
		prefetch(r, first.Name)
		firstJob, err := jobOf(first)
		Expect(err).NotTo(HaveOccurred())

		By("the second Model waiting for it, and saying so")
		got, requeue := prefetch(r, second.Name)
		_, err = jobOf(second)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a second stager on the same cache directory and claim must not be created")
		Expect(stagersOn(claim)).To(ConsistOf(firstJob.Name))
		Expect(requeue).To(Equal(prefetchStagerWait))
		waiting := meta.FindStatusCondition(got.Status.Conditions, ConditionPrefetchWaiting)
		Expect(waiting).NotTo(BeNil())
		Expect(waiting.Status).To(Equal(metav1.ConditionTrue))
		Expect(waiting.Reason).To(Equal(ReasonPrefetchWaitingForStager))
		Expect(waiting.Message).To(ContainSubstring(`prefetch job "` + firstJob.Name + `" of Model "first"`))
		progressing := meta.FindStatusCondition(got.Status.Conditions, ConditionProgressing)
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Reason).To(Equal(ReasonPrefetchWaitingForStager))
		entry := findPrefetchStatus(got, claim)
		Expect(entry).NotTo(BeNil())
		Expect(entry.Phase).To(Equal(inferencev1alpha1.PrefetchPhasePending))
		Expect(entry.JobName).To(BeEmpty())

		By("a stager for another claim, or another source, not waiting")
		otherClaim := newModel("other-claim", source, "other-cache")
		prefetch(r, otherClaim.Name)
		_, err = jobOf(otherClaim)
		Expect(err).NotTo(HaveOccurred(), "the same source on another claim is another cache directory")
		otherSource := newModel("other-source", "https://example.com/models/other.gguf", claim)
		prefetch(r, otherSource.Name)
		_, err = jobOf(otherSource)
		Expect(err).NotTo(HaveOccurred(), "another source on the same claim is another cache directory")

		By("the first stager finishing")
		markJobComplete(ctx, client.ObjectKeyFromObject(firstJob))
		got, _ = prefetch(r, second.Name)
		secondJob, err := jobOf(second)
		Expect(err).NotTo(HaveOccurred(), "the second stager starts once the first is done")
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionPrefetchWaiting)).To(BeNil())
		Expect(findPrefetchStatus(got, claim).Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
		Expect(findPrefetchStatus(got, claim).JobName).To(Equal(secondJob.Name))
	})

	// The scenario #2250 reports: a multi-file Hugging Face repository, staged
	// by spec.files, which two Models share on one claim.
	It("serializes stagers of a multi-file hf:// source shared on a claim", func() {
		const source = "hf://org/repo"
		files := []string{"config.json", "model.safetensors"}
		first := newModel("hf-first", source, claim)
		second := newModel("hf-second", source, claim)
		for _, m := range []*inferencev1alpha1.Model{first, second} {
			m.Spec.Format = "safetensors"
			m.Spec.Files = files
			Expect(k8sClient.Update(ctx, m)).To(Succeed())
		}
		r := reconciler()

		prefetch(r, first.Name)
		firstJob, err := jobOf(first)
		Expect(err).NotTo(HaveOccurred())
		Expect(prefetchJobIsStager(firstJob)).To(BeTrue(), "the case must exercise a multi-file stager")

		got, requeue := prefetch(r, second.Name)
		_, err = jobOf(second)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a second multi-file stager on the same cache directory and claim must not be created")
		Expect(stagersOn(claim)).To(ConsistOf(firstJob.Name))
		Expect(requeue).To(Equal(prefetchStagerWait))
		waiting := meta.FindStatusCondition(got.Status.Conditions, ConditionPrefetchWaiting)
		Expect(waiting).NotTo(BeNil())
		Expect(waiting.Message).To(ContainSubstring(`prefetch job "` + firstJob.Name + `" of Model "hf-first"`))

		markJobComplete(ctx, client.ObjectKeyFromObject(firstJob))
		got, _ = prefetch(r, second.Name)
		secondJob, err := jobOf(second)
		Expect(err).NotTo(HaveOccurred(), "the second stager starts once the first is done")
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionPrefetchWaiting)).To(BeNil())
		Expect(findPrefetchStatus(got, claim).JobName).To(Equal(secondJob.Name))
	})

	It("waits for a stager Job that is still being deleted", func() {
		const source = "https://example.com/models/deleting.gguf"
		first := newModel("deleting-first", source, claim)
		second := newModel("deleting-second", source, claim)
		r := reconciler()
		prefetch(r, first.Name)
		firstJob, err := jobOf(first)
		Expect(err).NotTo(HaveOccurred())
		markJobFailed(ctx, client.ObjectKeyFromObject(firstJob))
		// A finished stager in foreground deletion may still have a pod
		// writing; the Job is held until that pod is gone.
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(firstJob), firstJob)).To(Succeed())
		Expect(deletePrefetchJob(ctx, k8sClient, firstJob)).To(Succeed())

		prefetch(r, second.Name)
		_, err = jobOf(second)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		finishJobDeletion(ctx, client.ObjectKeyFromObject(firstJob))
		prefetch(r, second.Name)
		_, err = jobOf(second)
		Expect(err).NotTo(HaveOccurred())
	})

	// Two Models reconciling at once on different workers: the second must
	// not read "no stager" while the first is between its check and its
	// create. The first Model's Create is held until the second has listed
	// the Jobs (or a deadline passes), which is the interleaving that lets
	// both through when the check and the create are not one step.
	It("does not let two concurrent reconciles both start a stager", func() {
		const source = "https://example.com/models/concurrent.gguf"
		first := newModel("concurrent-first", source, claim)
		second := newModel("concurrent-second", source, claim)

		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		firstCreating := make(chan struct{})
		secondListed := make(chan struct{})
		var closeListed sync.Once
		intercepted := interceptor.NewClient(base, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if job, ok := obj.(*batchv1.Job); ok && job.Name == prefetchJobName(first) {
					close(firstCreating)
					select {
					case <-secondListed:
					case <-time.After(2 * time.Second):
					}
				}
				return c.Create(ctx, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				err := c.List(ctx, list, opts...)
				if _, ok := list.(*batchv1.JobList); ok {
					select {
					case <-firstCreating:
						closeListed.Do(func() { close(secondListed) })
					default:
					}
				}
				return err
			},
		})
		r := reconciler()
		r.Client = intercepted
		r.APIReader = intercepted

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			prefetch(r, first.Name)
		}()
		Eventually(firstCreating, 5*time.Second).Should(BeClosed())
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			prefetch(r, second.Name)
		}()
		wg.Wait()

		Expect(stagersOn(claim)).To(ConsistOf(prefetchJobName(first)),
			"only one stager may write the cache directory on the claim")
		got := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(second), got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionPrefetchWaiting)).To(BeTrue())
	})
})
