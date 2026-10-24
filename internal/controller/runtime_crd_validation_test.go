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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs run spec.runtime through the envtest apiserver, so they pin
// the generated CRD schema rather than the Go type. #783 made the metal-agent
// honor spec.runtime per CR (#525) but left the CRD enum and default alone:
// the API server rejected every Metal runtime and stamped "llamacpp" on every
// CR, so neither half of #525 could work and no unit test noticed.
var _ = Describe("InferenceService spec.runtime CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name, runtime string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "runtime-crd-model",
				Runtime:  runtime,
			},
		}
	}

	for _, rt := range []string{"mlx-server", "omlx", "vllm-swift", "ollama", "tensorfold"} {
		It("admits the metal-agent runtime "+rt, func() {
			isvc := newISvc("rt-metal-"+rt, rt)
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

			stored := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: "default"}, stored)).To(Succeed())
			Expect(stored.Spec.Runtime).To(Equal(rt))
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		})
	}

	// An unset runtime must stay unset in storage: the metal-agent reads ""
	// as "use my --runtime flag", and the controller reads it as llamacpp.
	It("stores an unset runtime as empty instead of defaulting it", func() {
		isvc := newISvc("rt-unset", "")
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: "default"}, stored)).To(Succeed())
		Expect(stored.Spec.Runtime).To(BeEmpty())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("still rejects a runtime outside the enum", func() {
		err := k8sClient.Create(ctx, newISvc("rt-bogus", "llama-server"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.runtime"))
	})
})

// These specs exercise spec.vllmConfig.stallDetection through the envtest
// apiserver, pinning the generated CRD schema: the window bounds are enforced
// at apply time and the field admits without a window (the controller
// resolves the 120s default).
var _ = Describe("InferenceService spec.vllmConfig.stallDetection CRD validation", func() {
	ctx := context.Background()

	newStallISvc := func(name string, stall *inferencev1alpha1.VLLMStallDetection) *inferencev1alpha1.InferenceService {
		enabled := true
		stall.Enabled = &enabled
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "stall-crd-model",
				Runtime:  "vllm",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					StallDetection: stall,
				},
			},
		}
	}

	It("rejects a windowSeconds below the minimum", func() {
		tooSmall := int32(5)
		err := k8sClient.Create(ctx, newStallISvc("stall-window-low",
			&inferencev1alpha1.VLLMStallDetection{WindowSeconds: &tooSmall}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("windowSeconds"))
	})

	It("rejects a windowSeconds above the maximum", func() {
		tooBig := int32(4000)
		err := k8sClient.Create(ctx, newStallISvc("stall-window-high",
			&inferencev1alpha1.VLLMStallDetection{WindowSeconds: &tooBig}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("windowSeconds"))
	})

	It("admits an enabled stall detection without a window", func() {
		isvc := newStallISvc("stall-no-window",
			&inferencev1alpha1.VLLMStallDetection{})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: "default"}, stored)).To(Succeed())
		Expect(stored.Spec.VLLMConfig.StallDetection.WindowSeconds).To(BeNil())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})
})

// spec.sglangConfig.stallDetection carries the same schema bounds.
var _ = Describe("InferenceService spec.sglangConfig.stallDetection CRD validation", func() {
	ctx := context.Background()

	newSGLangStallISvc := func(name string, stall *inferencev1alpha1.SGLangStallDetection) *inferencev1alpha1.InferenceService {
		enabled := true
		stall.Enabled = &enabled
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:     "stall-crd-model",
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{StallDetection: stall},
			},
		}
	}

	It("rejects a windowSeconds below the minimum", func() {
		tooSmall := int32(5)
		err := k8sClient.Create(ctx, newSGLangStallISvc("sglang-stall-window-low",
			&inferencev1alpha1.SGLangStallDetection{WindowSeconds: &tooSmall}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("windowSeconds"))
	})

	It("rejects a windowSeconds above the maximum", func() {
		tooBig := int32(4000)
		err := k8sClient.Create(ctx, newSGLangStallISvc("sglang-stall-window-high",
			&inferencev1alpha1.SGLangStallDetection{WindowSeconds: &tooBig}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("windowSeconds"))
	})

	It("admits an enabled stall detection without a window", func() {
		isvc := newSGLangStallISvc("sglang-stall-no-window", &inferencev1alpha1.SGLangStallDetection{})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		_ = k8sClient.Delete(ctx, isvc)
	})
})

