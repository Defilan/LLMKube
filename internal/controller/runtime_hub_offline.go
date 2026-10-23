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
	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ReasonHubOfflineRuntimeDownload is the Warning event reason for a service
// that asks for a Hub-offline serving container while its runtime downloads the
// Model itself. The runtime fails at start and only the container log would say
// why.
const ReasonHubOfflineRuntimeDownload = "HubOfflineRuntimeDownload"

// hubOfflineEnv is the serving container's env for spec.hubOffline. Both
// variables are set because huggingface_hub reads HF_HUB_OFFLINE with
// TRANSFORMERS_OFFLINE as its fallback, and transformers 4.41 and older read
// only the latter.
func hubOfflineEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar {
	if isvc == nil || !isvc.Spec.HubOffline {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "HF_HUB_OFFLINE", Value: "1"},
		{Name: "TRANSFORMERS_OFFLINE", Value: "1"},
	}
}

// hubOfflineRuntimeDownload reports whether spec.hubOffline leaves the runtime
// no way to start: the operator stages nothing, so the serving container fetches
// the Model itself, and the source is a Hub repo id it would fetch. The
// pre-staged and non-Hub sources have no Hub lookup to fail, so they are
// excluded by isHFRepoSource.
func hubOfflineRuntimeDownload(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, backend RuntimeBackend) bool {
	if isvc == nil || model == nil || !isvc.Spec.HubOffline {
		return false
	}
	if operatorStagesModel(isvc, backend) {
		return false
	}
	return isHFRepoSource(model.Spec.Source)
}

// hubOfflineRuntimeDownloadMessage names the failure and the ways out.
func hubOfflineRuntimeDownloadMessage(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) string {
	return "spec.hubOffline is set, but runtime " + resolveServingRuntime(isvc) + " downloads Model " + model.Name +
		" itself on the serving node, so its start fails. Let the operator stage the Model (drop spec.skipModelInit, or set spec.stageModel: true), " +
		"serve a pvc:// source, or unset spec.hubOffline"
}
