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
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ReasonRolloutStrategyIgnored is the Warning event reason for an
// InferenceService whose spec.rolloutPolicy.strategy cannot apply to it: the
// workload already rolls with the Kubernetes default (non-GPU), must not
// overlap pods (DRA claims), or runs no Deployment at all (metal, multiNode).
const ReasonRolloutStrategyIgnored = "RolloutStrategyIgnored"

const rolloutStrategyIgnoredMessage = "spec.rolloutPolicy.strategy is ignored: a non-GPU service already rolls with the Kubernetes default"

const rolloutStrategyIgnoredDRAMessage = "spec.rolloutPolicy.strategy is ignored: a DRA service keeps Recreate, because a replacement pod cannot get the resource claim while the old pod holds it"

const rolloutStrategyIgnoredMetalMessage = "spec.rolloutPolicy.strategy is ignored: a metal service runs no Deployment and no pod-replacement strategy"

const rolloutStrategyIgnoredMultiNodeMessage = "spec.rolloutPolicy.strategy is ignored: a multiNode group runs no Deployment"

// rolloutStrategyIgnoredCause returns the message naming why this service's
// strategy cannot apply. It mirrors resolveRolloutStrategy's order exactly, so a
// warning cannot name a cause the resolver did not take, or miss one it did.
func rolloutStrategyIgnoredCause(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) string {
	switch {
	case isvc != nil && isvc.Spec.MultiNode != nil:
		return rolloutStrategyIgnoredMultiNodeMessage
	case len(modelResourceClaims(model)) > 0:
		return rolloutStrategyIgnoredDRAMessage
	case isMetalModel(model):
		return rolloutStrategyIgnoredMetalMessage
	default:
		return rolloutStrategyIgnoredMessage
	}
}

// requestedRolloutStrategy returns the strategy the user asked for, or "" when
// the field is unset. An unknown value is returned as-is rather than coerced,
// so the caller reports it as ignored instead of silently honouring it.
func requestedRolloutStrategy(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil || isvc.Spec.RolloutPolicy == nil {
		return ""
	}
	return isvc.Spec.RolloutPolicy.Strategy
}

// resolveRolloutStrategy is the single authority for the Deployment
// pod-replacement strategy. Every path that writes one goes through here, so
// the GPU device-plugin answer and the DRA answer cannot drift apart: both
// must keep the old pod and its replacement from counting at the same time
// (#192), DRA because a replacement cannot get the claim while the old pod
// holds it. A zero Type means "set no strategy and leave the API default",
// which is what a non-GPU service already gets.
//
// The second return value is false when a requested strategy does not produce
// the strategy the user asked for, so the reconcile can say so rather than
// accepting a field it ignores (#1503).
func resolveRolloutStrategy(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
) (appsv1.DeploymentStrategy, bool) {
	requested := requestedRolloutStrategy(isvc)

	var strategy appsv1.DeploymentStrategy
	switch {
	case isvc != nil && isvc.Spec.MultiNode != nil:
		// A multiNode group runs one pod per node and no Deployment.
		strategy = appsv1.DeploymentStrategy{}
	case len(modelResourceClaims(model)) > 0:
		strategy = recreateStrategy()
	case isMetalModel(model):
		// The metal-agent runs the engine natively on the Mac; there is no
		// Deployment to carry a strategy.
		strategy = appsv1.DeploymentStrategy{}
	case resolveGPUCount(isvc, model) <= 0:
		strategy = appsv1.DeploymentStrategy{}
	case requested == inferencev1alpha1.RolloutStrategyOneAtATime:
		strategy = oneAtATimeStrategy()
	default:
		strategy = recreateStrategy()
	}

	return strategy, requested == "" || strategyReflects(strategy, requested)
}

func recreateStrategy() appsv1.DeploymentStrategy {
	return appsv1.DeploymentStrategy{
		Type: appsv1.RecreateDeploymentStrategyType,
	}
}

// oneAtATimeStrategy replaces one pod at a time. maxSurge 0 is what keeps the
// old pod and its replacement from ever counting together: NewRSNewReplicas
// caps the new ReplicaSet at deployment.Spec.Replicas while any old pod counts,
// so the replacement starts only after an old pod is scaled down, and no spare
// GPU is needed.
func oneAtATimeStrategy() appsv1.DeploymentStrategy {
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		},
	}
}

// strategyReflects reports whether strategy is the one the requested value
// names. Recreate and RollingUpdate both carry defaults for the other's
// fields, so comparing the whole struct would never match.
func strategyReflects(strategy appsv1.DeploymentStrategy, requested string) bool {
	switch requested {
	case inferencev1alpha1.RolloutStrategyRecreate:
		return strategy.Type == appsv1.RecreateDeploymentStrategyType
	case inferencev1alpha1.RolloutStrategyOneAtATime:
		return strategy.Type == appsv1.RollingUpdateDeploymentStrategyType &&
			strategy.RollingUpdate != nil &&
			strategy.RollingUpdate.MaxSurge != nil &&
			strategy.RollingUpdate.MaxSurge.IntValue() == 0 &&
			strategy.RollingUpdate.MaxUnavailable != nil &&
			strategy.RollingUpdate.MaxUnavailable.IntValue() == 1
	default:
		return false
	}
}

// ignoredRolloutStrategy reports whether a set strategy has no effect on this
// service, with the message to emit. Derived from resolveRolloutStrategy so the
// warning cannot claim a field is ignored while the builder honours it.
func ignoredRolloutStrategy(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
) (bool, string) {
	_, honoured := resolveRolloutStrategy(isvc, model)
	if honoured {
		return false, ""
	}
	return true, rolloutStrategyIgnoredCause(isvc, model)
}
