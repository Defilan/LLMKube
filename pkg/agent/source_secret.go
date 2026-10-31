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

package agent

// Which Secrets a Model's spec.sourceSecretRef may make the agent read (#1956).
// The agent's identity can read every Secret in the namespaces it serves (the
// relay token Secret among them), and anyone who can write a Model chooses the
// name, so without a gate a Model author could point the agent's downloader at
// a Secret that was never meant for it, with an AWS_ENDPOINT_URL or
// HF_ENDPOINT of their choosing. The Secret's owner opts it in with the
// inference.llmkube.dev/model-source-credentials=true label; the relay token
// Secret is never read as model-source credentials.

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// EventReasonSourceSecretUnlabeled is the Warning Event the agent emits on each
// start that reads an unmarked sourceSecretRef Secret only because
// --allow-unlabeled-source-secrets is set. It is not a refusal (the start
// goes ahead), so it is not in MetalAgentRefusalReasons.
const EventReasonSourceSecretUnlabeled = "SourceSecretUnlabeled"

// allowUnlabeledSourceSecretsFlag names the deprecated escape hatch in log
// lines and messages.
//
//nolint:gosec // G101: flag name, not a credential value
const allowUnlabeledSourceSecretsFlag = "--allow-unlabeled-source-secrets"

// SourceSecretNotAllowedError reports that a Model's sourceSecretRef names a
// Secret the agent will not read as model-source credentials. The agent
// refuses the start with ReasonSourceSecretNotAllowed rather than downloading
// without the credentials, so a Model whose Secret is refused never silently
// falls back to an anonymous request.
type SourceSecretNotAllowedError struct {
	Namespace string
	Name      string
	// Why is the sentence explaining the refusal and the operator's fix.
	Why string
}

func (e *SourceSecretNotAllowedError) Error() string {
	return fmt.Sprintf("spec.sourceSecretRef names Secret %q in namespace %q: %s", e.Name, e.Namespace, e.Why)
}

// labelCommand is the exact command that marks key's Secret, quoted in the
// refusal message so the operator does not have to look the label up.
func labelCommand(key types.NamespacedName) string {
	return fmt.Sprintf("kubectl label secret %s -n %s %s=true",
		key.Name, key.Namespace, inferencev1alpha1.LabelModelSourceCredentials)
}

// checkSourceSecretName refuses, by name alone and so before any read, the
// relay token Secret: it authenticates the relay to this agent's ingress and
// is never model-source credentials, so neither the marker nor the escape
// flag lets a Model name it.
func checkSourceSecretName(key types.NamespacedName) error {
	if key.Name == inferencev1alpha1.MetalRelaySecretName {
		return &SourceSecretNotAllowedError{Namespace: key.Namespace, Name: key.Name,
			Why: "it is the Metal relay token Secret, which the agent never reads as model-source credentials; " +
				"point sourceSecretRef at a Secret holding only the model-source keys (AWS_*, HF_TOKEN, HF_ENDPOINT)"}
	}
	return nil
}

// checkSourceSecretLabels decides from an existing Secret's labels whether
// the agent may read it. It returns unlabeled=true, and no error, for an
// unmarked Secret only when allowUnlabeled (the deprecated escape flag) is
// set, so the caller can warn about it.
func checkSourceSecretLabels(
	key types.NamespacedName, labels map[string]string, allowUnlabeled bool,
) (unlabeled bool, err error) {
	if labels[inferencev1alpha1.LabelModelSourceCredentials] == "true" {
		return false, nil
	}
	if allowUnlabeled {
		return true, nil
	}
	return false, &SourceSecretNotAllowedError{Namespace: key.Namespace, Name: key.Name,
		Why: fmt.Sprintf("it is not marked for model-source credentials; if this Secret is meant for model "+
			"downloads, mark it: %s", labelCommand(key))}
}

