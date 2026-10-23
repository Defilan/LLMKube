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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs drive the parsers through the envtest apiserver and the
// reconciler, so they pin both the generated CRD schema and the argv the
// container receives. They build the InferenceService as unstructured data on
// purpose: the repo runs the same file against the branch and against base,
// where the typed fields do not exist.
var _ = Describe("typed tool and reasoning parsers", func() {
	ctx := context.Background()

	// newParserModel creates the Ready Model the reconcile needs and returns
	// its name.
	newParserModel := func(name string) string {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		return model.Name
	}

	// newParserISvc creates the InferenceService from raw spec data and
	// returns the create error so a case can assert admission.
	newParserISvc := func(name, modelRef, runtime string, config map[string]interface{}, configKey string) error {
		spec := map[string]interface{}{
			"modelRef": modelRef,
			"runtime":  runtime,
			"image":    "ghcr.io/ggml-org/llama.cpp:server",
		}
		if config != nil {
			spec[configKey] = config
		}
		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "inference.llmkube.dev/v1alpha1",
			"kind":       "InferenceService",
			"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
			"spec":       spec,
		}}
		return k8sClient.Create(ctx, obj)
	}

	// containerArgs reconciles once and reads the served container's argv out
	// of the generated Deployment, which is the object that reaches the node.
	containerArgs := func(name string, containerName string) []string {
		r := &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, dep)).To(Succeed())
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == containerName {
				return c.Args
			}
		}
		Fail("deployment has no container named " + containerName)
		return nil
	}

	cleanup := func(name, modelRef string) {
		_ = k8sClient.Delete(ctx, &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		_ = k8sClient.Delete(ctx, &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: modelRef, Namespace: "default"}})
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(name), Namespace: "default"}})
	}

	It("carries vllmConfig parsers to the vllm container as the auto-tool-choice pair", func() {
		const name = "parsers-vllm"
		modelRef := newParserModel("parsers-vllm-model")
		defer cleanup(name, modelRef)

		Expect(newParserISvc(name, modelRef, "vllm", map[string]interface{}{
			"toolCallParser":  "glm47",
			"reasoningParser": "glm45",
		}, "vllmConfig")).To(Succeed())

		args := containerArgs(name, "vllm")
		Expect(containsArg(args, "--tool-call-parser", "glm47")).To(BeTrue(), "args: %v", args)
		Expect(containsArg(args, "--reasoning-parser", "glm45")).To(BeTrue(), "args: %v", args)
		Expect(containsArg(args, "--enable-auto-tool-choice", "")).To(BeTrue(), "args: %v", args)
		Expect(countArg(args, "--enable-auto-tool-choice")).To(Equal(1), "args: %v", args)
	})

	It("admits the GLM parsers on sglangConfig and carries them to the sglang container", func() {
		const name = "parsers-sglang"
		modelRef := newParserModel("parsers-sglang-model")
		defer cleanup(name, modelRef)

		Expect(newParserISvc(name, modelRef, "sglang", map[string]interface{}{
			"toolCallParser":  "glm47",
			"reasoningParser": "glm45",
		}, "sglangConfig")).To(Succeed())

		args := containerArgs(name, "sglang")
		Expect(containsArg(args, "--tool-call-parser", "glm47")).To(BeTrue(), "args: %v", args)
		Expect(containsArg(args, "--reasoning-parser", "glm45")).To(BeTrue(), "args: %v", args)
	})

	// Parity: SGLang registers no glm47 reasoning detector at the pinned image,
	// so the enum must keep rejecting it.
	It("still rejects glm47 as an sglang reasoning parser", func() {
		const name = "parsers-sglang-glm47-reasoning"
		modelRef := newParserModel("parsers-sglang-glm47-model")
		defer cleanup(name, modelRef)

		err := newParserISvc(name, modelRef, "sglang", map[string]interface{}{
			"reasoningParser": "glm47",
		}, "sglangConfig")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("reasoningParser"))
	})
})
