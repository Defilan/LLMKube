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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// weightedBuilderRouter is canonicalModelRouter with a second local backend
// and a weighted rule over the two local backends (#2325).
func weightedBuilderRouter(weights ...*int32) (*inferencev1alpha1.ModelRouter, []*inferencev1alpha1.InferenceService) {
	mr := canonicalModelRouter()
	mr.Spec.Backends = append(mr.Spec.Backends, inferencev1alpha1.RouterBackend{
		Name:                "local-gemma",
		InferenceServiceRef: &corev1.LocalObjectReference{Name: "gemma-judge"},
		Tier:                "local",
	})
	mr.Spec.Backends[0].Weight = weights[0]
	mr.Spec.Backends[2].Weight = weights[1]
	mr.Spec.Rules = append(mr.Spec.Rules, inferencev1alpha1.RouterRule{
		Name:  "split",
		Match: &inferencev1alpha1.RuleMatch{Models: []string{"qwen*"}},
		Route: inferencev1alpha1.RuleRoute{
			Backends: []string{"local-qwen", "local-gemma"},
			Strategy: weightedStrategy,
		},
	})
	isvcs := []*inferencev1alpha1.InferenceService{
		{ObjectMeta: metav1.ObjectMeta{Name: "qwen3-coder", Namespace: testBuilderNs}},
		{ObjectMeta: metav1.ObjectMeta{Name: "gemma-judge", Namespace: testBuilderNs}},
	}
	return mr, isvcs
}

func anthropicKeySecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-key", Namespace: testBuilderNs},
		Data:       map[string][]byte{"ANTHROPIC_API_KEY": []byte("test")},
	}
}

// TestCompileRouterConfigRendersWeights proves spec.backends[].weight reaches
// the router-proxy config: a set weight is written as is, an explicit 0
// survives (it drains the backend), and an unset weight is left out so the
// proxy applies the default of 1. The assertions read the compiled bytes the
// ConfigMap carries and decode them with the proxy's own loader.
func TestCompileRouterConfigRendersWeights(t *testing.T) {
	mr, isvcs := weightedBuilderRouter(ptrInt32B(3), ptrInt32B(0))
	// A weighted rule needs one live backend; the canonical cloud backend
	// keeps its weight unset.
	mr.Spec.Rules[2].Route.Backends = append(mr.Spec.Rules[2].Route.Backends, "cloud-opus")
	r := newRouterReconcilerForTest(t, mr, isvcs[0], isvcs[1], anthropicKeySecret())

	compiled, err := r.compileRouterConfig(context.Background(), mr)
	if err != nil {
		t.Fatalf("compileRouterConfig: %v", err)
	}
	var raw struct {
		Backends []map[string]json.RawMessage `json:"backends"`
		Rules    []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(compiled.JSON, &raw); err != nil {
		t.Fatalf("unmarshal compiled JSON: %v", err)
	}
	wantRaw := map[string]string{"local-qwen": "3", "local-gemma": "0", "cloud-opus": ""}
	for _, b := range raw.Backends {
		var name string
		_ = json.Unmarshal(b["name"], &name)
		if got := string(b["weight"]); got != wantRaw[name] {
			t.Errorf("backend %s: wire weight %q, want %q", name, got, wantRaw[name])
		}
	}
	var route struct {
		Strategy string `json:"strategy"`
	}
	if err := json.Unmarshal(raw.Rules[2]["route"], &route); err != nil || route.Strategy != weightedStrategy {
		t.Errorf("rule split: wire strategy %q (err %v), want weighted", route.Strategy, err)
	}

	var cfg router.Config
	if err := json.Unmarshal(compiled.JSON, &cfg); err != nil {
		t.Fatalf("decode as router.Config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("router-proxy rejects the compiled config: %v", err)
	}
	for name, want := range map[string]*int32{"local-qwen": ptrInt32B(3), "local-gemma": ptrInt32B(0), "cloud-opus": nil} {
		got := cfg.BackendByName(name).Weight
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("backend %s: decoded weight %v, want %v", name, got, want)
		}
	}
}

// TestCompileRouterConfigRejectsWeightedPoolSplit: a weighted rule over two
// members of one ModelPool would swap the pool's single GPU slot on a large
// share of requests, so the compile fails with a message naming the pool
// instead of shipping a config that thrashes. The reconciler turns the
// compile error into Available=False/CompileFailed.
func TestCompileRouterConfigRejectsWeightedPoolSplit(t *testing.T) {
	mr, isvcs := weightedBuilderRouter(nil, nil)
	pool := modelPoolFor("heavy-slot", "qwen3-coder", "gemma-judge")
	r := newRouterReconcilerForTest(t, mr, isvcs[0], isvcs[1], anthropicKeySecret(), pool)

	_, err := r.compileRouterConfig(context.Background(), mr)
	if err == nil {
		t.Fatal("compileRouterConfig succeeded, want an error for a weighted rule across one pool's members")
	}
	for _, want := range []string{"split", "heavy-slot", "share one GPU slot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// The same backends under primary-fallback compile: the pool decides
	// residency there, one member at a time.
	mr.Spec.Rules[2].Route.Strategy = ""
	if _, err := r.compileRouterConfig(context.Background(), mr); err != nil {
		t.Errorf("primary-fallback over the same pool members: %v, want nil", err)
	}
}

func TestValidateModelRouterWeightedRule(t *testing.T) {
	cases := []struct {
		name     string
		weights  [2]*int32
		required []string
		caps     [2][]string
		wantErr  bool
	}{
		{name: "unset weights default to 1"},
		{name: "one backend drained", weights: [2]*int32{ptrInt32B(0), nil}},
		{name: "every backend drained", weights: [2]*int32{ptrInt32B(0), ptrInt32B(0)}, wantErr: true},
		{name: "only capable backend drained", weights: [2]*int32{ptrInt32B(0), ptrInt32B(5)},
			required: []string{"vision"}, caps: [2][]string{{"vision"}, nil}, wantErr: true},
		{name: "capable backend live", weights: [2]*int32{ptrInt32B(0), ptrInt32B(5)},
			required: []string{"vision"}, caps: [2][]string{nil, {"vision"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := validRouter()
			for i := range 2 {
				mr.Spec.Backends[i].Weight = tc.weights[i]
				mr.Spec.Backends[i].Capabilities = tc.caps[i]
			}
			mr.Spec.Rules = append(mr.Spec.Rules, inferencev1alpha1.RouterRule{
				Name:  "split",
				Match: &inferencev1alpha1.RuleMatch{RequiredCapabilities: tc.required},
				Route: inferencev1alpha1.RuleRoute{
					Backends: []string{testRouterLocalBackend, testRouterCloudBackend},
					Strategy: weightedStrategy,
				},
			})
			var found bool
			for _, e := range validateModelRouter(mr) {
				if e.Field == "spec.rules[2].route.backends" && strings.Contains(e.Message, "positive weight") {
					found = true
					continue
				}
				t.Errorf("unexpected validation error: %s", e)
			}
			if found != tc.wantErr {
				t.Errorf("weighted rule rejected = %v, want %v", found, tc.wantErr)
			}
		})
	}

	// A drained backend matters only to weighted rules.
	mr := validRouter()
	mr.Spec.Backends[0].Weight = ptrInt32B(0)
	mr.Spec.Backends[1].Weight = ptrInt32B(0)
	if errs := validateModelRouter(mr); len(errs) != 0 {
		t.Errorf("primary-fallback rules with zero weights: %v, want no errors", errs)
	}
}
