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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// A ConfigMap volume resolves in the pod's namespace. With --ca-cert-configmap
// set and the ConfigMap absent from the service's namespace, the pod used to sit
// in ContainerCreating/FailedMount while the InferenceService said only
// "Creating". These specs pin the replacement: no workload is created, the
// object names the missing ConfigMap and namespace, and it recovers on its own
// once the ConfigMap is copied in.

func caConfigMap(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"},
	}
}

// drainEvents returns what the fake recorder holds without blocking.
func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

var _ = Describe("CA ConfigMap preflight (InferenceService Deployment path)", func() {
	const ns = "default"
	var (
		ctx        context.Context
		reconciler *InferenceServiceReconciler
		recorder   *events.FakeRecorder
		caName     string
		modelName  string
		isvcName   string
		key        types.NamespacedName
	)

	BeforeEach(func() {
		ctx = context.Background()
		suffix := rand.String(6)
		// A per-spec ConfigMap name keeps the namespace free of it until the
		// spec creates it, without needing a namespace per spec.
		caName = "corp-ca-" + suffix
		modelName = "ca-model-" + suffix
		isvcName = "ca-isvc-" + suffix
		key = types.NamespacedName{Name: isvcName, Namespace: ns}

		recorder = events.NewFakeRecorder(20)
		reconciler = &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			Recorder:           recorder,
			CACertConfigMap:    caName,
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	})

	createService := func(source string) {
		GinkgoHelper()
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   source,
				Format:   "gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		model.Status.CacheKey = "ca-" + modelName
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, model) })

		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: ns},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: modelName,
				Replicas: ptr.To(int32(1)),
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, isvc)
			_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: ns}})
		})
	}

	reconcileOnce := func() reconcile.Result {
		GinkgoHelper()
		res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	getService := func() *inferencev1alpha1.InferenceService {
		GinkgoHelper()
		var got inferencev1alpha1.InferenceService
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		return &got
	}

	deploymentExists := func() bool {
		GinkgoHelper()
		var dep appsv1.Deployment
		err := k8sClient.Get(ctx, key, &dep)
		if apierrors.IsNotFound(err) {
			return false
		}
		Expect(err).NotTo(HaveOccurred())
		return true
	}

	It("holds the Deployment and names the ConfigMap and namespace, then proceeds once it exists", func() {
		createService("https://example.com/model.gguf")

		res := reconcileOnce()

		Expect(deploymentExists()).To(BeFalse(),
			"a Deployment whose pod cannot mount the CA would wedge in ContainerCreating on a GPU")
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "a blocked service must be retried")

		got := getService()
		Expect(got.Status.Phase).To(Equal("Pending"))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionCACertConfigMapAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonCACertConfigMapMissing))
		Expect(cond.Message).To(ContainSubstring(caName))
		Expect(cond.Message).To(ContainSubstring(`namespace "` + ns + `"`))
		progressing := meta.FindStatusCondition(got.Status.Conditions, "Progressing")
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Message).To(ContainSubstring(caName))

		evts := drainEvents(recorder)
		Expect(evts).To(ContainElement(SatisfyAll(
			HavePrefix("Warning "+ReasonCACertConfigMapMissing),
			ContainSubstring(caName),
		)))

		// A second blocked pass must not repeat the event: one per episode.
		reconcileOnce()
		Expect(deploymentExists()).To(BeFalse())
		for _, e := range drainEvents(recorder) {
			Expect(e).NotTo(ContainSubstring(ReasonCACertConfigMapMissing), "Warning repeated on a requeue: %s", e)
		}

		By("copying the ConfigMap into the namespace")
		cm := caConfigMap(caName, ns)
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cm) })

		reconcileOnce()

		Expect(deploymentExists()).To(BeTrue(), "the service must proceed once the ConfigMap exists")
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
		Expect(podSpecMountsConfigMap(&dep.Spec.Template.Spec, caName)).To(BeTrue(),
			"the CA stays a mandatory mount; the fix must not drop it")
		got = getService()
		Expect(got.Status.Phase).NotTo(Equal("Pending"))
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionCACertConfigMapAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(ReasonCACertConfigMapFound))

		By("deleting the ConfigMap under a running service")
		Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		reconcileOnce()
		Expect(deploymentExists()).To(BeTrue(), "an existing Deployment is never removed for a missing CA")
		got = getService()
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionCACertConfigMapAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse),
			"a replacement pod would wedge; the condition must say why")
	})

	It("does not block a service whose pod mounts no CA volume", func() {
		// pvc:// mounts the pre-staged artifact directly; no downloader, no CA.
		createService("pvc://ca-preflight-weights/model.gguf")

		reconcileOnce()

		Expect(deploymentExists()).To(BeTrue(), "the preflight must only gate pods that mount the ConfigMap")
		got := getService()
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionCACertConfigMapAvailable)).To(BeNil())
	})

	It("maps only the operator's CA ConfigMap to the services in its namespace", func() {
		createService("https://example.com/model.gguf")

		Expect(reconciler.findInferenceServicesForCACertConfigMap(ctx, caConfigMap("unrelated", ns))).To(BeEmpty())
		Expect(reconciler.findInferenceServicesForCACertConfigMap(ctx, caConfigMap(caName, "kube-system"))).
			NotTo(ContainElement(reconcile.Request{NamespacedName: key}))
		Expect(reconciler.findInferenceServicesForCACertConfigMap(ctx, caConfigMap(caName, ns))).
			To(ContainElement(reconcile.Request{NamespacedName: key}))
	})
})

