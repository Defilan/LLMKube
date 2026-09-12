package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// envtest has no kubelet, so member pods stay Pending. These specs cover what
// the reconciler owns: creation, ownership, no Deployment, status shape, and
// the recreate paths. Readiness math is unit-tested in multinode_test.go.
var _ = Describe("multiNode group reconcile", func() {
	const (
		ns        = "default"
		modelName = "mn-model"
		isvcName  = "mn-ring"
	)
	var (
		ctx        context.Context
		reconciler *InferenceServiceReconciler
	)

	key := types.NamespacedName{Name: isvcName, Namespace: ns}
	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}
	memberPods := func() []corev1.Pod {
		var list corev1.PodList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{LabelMultiNodeGroup: isvcName})).To(Succeed())
		return list.Items
	}
	// finishTerminating plays the kubelet: envtest has none, so a pod the
	// reconciler deleted sits Terminating forever until force-removed.
	finishTerminating := func() {
		for _, p := range memberPods() {
			if p.DeletionTimestamp == nil {
				continue
			}
			pod := p
			_ = k8sClient.Delete(ctx, &pod, client.GracePeriodSeconds(0))
		}
	}
	memberPodNames := func() []string {
		pods := memberPods()
		names := make([]string, 0, len(pods))
		for _, p := range pods {
			names = append(names, p.Name)
		}
		return names
	}

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			ModelCachePath:     "/models",
			ModelCacheMode:     ModelCacheModePerService,
		}
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "s3://models/org/name", Format: "safetensors",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cuda", GPU: &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"}},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		model.Status.CacheKey = "org-name"
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, model) })

		// The chart ships the llmkube-* PriorityClasses; envtest has none, and
		// unlike a Deployment a directly created pod is validated against it.
		pc := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "llmkube-normal"}, Value: 1000}
		if err := k8sClient.Create(ctx, pc); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		// Claims are user-owned: the service claim is checked by
		// ensureModelCachePVC, member claims by checkMemberClaims.
		for _, name := range []string{"mn-claim-0", "mn-claim-1"} {
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
				},
			}
			// envtest runs no pvc-protection controller, so a deleted claim
			// never finishes terminating: create once, keep for the suite.
			if err := k8sClient.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		}

		tp := int32(2)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: ns},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime: "vllm", ModelRef: modelName,
				Resources:  &inferencev1alpha1.InferenceResourceRequirements{GPU: 1},
				ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "mn-claim-0"},
				VLLMConfig: &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp},
				MultiNode: &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{
					{Node: "node-a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
					{Node: "node-b", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.2"}, ModelCache: &inferencev1alpha1.MultiNodeMemberCache{ClaimName: "mn-claim-1"}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, isvc)
			// envtest runs no garbage collector: remove the members by hand
			// and wait for them to go so the next spec starts clean.
			for _, p := range memberPods() {
				pod := p
				_ = k8sClient.Delete(ctx, &pod, client.GracePeriodSeconds(0))
			}
			Eventually(memberPods).Should(BeEmpty())
			Eventually(func() bool {
				var got inferencev1alpha1.InferenceService
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &got))
			}).Should(BeTrue())
		})
	})

	It("creates one owned pod per member and no Deployment", func() {
		reconcileOnce()
		pods := memberPods()
		Expect(pods).To(HaveLen(2))

		var owner inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &owner)).To(Succeed())
		for _, p := range pods {
			pod := p
			Expect(metav1.IsControlledBy(&pod, &owner)).To(BeTrue(), "%s must be controlled by the ISVC", pod.Name)
			Expect(pod.Spec.HostNetwork).To(BeTrue())
		}
		Expect(memberPodNames()).To(ConsistOf("mn-ring-mn-0", "mn-ring-mn-1"))

		var dep appsv1.Deployment
		err := k8sClient.Get(ctx, key, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Deployment for a multiNode service")

		Expect(owner.Status.MultiNode).NotTo(BeNil())
		Expect(owner.Status.MultiNode.Size).To(Equal(int32(2)))
		Expect(owner.Status.MultiNode.Members).To(HaveLen(2))
		Expect(owner.Status.MultiNode.Members[1].Node).To(Equal("node-b"))
		cond := meta.FindStatusCondition(owner.Status.Conditions, ConditionMultiNodeGroupReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(owner.Status.Phase).NotTo(Equal(PhaseReady))
	})

	It("recreates the whole group when a member fails", func() {
		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2))

		var worker corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-1", Namespace: ns}, &worker)).To(Succeed())
		worker.Status.Phase = corev1.PodFailed
		Expect(k8sClient.Status().Update(ctx, &worker)).To(Succeed())

		reconcileOnce() // observes the failure, deletes the group
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())

		var mid inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &mid)).To(Succeed())
		cond := meta.FindStatusCondition(mid.Status.Conditions, ConditionMultiNodeGroupReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("MemberFailed"))

		reconcileOnce() // recreates
		Expect(memberPods()).To(HaveLen(2))
	})

	It("recreates the group when the spec changes", func() {
		reconcileOnce()
		before := memberPods()
		Expect(before).To(HaveLen(2))
		oldHash := before[0].Annotations[AnnotationMultiNodeGroupHash]
		Expect(oldHash).NotTo(BeEmpty())

		var isvc inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &isvc)).To(Succeed())
		isvc.Spec.MultiNode.Members[1].Fabric.SocketInterface = "enp9"
		Expect(k8sClient.Update(ctx, &isvc)).To(Succeed())

		reconcileOnce() // stale -> delete
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())
		reconcileOnce() // recreate
		after := memberPods()
		Expect(after).To(HaveLen(2))
		Expect(after[0].Annotations[AnnotationMultiNodeGroupHash]).NotTo(Equal(oldHash))
	})

	It("fails clearly when a member claim is missing", func() {
		var isvc inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &isvc)).To(Succeed())
		isvc.Spec.MultiNode.Members[1].ModelCache.ClaimName = "does-not-exist"
		Expect(k8sClient.Update(ctx, &isvc)).To(Succeed())

		reconcileOnce()
		Expect(memberPods()).To(BeEmpty())
		var got inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		degraded := meta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Message).To(ContainSubstring(`members[1].modelCache.claimName "does-not-exist" not found`))
	})

	It("rejects a world size that does not fill the group", func() {
		var isvc inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &isvc)).To(Succeed())
		one := int32(1)
		isvc.Spec.VLLMConfig.TensorParallelSize = &one
		Expect(k8sClient.Update(ctx, &isvc)).To(Succeed())

		reconcileOnce()
		Expect(memberPods()).To(BeEmpty())
		var got inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		degraded := meta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Message).To(ContainSubstring("multiNode group provides 2 GPUs"))
	})

	It("holds the group while a member stages, then polices post-formation restarts", func() {
		// #1757: one member's claim starts empty, so its init container is
		// still downloading while the other member's runtime waits for the
		// rendezvous, exits, and is restarted by the kubelet. Recreating the
		// group on that exit kills the downloader and loses its progress,
		// which never converges: 62 recreates in 20 minutes in the field.
		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2))

		// mn-1 is staging: init container running, runtime never started.
		var staging corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-1", Namespace: ns}, &staging)).To(Succeed())
		staging.Status.Phase = corev1.PodPending
		staging.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  "model-downloader",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}}
		Expect(k8sClient.Status().Update(ctx, &staging)).To(Succeed())

		// mn-0 started, could not rendezvous, exited, was restarted.
		var waiting corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-0", Namespace: ns}, &waiting)).To(Succeed())
		waiting.Status.Phase = corev1.PodRunning
		waiting.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "vllm", RestartCount: 2,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error", FinishedAt: metav1.Now(),
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, &waiting)).To(Succeed())

		reconcileOnce()
		Expect(memberPods()).To(HaveLen(2), "formation noise must not tear the group down while a member is staging")
		var held inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &held)).To(Succeed())
		Expect(meta.FindStatusCondition(held.Status.Conditions, ConditionMultiNodeFormed)).To(BeNil(),
			"a group with a staging member is not formed")

		// Staging completes: the downloader's init container exits 0 and the
		// runtime starts. The group forms, stamping a fresh timestamp.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-1", Namespace: ns}, &staging)).To(Succeed())
		staging.Status.Phase = corev1.PodRunning
		staging.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  "model-downloader",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Now()}},
		}}
		staging.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "vllm",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}}
		Expect(k8sClient.Status().Update(ctx, &staging)).To(Succeed())

		reconcileOnce()
		var formed inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &formed)).To(Succeed())
		formCond := meta.FindStatusCondition(formed.Status.Conditions, ConditionMultiNodeFormed)
		Expect(formCond).NotTo(BeNil())
		Expect(formCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(memberPods()).To(HaveLen(2), "the pre-formation exits finished at or before the formation timestamp")

		// A termination strictly after formation is a real crash and must
		// recreate, proving the masking is not permanent. The timestamp is
		// nudged into the future so the ordering against the stamp cannot race
		// the test clock.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mn-ring-mn-0", Namespace: ns}, &waiting)).To(Succeed())
		waiting.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "vllm", RestartCount: 3,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(time.Now().Add(time.Second)),
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, &waiting)).To(Succeed())

		reconcileOnce() // observes the post-formation crash, deletes the group
		finishTerminating()
		Eventually(memberPods).Should(BeEmpty())
		var torn inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &torn)).To(Succeed())
		cond := meta.FindStatusCondition(torn.Status.Conditions, ConditionMultiNodeGroupReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("MemberRestarted"))
		Expect(meta.FindStatusCondition(torn.Status.Conditions, ConditionMultiNodeFormed)).To(BeNil(),
			"teardown clears the formation timestamp so the next group forms afresh")
	})
})
