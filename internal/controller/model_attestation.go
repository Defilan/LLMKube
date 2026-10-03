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
	"context"
	"fmt"
	"strings"
	"time"

	verify "github.com/defilantech/socair-verify"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// AttestationMode is how the Model controller treats Socair attestations.
type AttestationMode string

const (
	// AttestationOff ignores attestations (the default).
	AttestationOff AttestationMode = "off"
	// AttestationWarn verifies and reports on the AttestationVerified
	// condition, but never blocks a Model.
	AttestationWarn AttestationMode = "warn"
	// AttestationEnforce fails a Model whose attestation is missing or not
	// admitted, so it never becomes Ready and no InferenceService serves it.
	AttestationEnforce AttestationMode = "enforce"
)

// ConditionAttestationVerified reports the attestation result on a Model when
// the policy is warn or enforce.
const ConditionAttestationVerified = "AttestationVerified"

// attestationRetry is how soon a Model refused for its attestation is looked
// at again. The usual fix is creating or correcting a ConfigMap, which this
// controller does not watch, so a refusal requeues rather than waiting for the
// Model itself to change.
const attestationRetry = time.Minute

// AttestationPolicy is the operator-configured policy for Model attestations.
type AttestationPolicy struct {
	Mode AttestationMode
	// TrustedKeys is the ConfigMap whose values under keys ending in ".pub"
	// are the PEM Ed25519 public keys whose signatures are trusted.
	TrustedKeys types.NamespacedName
	// AllowConditions admits an authorized_with_conditions attestation (some
	// checks NOT_TESTED, accepted by a named person) as well as authorized.
	AllowConditions bool
	// MaxAge, when positive, refuses an attestation issued longer ago.
	MaxAge time.Duration
}

// ParseAttestationMode validates the --model-attestation flag.
func ParseAttestationMode(s string) (AttestationMode, error) {
	switch m := AttestationMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", AttestationOff:
		return AttestationOff, nil
	case AttestationWarn, AttestationEnforce:
		return m, nil
	}
	return "", fmt.Errorf("invalid --model-attestation %q: want off, warn, or enforce", s)
}

// checkAttestation applies the attestation policy before any source is
// fetched. It runs on every reconcile, ahead of the Ready short-circuit, so a
// key removed from the trusted set or an attestation replaced in its
// ConfigMap takes effect on the next reconcile.
//
// The gate is in the controller rather than an admission webhook because the
// Model webhooks are failurePolicy=Ignore: an unreachable webhook would let an
// unattested Model through. The spec fields it reads (sha256, attestation) are
// client-controlled; everything that decides admission (the trusted keys and
// the policy) is operator-controlled.
func (r *ModelReconciler) checkAttestation(ctx context.Context, model *inferencev1alpha1.Model) (handled bool, result ctrl.Result, err error) {
	p := r.Attestation
	if p.Mode == "" || p.Mode == AttestationOff {
		return false, ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)

	att, rejectErr := r.admitAttestation(ctx, model)
	if rejectErr == nil {
		msg := fmt.Sprintf("signed by trusted key %s, state %s, attestation %s", shortKeyID(att.KeyID), att.State, att.DocumentID)
		if !conditionIs(model, ConditionAttestationVerified, metav1.ConditionTrue, msg) {
			if statusErr := r.updateStatus(ctx, model, ConditionAttestationVerified, metav1.ConditionTrue, "Verified", msg); statusErr != nil {
				return true, ctrl.Result{}, statusErr
			}
		}
		return false, ctrl.Result{}, nil
	}

	msg := "attestation rejected: " + rejectErr.Error()
	if p.Mode == AttestationWarn {
		logger.Info("model attestation not admitted (warn mode, not blocking)", "reason", rejectErr.Error())
		if !conditionIs(model, ConditionAttestationVerified, metav1.ConditionFalse, msg) {
			if statusErr := r.updateStatus(ctx, model, ConditionAttestationVerified, metav1.ConditionFalse, inferencev1alpha1.ReasonModelAttestationRejected, msg); statusErr != nil {
				return true, ctrl.Result{}, statusErr
			}
		}
		return false, ctrl.Result{}, nil
	}

	logger.Error(rejectErr, "model refused by attestation policy")
	llmkubemetrics.ReconcileTotal.WithLabelValues("model", "error").Inc()
	model.Status.Phase = PhaseFailed
	setCondition(model, ConditionAttestationVerified, metav1.ConditionFalse, inferencev1alpha1.ReasonModelAttestationRejected, msg)
	if statusErr := r.updateStatus(ctx, model, ConditionDegraded, metav1.ConditionTrue, inferencev1alpha1.ReasonModelAttestationRejected, msg); statusErr != nil {
		return true, ctrl.Result{}, statusErr
	}
	return true, ctrl.Result{RequeueAfter: attestationRetry}, nil
}

