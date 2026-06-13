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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentReleasePhase is the rollout lifecycle state of an AgentRelease.
// +kubebuilder:validation:Enum=Pending;AwaitingApproval;InProgress;Paused;Succeeded;Failed
type AgentReleasePhase string

const (
	// AgentReleasePhasePending means the release has been created but no
	// rollout decision has been made yet (e.g. waiting for the controller
	// to compute the target node set).
	AgentReleasePhasePending AgentReleasePhase = "Pending"

	// AgentReleasePhaseAwaitingApproval means the release is ready to roll
	// out but is held until a human sets spec.approved=true.
	AgentReleasePhaseAwaitingApproval AgentReleasePhase = "AwaitingApproval"

	// AgentReleasePhaseInProgress means the rollout is actively updating nodes.
	AgentReleasePhaseInProgress AgentReleasePhase = "InProgress"

	// AgentReleasePhasePaused means the rollout was paused via spec.paused=true
	// after at least one node was already updated.
	AgentReleasePhasePaused AgentReleasePhase = "Paused"

	// AgentReleasePhaseSucceeded means all target nodes have been updated and
	// passed the health gate.
	AgentReleasePhaseSucceeded AgentReleasePhase = "Succeeded"

	// AgentReleasePhaseFailed means the rollout encountered an unrecoverable
	// error; see status.conditions for details.
	AgentReleasePhaseFailed AgentReleasePhase = "Failed"
)

// AgentReleaseArtifact describes a single platform-specific binary artifact
// for an agent release. The operator uses OS and Arch to select the correct
// artifact for each target FleetNode.
type AgentReleaseArtifact struct {
	// OS is the target operating system for this artifact.
	// +kubebuilder:validation:Enum=darwin;linux
	// +kubebuilder:validation:Required
	OS string `json:"os"`

	// Arch is the target CPU architecture for this artifact.
	// +kubebuilder:validation:Enum=amd64;arm64
	// +kubebuilder:validation:Required
	Arch string `json:"arch"`

	// URL is the download location for this artifact. The operator will
	// fetch it on behalf of target nodes unless a pre-placement mechanism
	// is in use.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// SHA256 is the lowercase hex-encoded SHA-256 digest of the artifact
	// binary. This is the trust anchor: the operator verifies the download
	// against this digest before dispatching the update to any node.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	SHA256 string `json:"sha256"`
}

