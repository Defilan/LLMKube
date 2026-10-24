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
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs drive vllmConfig.speculative through the envtest apiserver and
// the reconciler, so they pin the CRD schema, the generated Container.Args and
// the volume set the serving pod actually gets. The InferenceService is built
// as unstructured data on purpose: the repo runs the same file against the
// branch and against base, where the typed speculative fields do not exist.
var _ = Describe("vLLM speculative decoding", func() {
	ctx := context.Background()

	// newSpecModel creates a Ready Model and returns it. The draft Model in
	// these specs is a multi-file (directory-oriented) model so the path
	// handed to vLLM is the staged directory, which is what a draft HF tree
	// needs, and it differs from the CR name in an obvious way.
	newSpecModel := func(name string, multiFile bool) *inferencev1alpha1.Model {
		spec := inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/model.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
		}
		if multiFile {
			spec.Format = "safetensors"
			spec.Source = "hf://example/draft"
			spec.Files = []string{"config.json", "model-00001-of-00001.safetensors"}
		}
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		return model
	}

	newSpecISvc := func(name, runtime, modelRef string, extra map[string]interface{}) error {
		spec := map[string]interface{}{
			"modelRef": modelRef,
			"runtime":  runtime,
			"image":    "vllm/vllm-openai:v0.25.1",
		}
		for k, v := range extra {
			spec[k] = v
		}
		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "inference.llmkube.dev/v1alpha1",
			"kind":       "InferenceService",
			"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
			"spec":       spec,
		}}
		return k8sClient.Create(ctx, obj)
	}

	// reconcileAndRead reconciles once and returns the Deployment the
	// reconciler wrote, which is the object that reaches the node.
	reconcileAndRead := func(name string) (*appsv1.Deployment, *inferencev1alpha1.InferenceService) {
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
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
		return dep, isvc
	}

	// servedArgs returns the vllm container's argv, which is the only
	// container these specs care about.
	servedArgs := func(dep *appsv1.Deployment) []string {
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == "vllm" {
				return c.Args
			}
		}
		Fail("deployment has no container named vllm")
		return nil
	}

	// speculativeConfigValue returns the JSON document the argv hands vLLM, so
	// a case can assert on the keys rather than on a byte-exact string.
	speculativeConfigValue := func(args []string) (map[string]interface{}, bool) {
		for i, a := range args {
			var raw string
			switch {
			case a == "--speculative-config" && i+1 < len(args):
				raw = args[i+1]
			case strings.HasPrefix(a, "--speculative-config="):
				raw = strings.TrimPrefix(a, "--speculative-config=")
			default:
				continue
			}
			out := map[string]interface{}{}
			Expect(json.Unmarshal([]byte(raw), &out)).To(Succeed(), "rendered JSON is not an object: %s", raw)
			return out, true
		}
		return nil, false
	}

	cleanup := func(name string, models ...*inferencev1alpha1.Model) {
		_ = k8sClient.Delete(ctx, &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		for _, m := range models {
			_ = k8sClient.Delete(ctx, m)
		}
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(name), Namespace: "default"}})
	}

	It("renders --speculative-config for a method that needs no draft weights", func() {
		const name = "vllm-spec-method"
		target := newSpecModel("vllm-spec-method-target", false)
		defer cleanup(name, target)

		Expect(newSpecISvc(name, "vllm", target.Name, map[string]interface{}{
			"vllmConfig": map[string]interface{}{
				"speculative": map[string]interface{}{
					"enabled":              true,
					"method":               "dspark",
					"numSpeculativeTokens": int64(5),
				},
			},
		})).To(Succeed())

		dep, _ := reconcileAndRead(name)
		args := servedArgs(dep)

		got, ok := speculativeConfigValue(args)
		Expect(ok).To(BeTrue(), "args carry no --speculative-config: %v", args)
		Expect(got).To(Equal(map[string]interface{}{
			"method":                 "dspark",
			"num_speculative_tokens": float64(5),
		}), "args: %v", args)
		Expect(args).NotTo(ContainElement("--speculative-model"), "args: %v", args)
		Expect(args).NotTo(ContainElement("--num-speculative-tokens"), "args: %v", args)
	})

	It("mounts the draft Model and renders its staged path", func() {
		const name = "vllm-spec-draft"
		target := newSpecModel("vllm-spec-draft-target", false)
		draft := newSpecModel("vllm-spec-draft-weights", true)
		defer cleanup(name, target, draft)

		Expect(newSpecISvc(name, "vllm", target.Name, map[string]interface{}{
			"vllmConfig": map[string]interface{}{
				"speculative": map[string]interface{}{
					"enabled":              true,
					"model":                draft.Name,
					"numSpeculativeTokens": int64(4),
				},
			},
		})).To(Succeed())

		dep, isvc := reconcileAndRead(name)
		Expect(isvc.Status.Phase).NotTo(Equal(PhaseFailed), "status: %+v", isvc.Status)
		args := servedArgs(dep)

		got, ok := speculativeConfigValue(args)
		Expect(ok).To(BeTrue(), "args carry no --speculative-config: %v", args)
		modelValue, _ := got["model"].(string)
		Expect(modelValue).NotTo(BeEmpty(), "args: %v", args)
		Expect(modelValue).NotTo(Equal(draft.Name),
			"the draft Model's name is not a path vLLM can load: %v", args)
		Expect(modelValue).To(HavePrefix("/models/"), "args: %v", args)
		Expect(got["method"]).To(Equal("draft_model"), "args: %v", args)
		Expect(got["num_speculative_tokens"]).To(Equal(float64(4)), "args: %v", args)
		Expect(args).NotTo(ContainElement("--speculative-model"), "args: %v", args)

		// The draft's weights have to be in the pod for that path to exist.
		names := make([]string, 0, len(dep.Spec.Template.Spec.InitContainers))
		for _, c := range dep.Spec.Template.Spec.InitContainers {
			names = append(names, c.Name)
		}
		Expect(countString(names, "model-downloader")).To(Equal(1),
			"expected the target's downloader, got %v", names)
		Expect(countString(names, "draft-model-downloader")).To(Equal(1),
			"the draft's weights have to be staged for the rendered path to resolve, got %v", names)
	})

	It("skips the flags and reports the cause when skipModelInit leaves the draft unstaged", func() {
		const name = "vllm-spec-skipinit"
		target := newSpecModel("vllm-spec-skipinit-target", false)
		draft := newSpecModel("vllm-spec-skipinit-weights", true)
		defer cleanup(name, target, draft)

		Expect(newSpecISvc(name, "vllm", target.Name, map[string]interface{}{
			"skipModelInit": true,
			"vllmConfig": map[string]interface{}{
				"speculative": map[string]interface{}{
					"enabled": true,
					"model":   draft.Name,
				},
			},
		})).To(Succeed())

		dep, isvc := reconcileAndRead(name)
		args := servedArgs(dep)
		_, emitted := speculativeConfigValue(args)
		Expect(emitted).To(BeFalse(),
			"nothing staged the draft, so vLLM would fail to load it: %v", args)

		cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionVLLMSpecValid)
		Expect(cond).NotTo(BeNil(), "expected a %s condition", ConditionVLLMSpecValid)
		Expect(cond.Reason).To(Equal("SpeculativeDraftNotStaged"))
		Expect(cond.Message).To(ContainSubstring("skipModelInit"))
	})

	It("rejects vllmConfig.speculative enabled with neither model nor method", func() {
		const name = "vllm-spec-neither"
		target := newSpecModel("vllm-spec-neither-target", false)
		defer cleanup(name, target)

		err := newSpecISvc(name, "vllm", target.Name, map[string]interface{}{
			"vllmConfig": map[string]interface{}{
				"speculative": map[string]interface{}{
					"enabled":              true,
					"numSpeculativeTokens": int64(4),
				},
			},
		})
		Expect(err).To(HaveOccurred(),
			"vLLM rejects this spec (num_speculative_tokens without a speculative model) at startup, so admission must refuse it")
		Expect(err.Error()).To(ContainSubstring("model"))
	})

	// Parity: an explicitly disabled block renders nothing on either tree.
	It("emits no speculative flag when the block is disabled", func() {
		const name = "vllm-spec-disabled"
		target := newSpecModel("vllm-spec-disabled-target", false)
		defer cleanup(name, target)

		Expect(newSpecISvc(name, "vllm", target.Name, map[string]interface{}{
			"vllmConfig": map[string]interface{}{
				"speculative": map[string]interface{}{
					"enabled": false,
					"model":   "some-draft",
				},
			},
		})).To(Succeed())

		dep, _ := reconcileAndRead(name)
		args := servedArgs(dep)
		Expect(args).NotTo(ContainElement("--speculative-model"), "args: %v", args)
		Expect(args).NotTo(ContainElement("--speculative-config"), "args: %v", args)
	})

	// Parity: no speculative block at all.
	It("emits no speculative flag without a speculative block", func() {
		const name = "vllm-spec-absent"
		target := newSpecModel("vllm-spec-absent-target", false)
		defer cleanup(name, target)

		Expect(newSpecISvc(name, "vllm", target.Name, map[string]interface{}{
			"vllmConfig": map[string]interface{}{"maxModelLen": int64(4096)},
		})).To(Succeed())

		dep, _ := reconcileAndRead(name)
		args := servedArgs(dep)
		Expect(args).NotTo(ContainElement("--speculative-model"), "args: %v", args)
		Expect(args).NotTo(ContainElement("--speculative-config"), "args: %v", args)
	})

	// Parity: a llama.cpp service carrying a leftover vllmConfig block must
	// not gain a mount or a vLLM flag from it.
	It("ignores vllmConfig.speculative on a llama.cpp service", func() {
		const name = "vllm-spec-llamacpp"
		target := newSpecModel("vllm-spec-llamacpp-target", false)
		draft := newSpecModel("vllm-spec-llamacpp-weights", true)
		defer cleanup(name, target, draft)

		Expect(newSpecISvc(name, "llamacpp", target.Name, map[string]interface{}{
			"vllmConfig": map[string]interface{}{
				"speculative": map[string]interface{}{
					"enabled": true,
					"model":   draft.Name,
				},
			},
		})).To(Succeed())

		dep, _ := reconcileAndRead(name)
		names := make([]string, 0, len(dep.Spec.Template.Spec.InitContainers))
		for _, c := range dep.Spec.Template.Spec.InitContainers {
			names = append(names, c.Name)
		}
		Expect(countString(names, "draft-model-downloader")).To(Equal(0),
			"a vllmConfig block must not stage a draft for llama.cpp: %v", names)
		for _, c := range dep.Spec.Template.Spec.Containers {
			Expect(c.Args).NotTo(ContainElement("--speculative-config"))
			Expect(c.Args).NotTo(ContainElement("--speculative-model"))
		}
	})

	// Parity: spec.speculativeDecoding (the llama.cpp field) feeds no vLLM
	// argument; its draft still rides in the pod on both trees.
	It("leaves spec.speculativeDecoding inert for the vllm argv", func() {
		const name = "vllm-spec-llamacppfield"
		target := newSpecModel("vllm-spec-llamacppfield-target", false)
		draft := newSpecModel("vllm-spec-llamacppfield-weights", true)
		defer cleanup(name, target, draft)

		Expect(newSpecISvc(name, "vllm", target.Name, map[string]interface{}{
			"speculativeDecoding": map[string]interface{}{
				"type":          "draft-dspark",
				"draftModelRef": draft.Name,
			},
		})).To(Succeed())

		dep, _ := reconcileAndRead(name)
		args := servedArgs(dep)
		Expect(args).NotTo(ContainElement("--speculative-config"), "args: %v", args)
		Expect(args).NotTo(ContainElement("--speculative-model"), "args: %v", args)
	})

	// Parity: extraArgs carrying --speculative-config wins, exactly once.
	It("lets an extraArgs --speculative-config win", func() {
		const name = "vllm-spec-extraargs"
		target := newSpecModel("vllm-spec-extraargs-target", false)
		defer cleanup(name, target)

		userJSON := `{"method":"dspark","num_speculative_tokens":5,"draft_sample_method":"probabilistic"}`
		Expect(newSpecISvc(name, "vllm", target.Name, map[string]interface{}{
			"extraArgs": []interface{}{"--speculative-config", userJSON},
			"vllmConfig": map[string]interface{}{
				"speculative": map[string]interface{}{
					"enabled":              true,
					"method":               "mtp",
					"numSpeculativeTokens": int64(2),
				},
			},
		})).To(Succeed())

		dep, _ := reconcileAndRead(name)
		args := servedArgs(dep)
		Expect(countArg(args, "--speculative-config")).To(Equal(1), "args: %v", args)
		Expect(args).To(ContainElement(userJSON), "the user's JSON must survive: %v", args)
	})
})

// countString counts occurrences of s in a slice.
func countString(in []string, s string) int {
	n := 0
	for _, v := range in {
		if v == s {
			n++
		}
	}
	return n
}