// admitAttestation loads the trusted keys and the Model's attestation and
// applies the policy. Every error names what to fix.
func (r *ModelReconciler) admitAttestation(ctx context.Context, model *inferencev1alpha1.Model) (*verify.Attestation, error) {
	if model.Spec.SHA256 == "" {
		return nil, fmt.Errorf("the attestation policy requires spec.sha256, the digest the attestation must be for")
	}
	if model.Spec.Attestation == nil {
		return nil, fmt.Errorf("spec.attestation is not set; sign the model's Socair report with `socair sign` and reference the envelope from a ConfigMap")
	}
	reader := r.attestationReader()

	var keysCM corev1.ConfigMap
	if err := reader.Get(ctx, r.Attestation.TrustedKeys, &keysCM); err != nil {
		return nil, fmt.Errorf("read trusted keys ConfigMap %s: %w", r.Attestation.TrustedKeys, err)
	}
	var pems [][]byte
	for k, v := range keysCM.Data {
		if strings.HasSuffix(k, ".pub") {
			pems = append(pems, []byte(v))
		}
	}
	for k, v := range keysCM.BinaryData {
		if strings.HasSuffix(k, ".pub") {
			pems = append(pems, v)
		}
	}
	ring, err := verify.ParseKeyring(pems...)
	if err != nil {
		return nil, fmt.Errorf("trusted keys ConfigMap %s: %w", r.Attestation.TrustedKeys, err)
	}

	ref := model.Spec.Attestation.ConfigMapKeyRef
	var envCM corev1.ConfigMap
	if err := reader.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: ref.Name}, &envCM); err != nil {
		return nil, fmt.Errorf("read attestation ConfigMap %s/%s: %w", model.Namespace, ref.Name, err)
	}
	envelope, ok := envCM.Data[ref.Key]
	if !ok {
		b, bok := envCM.BinaryData[ref.Key]
		if !bok {
			return nil, fmt.Errorf("attestation ConfigMap %s/%s has no key %q", model.Namespace, ref.Name, ref.Key)
		}
		envelope = string(b)
	}

	policy := verify.Policy{Keys: ring, AllowConditions: r.Attestation.AllowConditions, MaxAge: r.Attestation.MaxAge}
	return policy.Admit([]byte(envelope), model.Spec.SHA256)
}

// attestationReader reads the two ConfigMaps uncached when the manager's API
// reader is wired, so the controller does not hold an informer over every
// ConfigMap in the cluster.
func (r *ModelReconciler) attestationReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func shortKeyID(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}

// conditionIs reports whether the Model already carries this condition, so a
// steady-state reconcile does not rewrite status every time.
func conditionIs(model *inferencev1alpha1.Model, condType string, status metav1.ConditionStatus, message string) bool {
	for _, c := range model.Status.Conditions {
		if c.Type == condType {
			return c.Status == status && c.Message == message && c.ObservedGeneration == model.Generation
		}
	}
	return false
}

// setCondition sets a condition in memory; the caller persists it with the
// next status update.
func setCondition(model *inferencev1alpha1.Model, condType string, status metav1.ConditionStatus, reason, message string) {
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: model.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	for i, c := range model.Status.Conditions {
		if c.Type == condType {
			model.Status.Conditions[i] = cond
			return
		}
	}
	model.Status.Conditions = append(model.Status.Conditions, cond)
}