// These specs run spec.vllmConfig.kvCacheDtype and spec.sglangConfig.kvCacheDtype
// through the envtest apiserver, so they pin the generated CRD schema rather
// than the Go type. The Go type carries no enum: widening or narrowing the
// +kubebuilder:validation:Enum marker is invisible to every unit test, and only
// the schema decides what admission accepts (#2191).
//
// The rejections here are deliberate. vLLM accepts more cache types than the
// enum (fp8_ds_mla, fp8_inc, nvfp4, the turboquant_* family) but those are
// version- and hardware-specific and belong on kvCacheCustomDtype. SGLang's
// --kv-cache-dtype rejects fp8 and float16 outright, so its enum must not grow
// with vLLM's.
var _ = Describe("InferenceService vllmConfig.kvCacheDtype CRD validation", func() {
	ctx := context.Background()

	// kubeName turns a dtype into an RFC 1123 label: fp8_e4m3 carries
	// underscores, which object names reject.
	kubeName := func(dtype string) string {
		return strings.ReplaceAll(dtype, "_", "-")
	}

	newDtypeISvc := func(name, dtype string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "kv-dtype-crd-model",
				Runtime:  "vllm",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{
					KVCacheDtype: &dtype,
				},
			},
		}
	}

	for _, dt := range []string{"fp8", "float16", "bfloat16"} {
		It("admits kvCacheDtype="+dt, func() {
			isvc := newDtypeISvc("kv-dtype-"+dt, dt)
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

			stored := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: "default"}, stored)).To(Succeed())
			Expect(stored.Spec.VLLMConfig.KVCacheDtype).NotTo(BeNil())
			Expect(*stored.Spec.VLLMConfig.KVCacheDtype).To(Equal(dt))
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		})
	}

	for _, dt := range []string{"auto", "fp8_e5m2", "fp8_e4m3"} {
		It("still admits kvCacheDtype="+dt, func() {
			isvc := newDtypeISvc("kv-dtype-keep-"+kubeName(dt), dt)
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		})
	}

	It("stores the auto default when kvCacheDtype is unset", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "kv-dtype-unset", Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:   "kv-dtype-crd-model",
				Runtime:    "vllm",
				VLLMConfig: &inferencev1alpha1.VLLMConfig{},
			},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: "default"}, stored)).To(Succeed())
		Expect(stored.Spec.VLLMConfig.KVCacheDtype).NotTo(BeNil())
		Expect(*stored.Spec.VLLMConfig.KVCacheDtype).To(Equal("auto"))
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	for _, dt := range []string{"fp8_ds_mla", "fp8_inc", "nvfp4", "turboquant_3bit_nc", "not-a-dtype"} {
		It("still rejects kvCacheDtype="+dt, func() {
			err := k8sClient.Create(ctx, newDtypeISvc("kv-dtype-bad-"+dt, dt))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("kvCacheDtype"))
		})
	}

	// kvCacheCustomDtype stays a free string so a value the enum now carries
	// still works there, and an upstream value outside the enum still does.
	for _, custom := range []string{"fp8", "turboquant_3bit_nc"} {
		It("admits kvCacheCustomDtype="+custom+" unvalidated", func() {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "kv-custom-" + kubeName(custom), Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "kv-dtype-crd-model",
					Runtime:  "vllm",
					VLLMConfig: &inferencev1alpha1.VLLMConfig{
						KVCacheCustomDtype: custom,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

			stored := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: "default"}, stored)).To(Succeed())
			Expect(stored.Spec.VLLMConfig.KVCacheCustomDtype).To(Equal(custom))
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		})
	}

	newSGLangDtypeISvc := func(name, dtype string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:     "kv-dtype-crd-model",
				Runtime:      "sglang",
				SGLangConfig: &inferencev1alpha1.SGLangConfig{KVCacheDtype: &dtype},
			},
		}
	}

	for _, dt := range []string{"fp8", "float16"} {
		It("still rejects sglangConfig.kvCacheDtype="+dt, func() {
			err := k8sClient.Create(ctx, newSGLangDtypeISvc("sglang-kv-dtype-bad-"+dt, dt))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("kvCacheDtype"))
		})
	}

	It("still admits sglangConfig.kvCacheDtype=fp8_e4m3", func() {
		isvc := newSGLangDtypeISvc("sglang-kv-dtype-e4m3", "fp8_e4m3")
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})
})