// AgentReleaseHealthGate configures the post-update health check window.
// After an agent is updated, the controller waits up to TimeoutSeconds for
// the node's FleetNode heartbeat to stabilise for at least MinHealthySeconds
// before marking that node as Updated.
type AgentReleaseHealthGate struct {
	// MinHealthySeconds is how long a freshly-updated node must report a
	// fresh heartbeat continuously before the controller considers it healthy.
	// Defaults to 60 seconds.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinHealthySeconds int32 `json:"minHealthySeconds,omitempty"`

	// TimeoutSeconds is the maximum time the controller waits for a node to
	// pass the health gate before marking the node Failed. Defaults to 600
	// seconds (10 minutes).
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// AgentReleaseNodeStatus is the per-node rollout state within a release.
type AgentReleaseNodeStatus struct {
	// Name is the FleetNode name (metadata.name of the FleetNode CR).
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// ObservedVersion is the agent version last reported by this node's
	// FleetNode heartbeat. Empty until the node starts reporting version
	// information.
	// +optional
	ObservedVersion string `json:"observedVersion,omitempty"`

	// Phase is this node's position in the update lifecycle.
	// +kubebuilder:validation:Enum=Pending;Updating;Updated;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastTransitionTime is when Phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// Message is a human-readable reason for the current Phase (particularly
	// useful when Phase is Failed).
	// +optional
	Message string `json:"message,omitempty"`
}

// AgentReleaseSpec describes the desired state of an agent rollout.
type AgentReleaseSpec struct {
	// AgentKind identifies which agent binary this release targets. The
	// controller uses this to filter FleetNodes by their reported AgentKind.
	// +kubebuilder:validation:Required
	AgentKind FleetNodeAgentKind `json:"agentKind"`

	// Version is the target agent version string (e.g. "v0.9.0"). The
	// controller matches this against FleetNode.status.agentVersion to
	// determine which nodes require an update.
	// +kubebuilder:validation:Required
	Version string `json:"version"`

	// Artifacts is the list of platform-specific binaries for this release.
	// The controller selects the appropriate artifact for each FleetNode
	// based on its reported OS and architecture.
	// +optional
	Artifacts []AgentReleaseArtifact `json:"artifacts,omitempty"`

	// Selector restricts the rollout to FleetNodes whose labels match. When
	// nil, all FleetNodes of the given AgentKind are targeted.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// Concurrency is the maximum number of nodes that may be updating
	// simultaneously. Defaults to 1 (one node at a time).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Concurrency int32 `json:"concurrency,omitempty"`

	// Approved is the human-approval gate. When false (the default) the
	// controller waits in AwaitingApproval before beginning any rollout.
	// Set to true to allow the rollout to proceed.
	// +kubebuilder:default=false
	// +optional
	Approved bool `json:"approved,omitempty"`

	// Paused suspends all further rollout activity when true. Nodes that are
	// already Updating continue to completion; no new nodes are picked up.
	// +kubebuilder:default=false
	// +optional
	Paused bool `json:"paused,omitempty"`

	// HealthGate configures the post-update health check parameters applied
	// after each node is updated.
	// +optional
	HealthGate AgentReleaseHealthGate `json:"healthGate,omitempty"`
}

// AgentReleaseStatus is the controller's observed view of the rollout.
// All fields are set by the controller; do not edit manually.
type AgentReleaseStatus struct {
	// Phase is the overall rollout lifecycle state.
	// +optional
	Phase AgentReleasePhase `json:"phase,omitempty"`

	// ObservedGeneration is the metadata.generation of the AgentRelease spec
	// that was most recently reconciled. Used to detect pending spec changes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DesiredVersion mirrors spec.version at the time of last reconciliation,
	// allowing status readers to identify version drift without reading spec.
	// +optional
	DesiredVersion string `json:"desiredVersion,omitempty"`

	// TargetNodes is the total number of FleetNodes that match this release's
	// selector and AgentKind.
	// +optional
	TargetNodes int32 `json:"targetNodes,omitempty"`

	// UpdatedNodes is the count of nodes that have been successfully updated
	// and passed the health gate.
	// +optional
	UpdatedNodes int32 `json:"updatedNodes,omitempty"`

	// InFlightNodes lists the names of FleetNodes that are currently being
	// updated (Phase=Updating). The length of this list is bounded by
	// spec.concurrency.
	// +optional
	InFlightNodes []string `json:"inFlightNodes,omitempty"`

	// NodeStatuses is the per-node breakdown of rollout progress.
	// +optional
	NodeStatuses []AgentReleaseNodeStatus `json:"nodeStatuses,omitempty"`

	// Conditions is the set of standard Kubernetes conditions for this
	// release. The controller uses the types Approved, Progressing, and
	// Complete.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=agr
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=`.spec.approved`
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedNodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRelease describes a fleet-wide rollout of a new agent binary version.
// It is cluster-scoped because agent releases span the entire fleet; there is
// no meaningful namespace boundary for a node-level binary update. The
// controller (added in a subsequent PR) will use this resource to drive a
// rolling, approval-gated, health-checked upgrade across all matching
// FleetNodes.
type AgentRelease struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec describes the desired rollout configuration.
	Spec AgentReleaseSpec `json:"spec"`

	// status is the controller's observed rollout state. Do not edit manually.
	// +optional
	Status AgentReleaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentReleaseList is a list of AgentRelease resources.
type AgentReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRelease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRelease{}, &AgentReleaseList{})
}
