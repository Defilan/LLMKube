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
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sglangToolParserFixture builds the reporter's object: an InferenceService on
// a GPU Model with runtime sglang and the given toolCallParser, so a reconcile
// renders --tool-call-parser exactly as a cluster would.
func sglangToolParserFixture(t *testing.T, c client.Client, name, parser string) *inferencev1alpha1.InferenceService {
	t.Helper()
	ctx := context.Background()
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: testNS},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://Qwen/Qwen3-8B",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cuda",
				GPU: &inferencev1alpha1.GPUSpec{
					Enabled: true,
					Count:   1,
					Vendor:  "nvidia",
				},
			},
		},
	}
	if err := c.Create(ctx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	model.Status.Phase = PhaseReady
	if err := c.Status().Update(ctx, model); err != nil {
		t.Fatalf("mark model ready: %v", err)
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: model.Name,
			Runtime:  RuntimeSGLANG,
			Replicas: ptrInt32(1),
			SGLangConfig: &inferencev1alpha1.SGLangConfig{
				ToolCallParser: parser,
			},
			// No node in envtest carries nvidia.com/gpu or the device taint.
			Tolerations:  []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists}},
			NodeSelector: map[string]string{"llmkube.dev/test-node-pool": "any"},
		},
	}
	if err := c.Create(ctx, isvc); err != nil {
		t.Fatalf("create inferenceservice: %v", err)
	}
	return isvc
}

// TestSGLangToolCallParserConditionThroughReconcile reproduces #2219: the enum
// admits qwen3 and functionary, SGLang exits at startup on both, and the
// stored object said nothing. Read back from the API server, not the
// in-memory copy, because the reporter read the object with kubectl.
func TestSGLangToolCallParserConditionThroughReconcile(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	newTestNamespace(t, c)

	cases := []struct {
		parser     string
		wantReason string
	}{
		{parser: "qwen3", wantReason: "ToolCallParserUnsupported"},
		{parser: "functionary", wantReason: "ToolCallParserUnsupported"},
		{parser: "qwen25", wantReason: ""},
	}

	for _, tc := range cases {
		t.Run(tc.parser, func(t *testing.T) {
			name := "sglang-tool-" + tc.parser
			isvc := sglangToolParserFixture(t, c, name, tc.parser)

			r := &InferenceServiceReconciler{
				Client:             c,
				Scheme:             c.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			if _, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: testNS},
			}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			fresh := &inferencev1alpha1.InferenceService{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(isvc), fresh); err != nil {
				t.Fatalf("read back: %v", err)
			}

			// The flag still renders whatever the value, so the condition is
			// the only signal and the workload is never blocked.
			dep := &appsv1.Deployment{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(isvc), dep); err != nil {
				t.Fatalf("Deployment not created: %v", err)
			}
			var args []string
			for _, ct := range dep.Spec.Template.Spec.Containers {
				if ct.Name == "sglang" {
					args = ct.Args
				}
			}
			if !slices.Contains(args, "--tool-call-parser") || !slices.Contains(args, tc.parser) {
				t.Fatalf("rendered sglang args %v do not carry --tool-call-parser %s", args, tc.parser)
			}

			cond := apimeta.FindStatusCondition(fresh.Status.Conditions, ConditionSGLangSpecValid)
			if tc.wantReason == "" {
				if cond != nil && cond.Status == metav1.ConditionFalse {
					t.Fatalf("SGLangSpecValid=False for registered parser %s: reason=%q message=%q",
						tc.parser, cond.Reason, cond.Message)
				}
				return
			}
			if cond == nil {
				t.Fatalf("no SGLangSpecValid condition on the stored InferenceService (got conditions %+v)", fresh.Status.Conditions)
			}
			if cond.Status != metav1.ConditionFalse || cond.Reason != tc.wantReason {
				t.Fatalf("SGLangSpecValid: got status=%q reason=%q message=%q, want status=False reason=%s",
					cond.Status, cond.Reason, cond.Message, tc.wantReason)
			}
		})
	}
}
