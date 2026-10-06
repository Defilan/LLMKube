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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise the Model CRD's server-side CEL rule that keeps
// spec.sha256 to single-file downloaded Models (#1965): the digest attests to
// one artifact, and the init-container gates are built only for the
// single-file, controller-downloaded branch, so a multi-file Model, or a
// pre-staged pvc:// / oci:// Model, with a digest must be rejected at
// admission rather than have its digest silently ignored.
var _ = Describe("Model sha256 CRD validation", func() {
	ctx := context.Background()
	const digest = "d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b"

	newModel := func(name string, sha256 string, files []string, mmproj string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/" + name + ".gguf",
				SHA256: sha256,
				Files:  files,
				Mmproj: mmproj,
			},
		}
	}

	It("admits sha256 on a single-file Model", func() {
		m := newModel("sha256-cel-single", digest, nil, "")
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("admits files and mmproj without sha256", func() {
		m := newModel("sha256-cel-multi", "", []string{"a.gguf", "b.gguf"}, "proj.gguf")
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("rejects sha256 together with files", func() {
		m := newModel("sha256-cel-files", digest, []string{"a.gguf", "b.gguf"}, "")
		Expect(k8sClient.Create(ctx, m)).ToNot(Succeed())
	})

	It("rejects sha256 together with mmproj", func() {
		m := newModel("sha256-cel-mmproj", digest, nil, "proj.gguf")
		Expect(k8sClient.Create(ctx, m)).ToNot(Succeed())
	})

	It("rejects sha256 on a pre-staged pvc:// source", func() {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "sha256-cel-pvc", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "pvc://models-pvc/model.gguf",
				SHA256: digest,
			},
		}
		Expect(k8sClient.Create(ctx, m)).ToNot(Succeed())
	})

	It("rejects sha256 on a pre-staged oci:// source", func() {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "sha256-cel-oci", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "oci://registry.example.com/models/llama-3.1-8b@sha256:" + digest,
				SHA256: digest,
			},
		}
		Expect(k8sClient.Create(ctx, m)).ToNot(Succeed())
	})

	It("admits a pvc:// source without sha256", func() {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "sha256-cel-pvc-ok", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "pvc://models-pvc/model.gguf",
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("admits an oci:// source without sha256", func() {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "sha256-cel-oci-ok", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "oci://registry.example.com/models/llama-3.1-8b@sha256:" + digest,
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("rejects adding sha256 to an existing multi-file Model", func() {
		m := newModel("sha256-cel-update", "", []string{"a.gguf"}, "")
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, m)).To(Succeed()) }()
		fresh := &inferencev1alpha1.Model{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), fresh)).To(Succeed())
		fresh.Spec.SHA256 = digest
		Expect(k8sClient.Update(ctx, fresh)).ToNot(Succeed())
	})
})
