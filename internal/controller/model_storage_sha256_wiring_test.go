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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The verify helpers are the whole of the #1965 enforcement surface: a
// refactor that flips a withSHA256 argument or drops the digest env turns
// verification off without failing anything else. These table tests pin the
// wiring at the storage-config builders (the pod path) and the prefetch Job
// builder (the eager path) for every branch the builders emit.

const (
	sha256WiredDigestUpper = "D9BA44419F2A73ED1A666885066C65A235AB70F337E2B31CBB3D062A5F5B8D4B"
	sha256WiredDigestLower = "d9ba44419f2a73ed1a666885066c65a235ab70f337e2b31cbb3d062a5f5b8d4b"
)

var sha256HelperTokens = []string{
	"llmkube_sha256_hash",
	"llmkube_precheck_sha256",
	"llmkube_check_sha256",
	"llmkube_publish_sha256",
}

func sha256WiringModel(source string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "wired", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: source,
			SHA256: sha256WiredDigestUpper,
		},
	}
}

func downloaderFrom(cfg modelStorageConfig) (corev1.Container, string) {
	for _, c := range cfg.initContainers {
		if c.Name == "model-downloader" && len(c.Command) == 3 {
			return c, c.Command[2]
		}
	}
	return corev1.Container{}, ""
}