var _ = Describe("CA ConfigMap preflight (Model prefetch Job)", func() {
	const ns = "prefetch-test"
	ctx := context.Background()

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if err := k8sClient.Create(ctx, nsObj); err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}
	})

	It("holds the prefetch Job and names the ConfigMap, then starts it once it exists", func() {
		suffix := rand.String(6)
		caName := "corp-ca-" + suffix
		recorder := events.NewFakeRecorder(20)
		r := &ModelReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			Recorder:             recorder,
			CACertConfigMap:      caName,
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
		}
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "ca-prefetch-" + suffix, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/models/llama.gguf",
				Prefetch: true,
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, model) })
		modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
		jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns}})
		})

		handled, result, err := r.reconcilePrefetch(ctx, model)
		Expect(err).NotTo(HaveOccurred())
		Expect(handled).To(BeTrue())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		var job batchv1.Job
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &job))).To(BeTrue(),
			"a Job whose pod cannot mount the CA neither completes nor fails")

		var got inferencev1alpha1.Model
		Expect(k8sClient.Get(ctx, modelKey, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal("Pending"))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionCACertConfigMapAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring(caName))
		Expect(cond.Message).To(ContainSubstring(`namespace "` + ns + `"`))
		progressing := meta.FindStatusCondition(got.Status.Conditions, ConditionProgressing)
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Reason).To(Equal(ReasonCACertConfigMapMissing))
		Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ReasonCACertConfigMapMissing)))

		By("copying the ConfigMap into the namespace")
		cm := caConfigMap(caName, ns)
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cm) })

		handled, _, err = r.reconcilePrefetch(ctx, &got)
		Expect(err).NotTo(HaveOccurred())
		Expect(handled).To(BeTrue())

		Expect(k8sClient.Get(ctx, jobKey, &job)).To(Succeed())
		Expect(podSpecMountsConfigMap(&job.Spec.Template.Spec, caName)).To(BeTrue())
		Expect(k8sClient.Get(ctx, modelKey, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(PhaseDownloading))
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionCACertConfigMapAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	// The hold must settle. The Model controller has no predicate, so every
	// status write re-triggers it; a reconcile that writes Progressing=True
	// (PrefetchCacheIgnored, the normal no-claim-override case) and then
	// Progressing=False (CA missing) changes the object every time and loops,
	// about 100 writes a second in kind. Asserting on the writes, not the
	// resourceVersion, keeps this independent of LastTransitionTime's
	// one-second resolution.
	It("writes only the hold condition while the ConfigMap is missing, so the reconcile settles", func() {
		suffix := rand.String(6)
		var progressingWrites []metav1.ConditionStatus
		// interceptor wraps a WithWatch client; build one on the same apiserver.
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		counting := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if m, ok := obj.(*inferencev1alpha1.Model); ok && sub == "status" {
					if p := meta.FindStatusCondition(m.Status.Conditions, ConditionProgressing); p != nil {
						progressingWrites = append(progressingWrites, p.Status)
					}
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		})
		r := &ModelReconciler{
			Client:               counting,
			Scheme:               k8sClient.Scheme(),
			Recorder:             events.NewFakeRecorder(20),
			CACertConfigMap:      "corp-ca-" + suffix,
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
		}
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "ca-prefetch-settle-" + suffix, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/models/llama.gguf",
				Prefetch: true,
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, model) })
		modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}

		for i := 0; i < 2; i++ {
			var got inferencev1alpha1.Model
			Expect(k8sClient.Get(ctx, modelKey, &got)).To(Succeed())
			handled, _, err := r.reconcilePrefetch(ctx, &got)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
		}

		Expect(progressingWrites).NotTo(BeEmpty())
		Expect(progressingWrites).To(HaveEach(metav1.ConditionFalse),
			"a Progressing=True write on the hold path flips the condition back and forth on every reconcile")
	})
})

func TestPodSpecMountsConfigMap(t *testing.T) {
	cmVol := func(name string, optional *bool) corev1.Volume {
		return corev1.Volume{Name: "v-" + name, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name}, Optional: optional,
		}}}
	}
	cases := []struct {
		name string
		spec *corev1.PodSpec
		cm   string
		want bool
	}{
		{"nil spec", nil, "ca", false},
		{"flag unset", &corev1.PodSpec{Volumes: []corev1.Volume{cmVol("ca", nil)}}, "", false},
		{"mounted", &corev1.PodSpec{Volumes: []corev1.Volume{cmVol("other", nil), cmVol("ca", nil)}}, "ca", true},
		{"explicitly required", &corev1.PodSpec{Volumes: []corev1.Volume{cmVol("ca", ptr.To(false))}}, "ca", true},
		// The kubelet starts a pod without an optional ConfigMap, so it cannot wedge.
		{"optional", &corev1.PodSpec{Volumes: []corev1.Volume{cmVol("ca", ptr.To(true))}}, "ca", false},
		{"other ConfigMap only", &corev1.PodSpec{Volumes: []corev1.Volume{cmVol("other", nil)}}, "ca", false},
		{"no volumes", &corev1.PodSpec{}, "ca", false},
	}
	for _, tc := range cases {
		if got := podSpecMountsConfigMap(tc.spec, tc.cm); got != tc.want {
			t.Errorf("%s: podSpecMountsConfigMap = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCACertConfigMapMissingMessageNamesTheFix(t *testing.T) {
	msg := caCertConfigMapMissingMessage("corp-ca", "team-a")
	for _, want := range []string{`"corp-ca"`, `namespace "team-a"`, "kubectl -n team-a create configmap corp-ca"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}
