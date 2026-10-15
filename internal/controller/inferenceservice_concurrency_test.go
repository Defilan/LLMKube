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

import "testing"

// TestInferenceServiceReconcileConcurrencyExceedsOne pins the #2069 guard:
// at the controller-runtime default of 1 worker, one stuck InferenceService
// reconcile head-of-line blocks every other InferenceService in the cluster
// while the pod stays healthy. Dropping the constant back to 1 must fail
// here and force the change to be reconsidered. envtest cannot observe the
// worker count of a running controller, so the constant itself is the
// asserted surface.
func TestInferenceServiceReconcileConcurrencyExceedsOne(t *testing.T) {
	if InferenceServiceReconcileConcurrency < 2 {
		t.Fatalf("InferenceServiceReconcileConcurrency = %d; a single worker lets one stuck reconcile freeze every InferenceService (#2069)", InferenceServiceReconcileConcurrency)
	}
}