func envValueFor(envs []corev1.EnvVar, name string) (string, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func assertDigestEnv(t *testing.T, envs []corev1.EnvVar) {
	t.Helper()
	got, ok := envValueFor(envs, "MODEL_SHA256")
	if !ok {
		t.Fatalf("MODEL_SHA256 not injected: %v", envs)
	}
	if got != sha256WiredDigestLower {
		t.Errorf("MODEL_SHA256 = %q, want lowercased %q", got, sha256WiredDigestLower)
	}
}

func TestModelStorageConfig_SHA256Wiring(t *testing.T) {
	cases := []struct {
		name     string
		useCache bool
		source   string
		policy   string
		// everyBranch are the helper gates that must appear in the command
		// whenever spec.sha256 is set. OnChange http branches additionally
		// carry the marker guard as their first gate.
		everyBranch []string
	}{
		{"cached-http-ifnotpresent", true, "https://models.example.com/model.gguf", RefreshPolicyIfNotPresent, []string{"llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"cached-http-onchange", true, "https://models.example.com/model.gguf", RefreshPolicyOnChange, []string{"llmkube_precheck_sha256", "llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"cached-s3", true, "s3://models/repo/model.gguf", RefreshPolicyIfNotPresent, []string{"llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"cached-hf-ifnotpresent", true, "https://huggingface.co/org/repo/resolve/main/model.gguf", RefreshPolicyIfNotPresent, []string{"llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"cached-hf-onchange", true, "https://huggingface.co/org/repo/resolve/main/model.gguf", RefreshPolicyOnChange, []string{"llmkube_precheck_sha256", "llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"cached-local", true, "file:///data/models/model.gguf", RefreshPolicyIfNotPresent, []string{"llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"uncached-http-ifnotpresent", false, "https://models.example.com/model.gguf", RefreshPolicyIfNotPresent, []string{"llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"uncached-http-onchange", false, "https://models.example.com/model.gguf", RefreshPolicyOnChange, []string{"llmkube_precheck_sha256", "llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"uncached-s3", false, "s3://models/repo/model.gguf", RefreshPolicyIfNotPresent, []string{"llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"uncached-hf-ifnotpresent", false, "https://huggingface.co/org/repo/resolve/main/model.gguf", RefreshPolicyIfNotPresent, []string{"llmkube_check_sha256", "llmkube_publish_sha256"}},
		{"uncached-hf-onchange", false, "https://huggingface.co/org/repo/resolve/main/model.gguf", RefreshPolicyOnChange, []string{"llmkube_precheck_sha256", "llmkube_check_sha256", "llmkube_publish_sha256"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := sha256WiringModel(tc.source)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			}
			cfg := buildModelStorageConfig(model, isvc, "default", tc.useCache, ModelCacheModeShared,
				"", "img:latest", 0, []string{"/data/models"}, "")

			downloader, cmd := downloaderFrom(cfg)
			if downloader.Name == "" {
				t.Fatalf("no model-downloader container in %+v", cfg.initContainers)
			}
			assertDigestEnv(t, downloader.Env)
			for _, tok := range tc.everyBranch {
				if !strings.Contains(cmd, tok) {
					t.Errorf("sha-enabled command is missing %q; gates cannot be silently dropped:\n%s", tok, cmd)
				}
			}
			if strings.Contains(tc.source, "s3://") && !strings.Contains(cmd, "--aws-sigv4") {
				t.Errorf("S3 branch lost its sigv4 transfer")
			}
		})
	}
}

func TestModelStorageConfig_NoDigestStaysUnwired(t *testing.T) {
	for _, source := range []string{
		"https://models.example.com/model.gguf",
		"s3://models/repo/model.gguf",
		"file:///data/models/model.gguf",
		"https://huggingface.co/org/repo/resolve/main/model.gguf",
	} {
		for _, useCache := range []bool{true, false} {
			if !useCache && strings.HasPrefix(source, "file://") {
				continue // no cache to copy into: the error branch, no gates expected
			}
			for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
				model := sha256WiringModel(source)
				model.Spec.SHA256 = ""
				model.Spec.RefreshPolicy = policy
				isvc := &inferencev1alpha1.InferenceService{
					ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				}
				cfg := buildModelStorageConfig(model, isvc, "default", useCache, ModelCacheModeShared,
					"", "img:latest", 0, []string{"/data/models"}, "")
				downloader, cmd := downloaderFrom(cfg)
				if downloader.Name == "" {
					t.Fatalf("no model-downloader container (source=%s useCache=%v policy=%s)", source, useCache, policy)
				}
				if v, ok := envValueFor(downloader.Env, "MODEL_SHA256"); ok {
					t.Errorf("MODEL_SHA256 = %q without spec.sha256 (source=%s useCache=%v policy=%s)", v, source, useCache, policy)
				}
				for _, tok := range sha256HelperTokens {
					if strings.Contains(cmd, tok) {
						t.Errorf("unhashed command contains %q (source=%s useCache=%v policy=%s)", tok, source, useCache, policy)
					}
				}
			}
		}
	}
}

func TestPrefetchJob_SHA256Wiring(t *testing.T) {
	build := func(t *testing.T, sha256 string) (corev1.Container, string) {
		t.Helper()
		model := sha256WiringModel("https://models.example.com/model.gguf")
		model.Spec.SHA256 = sha256
		seedPrefetchCacheKey(model)
		target := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		}
		r := &ModelReconciler{InitContainerImage: "img:latest"}
		job, err := r.buildPrefetchJob(context.Background(), model, target)
		if err != nil {
			t.Fatalf("buildPrefetchJob: %v", err)
		}
		cfg := modelStorageConfig{initContainers: job.Spec.Template.Spec.InitContainers}
		downloader, cmd := downloaderFrom(cfg)
		if downloader.Name == "" {
			t.Fatalf("prefetch Job has no model-downloader init container")
		}
		return downloader, cmd
	}

	t.Run("digest set", func(t *testing.T) {
		downloader, cmd := build(t, sha256WiredDigestUpper)
		assertDigestEnv(t, downloader.Env)
		for _, tok := range []string{"llmkube_check_sha256", "llmkube_publish_sha256"} {
			if !strings.Contains(cmd, tok) {
				t.Errorf("prefetch command is missing %q:\n%s", tok, cmd)
			}
		}
	})
	t.Run("digest unset", func(t *testing.T) {
		downloader, cmd := build(t, "")
		if _, ok := envValueFor(downloader.Env, "MODEL_SHA256"); ok {
			t.Errorf("prefetch injected MODEL_SHA256 without spec.sha256")
		}
		for _, tok := range sha256HelperTokens {
			if strings.Contains(cmd, tok) {
				t.Errorf("prefetch command of an unhashed Model contains %q", tok)
			}
		}
	})
}
