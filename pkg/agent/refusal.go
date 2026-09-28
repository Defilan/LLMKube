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

	corev1 "k8s.io/api/core/v1"

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
