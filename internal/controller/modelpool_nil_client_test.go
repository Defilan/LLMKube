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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// A ModelPoolReconciler built without an HTTPClient must not panic.
//
// This is the shape the operator actually runs in: cmd/main.go constructs the
// reconciler with Client, Scheme and Recorder and never sets HTTPClient, so it
// is nil in production. memberIdle passed that nil straight to IdleProbe, and
// net/http.(*Client).do dereferences it, so the first idle check of the first
// swap panicked and took the reconcile with it. The pool then wedged in
// Swapping and every request against it burned the full swapBudget before
// returning 503.
//
// The existing suite could not catch this because every other ModelPool test
// injects the IdleCheck hook, which returns before HTTPClient is ever read.
// This test deliberately does NOT set that hook, so it exercises the real
// probe path.
//
// The assertion is only "does not panic, and reports an error". Reaching a
// cluster DNS name from a unit test must fail; what matters is that it fails
// as a returned error the caller can act on, which is what lets the documented
// fail-closed behaviour actually happen.
func TestMemberIdle_NilHTTPClientDoesNotPanic(t *testing.T) {
	r := &ModelPoolReconciler{} // no HTTPClient, no IdleCheck: production shape

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-member", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: "llamacpp",
		},
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("memberIdle panicked with a nil HTTPClient: %v", p)
		}
	}()

	idle, err := r.memberIdle(context.Background(), isvc)
	if err == nil {
		t.Fatal("expected an error probing an unreachable member, got nil")
	}
	if idle {
		t.Error("an unreachable member must never be reported idle; that would " +
			"unload a possibly-busy model")
	}
	// The failure should be a transport error, not a nil-pointer recovery.
	if strings.Contains(strings.ToLower(err.Error()), "nil pointer") {
		t.Errorf("error looks like a recovered panic rather than a probe failure: %v", err)
	}
}