// readSourceSecret gets the Secret key names into obj, a *corev1.Secret or,
// when only the marker matters, a *metav1.PartialObjectMetadata (so the
// pre-flight check never transfers the credentials), and returns a
// *SourceSecretNotAllowedError unless the agent may read it. The relay token
// Secret is refused before the Get. A Secret that does not exist is refused
// too: the Model names credentials it cannot have, and an hf:// download
// would otherwise go out unauthenticated. So is one the agent's RBAC does not
// let it read (Forbidden, typically a Secret outside a pinned resourceNames
// list): that does not clear on its own either, and the refusal tells the
// operator where to look. Any other Get error (a timeout, a 5xx, throttling)
// is returned as is, so the start is retried rather than refused for a cause
// that may clear on its own. unlabeled reports a read allowed only by
// allowUnlabeled.
func readSourceSecret(
	ctx context.Context, c client.Client, key types.NamespacedName, obj client.Object, allowUnlabeled bool,
) (unlabeled bool, err error) {
	if err := checkSourceSecretName(key); err != nil {
		return false, err
	}
	if err := c.Get(ctx, key, obj); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return false, &SourceSecretNotAllowedError{Namespace: key.Namespace, Name: key.Name,
				Why: fmt.Sprintf("the Secret does not exist; create it with the model-source keys and mark it: %s",
					labelCommand(key))}
		case apierrors.IsForbidden(err):
			return false, &SourceSecretNotAllowedError{Namespace: key.Namespace, Name: key.Name,
				Why: "the agent's RBAC does not allow reading it; if the agent's secrets get rule is pinned to " +
					"named Secrets (metalAgent.rbac.secretNames, or resourceNames in metal-agent-rbac.yaml), add it there"}
		}
		return false, fmt.Errorf("read sourceSecretRef %q in namespace %q: %w", key.Name, key.Namespace, err)
	}
	return checkSourceSecretLabels(key, obj.GetLabels(), allowUnlabeled)
}

// getSourceSecret is the only way the executor reads a sourceSecretRef: it
// returns the Secret key names when readSourceSecret allows it. The agent's
// pre-flight check (checkSourceSecret) refuses an unmarked Secret before any
// download or size probe; this is the same check again at the point of use,
// so no path reads an unmarked Secret's keys even if the pre-flight is
// bypassed or the Secret changes in between.
func (e *MetalExecutor) getSourceSecret(ctx context.Context, key types.NamespacedName) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if _, err := readSourceSecret(ctx, e.k8sClient, key, secret, e.allowUnlabeledSourceSecrets); err != nil {
		return nil, err
	}
	return secret, nil
}

// WithAllowUnlabeledSourceSecrets lets the executor read a sourceSecretRef
// Secret without the model-source-credentials marker (the deprecated
// --allow-unlabeled-source-secrets flag). The relay token Secret stays
// refused.
func WithAllowUnlabeledSourceSecrets(allow bool) Option {
	return func(e *MetalExecutor) {
		e.allowUnlabeledSourceSecrets = allow
	}
}

// checkSourceSecret refuses, before memory admission, a start whose Model's
// sourceSecretRef names a Secret the agent will not read, with
// SourceSecretNotAllowed. It runs for every runtime and whether or not the
// model is already cached, so whether a start is refused does not depend on
// the state of the model store: a cache eviction must not turn a running
// service into a refused one at its next restart. Only the Secret's metadata
// is read. It runs before admission because admission's s3 size probe reads
// the Secret and admission's success path clears SchedulingStatus, which
// would flap a refusal placed after it. A transient read error is returned
// without a refusal, so the next reconcile retries it.
//
// With --allow-unlabeled-source-secrets an unmarked Secret is allowed, and
// every start that relies on it is logged at Warn and gets a
// SourceSecretUnlabeled Warning Event, so the operator can find and mark the
// Secrets before the flag is removed.
func (a *MetalAgent) checkSourceSecret(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model,
) error {
	key := sourceSecretKeyFor(model.Namespace, model.Spec.SourceSecretRef)
	if key == nil {
		return nil
	}
	meta := &metav1.PartialObjectMetadata{}
	meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	unlabeled, err := readSourceSecret(ctx, a.config.K8sClient, *key, meta, a.config.AllowUnlabeledSourceSecrets)
	var notAllowed *SourceSecretNotAllowedError
	switch {
	case errors.As(err, &notAllowed):
		return a.refuseStart(ctx, isvc, EventReasonSourceSecretNotAllowed, notAllowed.Error())
	case err != nil:
		return err
	case unlabeled:
		msg := fmt.Sprintf("spec.sourceSecretRef names Secret %q in namespace %q, which is not marked for "+
			"model-source credentials; it is read only because %s is set, which is deprecated and will be removed. "+
			"Mark it: %s", key.Name, key.Namespace, allowUnlabeledSourceSecretsFlag, labelCommand(*key))
		a.logger.Warnw("reading an unmarked sourceSecretRef Secret",
			"namespace", isvc.Namespace, "name", isvc.Name, "secret", key.Name, "flag", allowUnlabeledSourceSecretsFlag)
		a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
			corev1.EventTypeWarning, EventReasonSourceSecretUnlabeled, "%s", msg)
	}
	return nil
}
