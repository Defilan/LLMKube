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

package agent

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Event reasons for starts the agent refuses before (or instead of) serving.
const (
	EventReasonEndpointNameConflict  = "EndpointNameConflict"
	EventReasonModelSourceNotAllowed = "ModelSourceNotAllowed"
	EventReasonExtraArgsRejected     = "ExtraArgsRejected"
)

// refuseStart records why the agent will not serve an InferenceService: it
// writes the agent-owned scheduling status fields (the same surface memory
// admission uses, cleared by the next admitted start) and emits a Warning
// Event, then returns an error so ensureProcess withdraws any endpoint. It
// must not be called while holding a.mu (emitInferenceEvent does I/O).
func (a *MetalAgent) refuseStart(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	reason, message string,
) error {
	a.logger.Warnw("refusing to serve inference service",
		"namespace", isvc.Namespace, "name", isvc.Name, "reason", reason, "message", message)
	isvc.Status.SchedulingStatus = reason
	isvc.Status.SchedulingMessage = message
	if err := a.config.K8sClient.Status().Update(ctx, isvc); err != nil {
		a.logger.Warnw("failed to update InferenceService status", "error", err)
	}
	a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
		corev1.EventTypeWarning, reason, "%s", message)
	return fmt.Errorf("%s: %s", reason, message)
}

// checkModelPaths refuses a Model the controller marked Failed and any local
// model source or pagedSSDCacheDir that resolves outside the allowed roots.
// Remote sources are downloaded into the model store, which is always inside
// a root, so they are not checked here.
func (a *MetalAgent) checkModelPaths(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	pagedSSDCacheDir string,
) error {
	if model.Status.Phase == inferencev1alpha1.PhaseFailed {
		return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed,
			fmt.Sprintf("Model %s is Failed (%s); the agent does not serve a Model the controller rejected",
				model.Name, modelFailureReason(model)))
	}
	// A ".." path segment is refused regardless of scheme. mlx-server,
	// vllm-swift and tensorfold's resolveModelPath all filepath.Join a
	// non-absolute ModelSource onto the model store, including one WITH a
	// scheme (only an absolute local path is left as-is), so e.g.
	// "https://../../../../Users/victim/x" or "hf://../../../x" walks the
	// joined path back out of the store. The CRD's Source pattern allows any
	// character after a scheme, and the controller marks a remote-looking
	// source Ready, so neither the pattern nor the Failed check above catches
	// this; check the literal segments here, before the scheme-aware branches
	// below (which still handle a symlink escape with no ".." at all, but only
	// for local and scheme-less sources; a remote-scheme source is not
	// path-checked at all beyond this ".." scan).
	for _, seg := range strings.Split(model.Spec.Source, "/") {
		if seg == ".." {
			return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed,
				fmt.Sprintf("model source %q contains a %q path segment; the agent refuses it because "+
					"some runtimes join sources onto the model store", model.Spec.Source, ".."))
		}
	}
	workDir := a.config.ModelStorePath
	if isLocalModelSource(model.Spec.Source) {
		if err := a.roots.CheckPath("model path", localSourcePath(model.Spec.Source), workDir, a.home); err != nil {
			return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed, err.Error())
		}
	} else if !strings.Contains(model.Spec.Source, "://") {
		// A scheme-less source with no leading "/" is neither a URL
		// (http(s)/hf/s3/pvc/oci/file) nor an absolute local path: it is a
		// relative value such as an owner/repo id, which every executor's
		// resolveModelPath joins onto the model store. filepath.Join cleans
		// ".." out of the joined path, so an unchecked value like
		// "a/../../../../etc/passwd" would resolve outside the store. Run it
		// through the same root check as a local source; ResolvePath joins it
		// onto workDir, refuses any ".." component, and resolves symlinks, so
		// a plain repo id that does not exist yet still resolves under the
		// store and passes.
		if err := a.roots.CheckPath("model path", model.Spec.Source, workDir, a.home); err != nil {
			return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed, err.Error())
		}
	}
	if pagedSSDCacheDir != "" {
		if err := a.roots.CheckPath("pagedSSDCacheDir", pagedSSDCacheDir, workDir, a.home); err != nil {
			return a.refuseStart(ctx, isvc, EventReasonModelSourceNotAllowed, err.Error())
		}
	}
	return nil
}

// modelFailureReason returns the reason of the Model's Degraded condition, or
// "no reason recorded". The literal "Degraded" matches the controller's
// ConditionDegraded (internal/controller/model_controller.go), which
// pkg/agent cannot import.
func modelFailureReason(model *inferencev1alpha1.Model) string {
	for _, c := range model.Status.Conditions {
		if c.Type == "Degraded" && c.Status == metav1.ConditionTrue && c.Reason != "" {
			return c.Reason
		}
	}
	return "no reason recorded"
}
