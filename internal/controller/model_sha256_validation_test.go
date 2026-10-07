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
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sha256 verifies a single artifact"))
	})

	It("rejects sha256 together with mmproj", func() {
		m := newModel("sha256-cel-mmproj", digest, nil, "proj.gguf")
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sha256 verifies a single artifact"))
	})

	It("admits sha256 on a pre-staged pvc:// source", func() {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "sha256-cel-pvc", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "pvc://models-pvc/model.gguf",
				SHA256: digest,
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("rejects sha256 on a pre-staged oci:// source", func() {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "sha256-cel-oci", Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "oci://registry.example.com/models/llama-3.1-8b@sha256:" + digest,
				SHA256: digest,
			},
		}
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sha256 verifies a single artifact"))
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
		err := k8sClient.Update(ctx, fresh)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sha256 verifies a single artifact"))
	})

	// fileSha256 (multi-file per-file digests, #1978).
	newFileSHA := func(name string, source string, files []string, mmproj string, digests map[string]inferencev1alpha1.SHA256Digest) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:     source,
				Files:      files,
				Mmproj:     mmproj,
				FileSHA256: digests,
			},
		}
	}

	It("admits fileSha256 on a file that names a staged artifact", func() {
		m := newFileSHA("file-sha-ok", "hf://org/repo", []string{"a.gguf", "b.gguf"}, "", map[string]inferencev1alpha1.SHA256Digest{"a.gguf": digest})
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("admits fileSha256 keyed on mmproj", func() {
		m := newFileSHA("file-sha-mmproj", "hf://org/repo", []string{"a.gguf"}, "proj.gguf", map[string]inferencev1alpha1.SHA256Digest{"proj.gguf": digest})
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	})

	It("rejects fileSha256 without files", func() {
		m := newFileSHA("file-sha-nofiles", "hf://org/repo", nil, "", map[string]inferencev1alpha1.SHA256Digest{"a.gguf": digest})
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("fileSha256 requires spec.files"))
	})

	It("rejects a fileSha256 key on mmproj when files is empty", func() {
		// The key names mmproj, so the membership rule admits it; only the
		// "requires files" rule can reject this, which is what distinguishes
		// that rule from the others.
		m := newFileSHA("file-sha-mmproj-nofiles", "hf://org/repo", nil, "proj.gguf", map[string]inferencev1alpha1.SHA256Digest{"proj.gguf": digest})
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("fileSha256 requires spec.files"))
	})

	It("rejects fileSha256 together with sha256", func() {
		m := newFileSHA("file-sha-both", "hf://org/repo", []string{"a.gguf"}, "", map[string]inferencev1alpha1.SHA256Digest{"a.gguf": digest})
		m.Spec.SHA256 = digest
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sha256 verifies a single artifact"))
	})

	It("rejects a fileSha256 value that is not 64 hex", func() {
		m := newFileSHA("file-sha-badval", "hf://org/repo", []string{"a.gguf"}, "", map[string]inferencev1alpha1.SHA256Digest{"a.gguf": "not-a-digest"})
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		// The value is rejected by the SHA256Digest schema pattern on the map
		// value, at the key's path, not by some unrelated fileSha256 rule.
		Expect(err.Error()).To(ContainSubstring("spec.fileSha256.a.gguf"))
		Expect(err.Error()).To(ContainSubstring("^[a-fA-F0-9]{64}$"))
	})

	It("rejects a glob in files when fileSha256 is set", func() {
		// The key equals the glob entry, so the key-membership rule is
		// satisfied and only the no-glob rule can reject this.
		m := newFileSHA("file-sha-glob", "hf://org/repo", []string{"*.gguf"}, "", map[string]inferencev1alpha1.SHA256Digest{"*.gguf": digest})
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("a glob cannot be keyed"))
	})

	It("rejects a fileSha256 key that is not a staged file", func() {
		m := newFileSHA("file-sha-orphankey", "hf://org/repo", []string{"a.gguf"}, "", map[string]inferencev1alpha1.SHA256Digest{"b.gguf": digest})
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("every fileSha256 key must name a spec.files entry or spec.mmproj"))
	})

	It("rejects a fileSha256 key containing whitespace", func() {
		// The file entry carries the leading space so the membership rule is
		// satisfied; only the whitespace rule can reject this.
		m := newFileSHA("file-sha-ws", "hf://org/repo", []string{" a.gguf"}, "", map[string]inferencev1alpha1.SHA256Digest{" a.gguf": digest})
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("fileSha256 keys must not contain whitespace"))
	})

	It("rejects fileSha256 on a pre-staged oci:// source", func() {
		m := newFileSHA("file-sha-oci", "oci://registry.example.com/models/llama-3.1-8b@sha256:"+digest, []string{"a.gguf"}, "", map[string]inferencev1alpha1.SHA256Digest{"a.gguf": digest})
		err := k8sClient.Create(ctx, m)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("fileSha256 cannot verify a pre-staged oci:// artifact"))
	})
})
