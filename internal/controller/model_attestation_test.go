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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The attestations under testdata/socair are real Socair output (socair scan,
// then socair sign), so these specs feed the controller the producer's bytes,
// not envelopes built from the verifier's own types.
func socairFixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", "socair", name))
	Expect(err).NotTo(HaveOccurred())
	return b
}

func otherPublicKeyPEM() string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	der, err := x509.MarshalPKIXPublicKey(pub)
	Expect(err).NotTo(HaveOccurred())
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// flipPayloadBit returns the envelope with one bit of its signed payload
// changed: the smallest possible tamper.
func flipPayloadBit(env []byte) string {
	var e map[string]any
	Expect(json.Unmarshal(env, &e)).To(Succeed())
	p, err := base64.StdEncoding.DecodeString(e["payload"].(string))
	Expect(err).NotTo(HaveOccurred())
	p[len(p)/2] ^= 1
	e["payload"] = base64.StdEncoding.EncodeToString(p)
	out, err := json.Marshal(e)
	Expect(err).NotTo(HaveOccurred())
	return string(out)
}

var _ = Describe("Model attestation gate", func() {
	const ns = "default"
	attestedSHA := strings.TrimSpace(string(socairFixture("model.sha256")))
	trustedKeys := types.NamespacedName{Namespace: ns, Name: "socair-trusted-keys"}

	var created []client.Object

	track := func(o client.Object) {
		Expect(k8sClient.Create(ctx, o)).To(Succeed())
		created = append(created, o)
	}

	setTrustedKeys := func(pubPEM string) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(ctx, trustedKeys, cm)
		if apierrors.IsNotFound(err) {
			track(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: trustedKeys.Name, Namespace: ns},
				Data:       map[string]string{"operator.pub": pubPEM},
			})
			return
		}
		Expect(err).NotTo(HaveOccurred())
		cm.Data = map[string]string{"operator.pub": pubPEM}
		Expect(k8sClient.Update(ctx, cm)).To(Succeed())
	}

	attestationCM := func(name, envelope string) {
		track(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string]string{"attestation.dsse.json": envelope},
		})
	}

	// A HuggingFace repo-ID source is runtime-resolved: past the gate the
	// controller marks it Ready without downloading, so Ready versus Failed
	// isolates the gate's decision.
	newModel := func(name, sha string, ref *string) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "acme/attested-model",
				SHA256:   sha,
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		if ref != nil {
			m.Spec.Attestation = &inferencev1alpha1.ModelAttestation{
				ConfigMapKeyRef: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: *ref},
					Key:                  "attestation.dsse.json",
				},
			}
		}
		track(m)
		return m
	}

	reconcileModel := func(name string, policy AttestationPolicy) (ctrl.Result, *inferencev1alpha1.Model) {
		r := &ModelReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			StoragePath: GinkgoT().TempDir(),
			Attestation: policy,
		}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		m := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, m)).To(Succeed())
		return result, m
	}

	condition := func(m *inferencev1alpha1.Model, t string) *metav1.Condition {
		for i := range m.Status.Conditions {
			if m.Status.Conditions[i].Type == t {
				return &m.Status.Conditions[i]
			}
		}
		return nil
	}

	enforce := AttestationPolicy{Mode: AttestationEnforce, TrustedKeys: trustedKeys}

	expectRefused := func(m *inferencev1alpha1.Model, result ctrl.Result, why string) {
		Expect(m.Status.Phase).To(Equal(PhaseFailed))
		degraded := condition(m, ConditionDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal(inferencev1alpha1.ReasonModelAttestationRejected))
		Expect(degraded.Message).To(ContainSubstring(why))
		verified := condition(m, ConditionAttestationVerified)
		Expect(verified).NotTo(BeNil())
		Expect(verified.Status).To(Equal(metav1.ConditionFalse))
		// The fix is usually a ConfigMap this controller does not watch, so
		// a refusal must come back on its own.
		Expect(result.RequeueAfter).To(Equal(attestationRetry))
	}

	BeforeEach(func() {
		created = nil
		setTrustedKeys(string(socairFixture("operator.pub")))
	})

	AfterEach(func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = k8sClient.Delete(ctx, created[i])
		}
	})

	It("leaves Models alone when the policy is off", func() {
		newModel("att-off", "", nil)
		_, m := reconcileModel("att-off", AttestationPolicy{})
		Expect(m.Status.Phase).To(Equal(PhaseReady))
		Expect(condition(m, ConditionAttestationVerified)).To(BeNil())
	})

	It("admits a Model whose attestation a trusted key signed for its digest", func() {
		attestationCM("att-good", string(socairFixture("authorized.dsse.json")))
		ref := "att-good"
		newModel("att-good", attestedSHA, &ref)
		_, m := reconcileModel("att-good", enforce)
		Expect(m.Status.Phase).To(Equal(PhaseReady))
		verified := condition(m, ConditionAttestationVerified)
		Expect(verified).NotTo(BeNil())
		Expect(verified.Status).To(Equal(metav1.ConditionTrue))
		Expect(verified.Message).To(ContainSubstring("state authorized"))
	})

	DescribeTable("refuses a Model the policy does not admit",
		func(setup func() (string, string, *string), policy AttestationPolicy, why string) {
			name, sha, ref := setup()
			newModel(name, sha, ref)
			result, m := reconcileModel(name, policy)
			expectRefused(m, result, why)
		},
		Entry("no attestation", func() (string, string, *string) {
			return "att-missing", attestedSHA, nil
		}, enforce, "spec.attestation is not set"),
		Entry("no spec.sha256", func() (string, string, *string) {
			attestationCM("att-nosha", string(socairFixture("authorized.dsse.json")))
			ref := "att-nosha"
			return "att-nosha", "", &ref
		}, enforce, "requires spec.sha256"),
		Entry("attestation ConfigMap missing", func() (string, string, *string) {
			ref := "att-nocm"
			return "att-nocm", attestedSHA, &ref
		}, enforce, "read attestation ConfigMap"),
		Entry("one flipped bit in the signed payload", func() (string, string, *string) {
			attestationCM("att-tamper", flipPayloadBit(socairFixture("authorized.dsse.json")))
			ref := "att-tamper"
			return "att-tamper", attestedSHA, &ref
		}, enforce, "no valid signature"),
		Entry("an attestation for another artifact", func() (string, string, *string) {
			attestationCM("att-digest", string(socairFixture("authorized.dsse.json")))
			ref := "att-digest"
			return "att-digest", strings.Repeat("a", 64), &ref
		}, enforce, "not the artifact"),
		Entry("a withheld attestation", func() (string, string, *string) {
			attestationCM("att-withheld", string(socairFixture("withheld.dsse.json")))
			ref := "att-withheld"
			return "att-withheld", attestedSHA, &ref
		}, enforce, "withheld"),
		Entry("a conditional attestation without allowConditions", func() (string, string, *string) {
			attestationCM("att-cond", string(socairFixture("conditional.dsse.json")))
			ref := "att-cond"
			return "att-cond", attestedSHA, &ref
		}, enforce, "ciso@example.com"),
	)

	It("refuses an attestation signed by a key the cluster does not trust", func() {
		setTrustedKeys(otherPublicKeyPEM())
		attestationCM("att-untrusted", string(socairFixture("authorized.dsse.json")))
		ref := "att-untrusted"
		newModel("att-untrusted", attestedSHA, &ref)
		result, m := reconcileModel("att-untrusted", enforce)
		expectRefused(m, result, "no valid signature by a trusted key")
	})

	It("admits a conditional attestation when allowConditions is set", func() {
		attestationCM("att-cond-ok", string(socairFixture("conditional.dsse.json")))
		ref := "att-cond-ok"
		newModel("att-cond-ok", attestedSHA, &ref)
		_, m := reconcileModel("att-cond-ok", AttestationPolicy{Mode: AttestationEnforce, TrustedKeys: trustedKeys, AllowConditions: true})
		Expect(m.Status.Phase).To(Equal(PhaseReady))
	})

	It("reports but does not block in warn mode", func() {
		newModel("att-warn", attestedSHA, nil)
		_, m := reconcileModel("att-warn", AttestationPolicy{Mode: AttestationWarn, TrustedKeys: trustedKeys})
		Expect(m.Status.Phase).To(Equal(PhaseReady))
		verified := condition(m, ConditionAttestationVerified)
		Expect(verified).NotTo(BeNil())
		Expect(verified.Status).To(Equal(metav1.ConditionFalse))
		Expect(verified.Reason).To(Equal(inferencev1alpha1.ReasonModelAttestationRejected))
	})

	// The gate runs ahead of the Ready short-circuit, so taking a key out of
	// the trusted set refuses a Model that was already admitted.
	It("refuses an admitted Model once its signer is no longer trusted", func() {
		attestationCM("att-revoke", string(socairFixture("authorized.dsse.json")))
		ref := "att-revoke"
		newModel("att-revoke", attestedSHA, &ref)
		_, m := reconcileModel("att-revoke", enforce)
		Expect(m.Status.Phase).To(Equal(PhaseReady))

		setTrustedKeys(otherPublicKeyPEM())
		result, m := reconcileModel("att-revoke", enforce)
		expectRefused(m, result, "no valid signature by a trusted key")
	})

	// The issue's acceptance: deploying a model with a tampered attestation is
	// denied. The refused Model stays Failed, so an InferenceService that
	// references it creates no Deployment.
	It("leaves no Deployment for an InferenceService over a refused Model", func() {
		attestationCM("att-deploy", flipPayloadBit(socairFixture("authorized.dsse.json")))
		ref := "att-deploy"
		newModel("att-deploy", attestedSHA, &ref)
		_, m := reconcileModel("att-deploy", enforce)
		Expect(m.Status.Phase).To(Equal(PhaseFailed))

		replicas := int32(1)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "att-deploy-svc", Namespace: ns},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "att-deploy", Replicas: &replicas},
		}
		track(isvc)
		isr := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		_, err := isr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "att-deploy-svc", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		depErr := k8sClient.Get(ctx, types.NamespacedName{Name: "att-deploy-svc", Namespace: ns}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(depErr)).To(BeTrue(), fmt.Sprintf("a refused Model must not be deployed, got %v", depErr))
	})
})

var _ = Describe("ParseAttestationMode", func() {
	DescribeTable("parses the flag",
		func(in string, want AttestationMode, ok bool) {
			got, err := ParseAttestationMode(in)
			if !ok {
				Expect(err).To(HaveOccurred())
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("empty is off", "", AttestationOff, true),
		Entry("off", "off", AttestationOff, true),
		Entry("warn", "warn", AttestationWarn, true),
		Entry("enforce, any case", "ENFORCE", AttestationEnforce, true),
		Entry("anything else is an error", "strict", AttestationMode(""), false),
	)
})
