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
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ModelRouterReasonBackendAPIKeyUnsupported marks a router with a backend
// InferenceService that requires an API key (spec.endpoint.apiKeySecretRef).
// No data plane forwards that key yet, so every request to it would get 401.
//
//nolint:gosec // G101: condition reason string, not a credential value
const ModelRouterReasonBackendAPIKeyUnsupported = "BackendAPIKeyUnsupported"

// keyedBackends returns the sorted names of InferenceServiceRef backends whose
// InferenceService sets spec.endpoint.apiKeySecretRef. A backend whose
// InferenceService does not exist is not keyed; the resolver reports it.
func keyedBackends(ctx context.Context, c client.Reader, mr *inferencev1alpha1.ModelRouter) ([]string, error) {
	var names []string
	for i := range mr.Spec.Backends {
		b := &mr.Spec.Backends[i]
		if b.InferenceServiceRef == nil {
			continue
		}
		isvc := &inferencev1alpha1.InferenceService{}
		err := c.Get(ctx, types.NamespacedName{Namespace: mr.Namespace, Name: b.InferenceServiceRef.Name}, isvc)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get InferenceService %s/%s: %w", mr.Namespace, b.InferenceServiceRef.Name, err)
		}
		if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.APIKeySecretRef != nil {
			names = append(names, b.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func keyedBackendsMessage(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return fmt.Sprintf("backend(s) %s point at an InferenceService with spec.endpoint.apiKeySecretRef; "+
		"ModelRouter does not forward that key yet, so requests would get 401. Remove the key "+
		"from the InferenceService or route to it directly", strings.Join(quoted, ", "))
}
