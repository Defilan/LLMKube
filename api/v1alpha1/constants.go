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

package v1alpha1

import "time"

const (
	// AnnotationAgentHeartbeat is stamped (RFC3339) on agent-managed Endpoints
	// every heartbeat interval. The InferenceService controller treats
	// registrations whose heartbeat is older than DefaultAgentHeartbeatTimeout
	// as not ready (issue #663). Endpoints without the annotation (older
	// agents) are exempt from expiry for backward compatibility.
	AnnotationAgentHeartbeat = "llmkube.ai/agent-heartbeat"

	// AnnotationAgentVersion is stamped on agent-managed Endpoints with the
	// running version of the metal-agent binary (e.g. "v0.8.4"). Set on every
	// RegisterEndpoint call so the cluster can observe which version is
	// managing a given InferenceService. Absent on Endpoints created by older
	// agents that predate this annotation.
	AnnotationAgentVersion = "llmkube.ai/agent-version"

	// AnnotationIdleEndpoint lets operators declare a custom HTTP path that
	// returns 2xx when a replica is idle. Used by the generic runtime to opt
	// in to drain-before-roll. Set on InferenceService metadata.annotations.
	AnnotationIdleEndpoint = "inference.llmkube.dev/idle-endpoint"

	// DefaultAgentHeartbeatInterval is how often the metal-agent re-asserts
	// its registrations (which also self-heals any missed update, #657).
	DefaultAgentHeartbeatInterval = 30 * time.Second

	// DefaultAgentHeartbeatTimeout is how stale a heartbeat may be before the
	// controller stops counting the registration as ready (6 intervals).
	DefaultAgentHeartbeatTimeout = 3 * time.Minute
)

// InferenceService and Model lifecycle phase strings written to
// status.phase by both the operator (internal/controller) and the
// metal-agent (pkg/agent). Hoisted here so a rename on either side is a
// compile error rather than a silent status-thrash bug: the two writers
// previously disagreed on the phase string for a suspended service (#1254)
// because pkg/agent could not import internal/controller and carried its
// own copy. The string values are CRD- and client-visible and must not
// change without a corresponding API bump.
const (
	// PhaseReady means the workload is serving (InferenceService) or the
	// model is cached and available (Model).
	PhaseReady = "Ready"
	// PhaseFailed means the workload or model could not be brought up.
	PhaseFailed = "Failed"
	// PhaseCached means the model file is present in the cache but not
	// yet referenced by a ready InferenceService.
	PhaseCached = "Cached"
	// PhaseDownloading means the model is being fetched or copied into
	// the cache.
	PhaseDownloading = "Downloading"
	// PhaseCreating means the InferenceService is being provisioned
	// (deployment/service creation, or waiting for the metal-agent).
	PhaseCreating = "Creating"
	// PhaseStopped means the InferenceService has been scaled to zero
	// (spec.replicas=0) and the workload torn down.
	PhaseStopped = "Stopped"
	// PhaseSuspended means the InferenceService has spec.suspend=true and
	// the workload torn down while preserving spec.replicas for resume.
	PhaseSuspended = "Suspended"
	// PhaseWaitingForGPU means the InferenceService is queued waiting for
	// GPU resources to become available.
	PhaseWaitingForGPU = "WaitingForGPU"
)

const (
	// ConditionRolloutDeferred indicates whether a rollout is being deferred
	// because the InferenceService has waitForIdle enabled and pods are not yet
	// idle. When True, the Deployment pod-template update is held until all
	// backend slots report idle or the idleTimeoutSeconds expires.
	ConditionRolloutDeferred string = "RolloutDeferred"

	// ReasonPodsBusy is set when RolloutDeferred=True because one or more
	// backend slots are currently processing requests.
	ReasonPodsBusy string = "PodsBusy"

	// ReasonIdleCheckFailed is set when RolloutDeferred=True because the
	// controller could not determine idleness (e.g. /slots unreachable,
	// non-200, or the backend was started with --no-slots). The rollout is
	// still deferred (fail-closed) until the idleTimeoutSeconds budget is
	// spent.
	ReasonIdleCheckFailed string = "IdleCheckFailed"

	// ReasonIdleTimeoutExceeded is set when RolloutDeferred=False after the
	// idle timeout expired and the rollout proceeded despite busy pods.
	ReasonIdleTimeoutExceeded string = "IdleTimeoutExceeded"

	// ReasonIdleCheckUnsupported is set when the runtime backend does not
	// implement IdleDetector, so drain-before-roll cannot probe idleness.
	ReasonIdleCheckUnsupported string = "IdleCheckUnsupported"

	// ReasonPodsCrashLooping is set when RolloutDeferred=True because some
	// old-generation pods are crashlooping (not Ready) while others are Ready
	// and serving. The rollout is deferred to protect in-flight work on the
	// Ready pods; when ALL old pods are unready the rollout proceeds instead
	// (no work to protect).
	ReasonPodsCrashLooping string = "PodsCrashLooping"

	// DefaultIdleCheckInterval is how often the controller re-checks pod
	// idleness when waiting for idle before rollout.
	DefaultIdleCheckInterval = 5 * time.Second
)
