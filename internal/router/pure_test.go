/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"bytes"
	"io"
	"testing"
)

// ---------------------------------------------------------------------------
// matchModels
// ---------------------------------------------------------------------------

func TestMatchModelsEmptyPatterns(t *testing.T) {
	// Empty pattern list matches anything (match-all semantics).
	if !matchModels(nil, "any-model") {
		t.Error("nil patterns should match any model")
	}
	if !matchModels([]string{}, "any-model") {
		t.Error("empty patterns should match any model")
	}
}

func TestMatchModelsEmptyModel(t *testing.T) {
	// An empty model only matches if a pattern is "*" or the pattern list is empty.
	if !matchModels([]string{"*"}, "") {
		t.Error("wildcard pattern should match empty model")
	}
	if !matchModels([]string{"qwen3-*", "*"}, "") {
		t.Error("wildcard in list should match empty model")
	}
	if matchModels([]string{"qwen3-*"}, "") {
		t.Error("non-wildcard patterns should not match empty model")
	}
	if matchModels([]string{"qwen3-*", "llama-*"}, "") {
		t.Error("no wildcard in list should not match empty model")
	}
}

func TestMatchModelsGlobPatterns(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		model    string
		want     bool
	}{
		{"exact match", []string{"qwen3-coder"}, "qwen3-coder", true},
		{"star prefix", []string{"*coder"}, "qwen3-coder", true},
		{"star suffix", []string{"qwen3-*"}, "qwen3-coder-30b", true},
		{"star middle", []string{"qwen3-*-30b"}, "qwen3-coder-30b", true},
		{"no match", []string{"llama-*"}, "qwen3-coder", false},
		{"multiple patterns first matches", []string{"llama-*", "qwen3-*"}, "qwen3-coder", true},
		{"multiple patterns none match", []string{"llama-*", "mistral-*"}, "qwen3-coder", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchModels(tc.patterns, tc.model)
			if got != tc.want {
				t.Errorf("matchModels(%v, %q) = %v, want %v", tc.patterns, tc.model, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// matchHeaders
// ---------------------------------------------------------------------------

func TestMatchHeadersEmptyWant(t *testing.T) {
	// Empty want map matches anything.
	if !matchHeaders(nil, map[string]string{"x-team": "research"}) {
		t.Error("nil want should match any headers")
	}
	if !matchHeaders(map[string]string{}, map[string]string{"x-team": "research"}) {
		t.Error("empty want should match any headers")
	}
}

func TestMatchHeadersMissingHeader(t *testing.T) {
	want := map[string]string{"X-Team": "research"}
	have := map[string]string{}
	if matchHeaders(want, have) {
		t.Error("missing header should fail the match")
	}
}

func TestMatchHeadersValueMismatch(t *testing.T) {
	want := map[string]string{"X-Team": "research"}
	have := map[string]string{"x-team": "engineering"}
	if matchHeaders(want, have) {
		t.Error("value mismatch should fail the match")
	}
}

func TestMatchHeadersMultipleHeaders(t *testing.T) {
	want := map[string]string{
		"X-Team":        "research",
		"X-Environment": "prod",
	}
	have := map[string]string{
		"x-team":        "research",
		"x-environment": "prod",
	}
	if !matchHeaders(want, have) {
		t.Error("all headers present and matching should succeed")
	}
}

func TestMatchHeadersOneMissingOfMultiple(t *testing.T) {
	want := map[string]string{
		"X-Team":        "research",
		"X-Environment": "prod",
	}
	have := map[string]string{
		"x-team": "research",
	}
	if matchHeaders(want, have) {
		t.Error("one missing header should fail the match")
	}
}

// ---------------------------------------------------------------------------
// satisfiesRequiredCapabilities
// ---------------------------------------------------------------------------

func TestSatisfiesRequiredCapabilitiesEmptyRequired(t *testing.T) {
	cfg := validConfig()
	m := NewMatcher(cfg)
	if !m.satisfiesRequiredCapabilities(nil, []string{"local-qwen"}) {
		t.Error("nil required should always pass")
	}
	if !m.satisfiesRequiredCapabilities([]string{}, []string{"local-qwen"}) {
		t.Error("empty required should always pass")
	}
}

func TestSatisfiesRequiredCapabilitiesMissingBackend(t *testing.T) {
	cfg := validConfig()
	m := NewMatcher(cfg)
	if m.satisfiesRequiredCapabilities([]string{"vision"}, []string{"ghost"}) {
		t.Error("missing backend should fail capability check")
	}
}

func TestSatisfiesRequiredCapabilitiesNoBackendHasCapability(t *testing.T) {
	cfg := validConfig()
	cfg.Backends[0].Capabilities = []string{"code"}
	cfg.Backends[1].Capabilities = []string{"code"}
	m := NewMatcher(cfg)
	if m.satisfiesRequiredCapabilities([]string{"vision"}, []string{"local-qwen", "cloud-opus"}) {
		t.Error("no backend with vision should fail")
	}
}

func TestSatisfiesRequiredCapabilitiesOneBackendHasAll(t *testing.T) {
	cfg := validConfig()
	cfg.Backends[0].Capabilities = []string{"code"}
	cfg.Backends[1].Capabilities = []string{"vision", "code"}
	m := NewMatcher(cfg)
	if !m.satisfiesRequiredCapabilities([]string{"vision"}, []string{"local-qwen", "cloud-opus"}) {
		t.Error("one backend with vision should pass")
	}
}

func TestSatisfiesRequiredCapabilitiesMultipleRequired(t *testing.T) {
	cfg := validConfig()
	cfg.Backends[0].Capabilities = []string{"vision"}
	cfg.Backends[1].Capabilities = []string{"vision", "code", "long-context"}
	m := NewMatcher(cfg)
	if !m.satisfiesRequiredCapabilities([]string{"vision", "code"}, []string{"local-qwen", "cloud-opus"}) {
		t.Error("backend with all required capabilities should pass")
	}
}

func TestSatisfiesRequiredCapabilitiesNoSingleBackendHasAll(t *testing.T) {
	cfg := validConfig()
	cfg.Backends[0].Capabilities = []string{"vision"}
	cfg.Backends[1].Capabilities = []string{"code"}
	m := NewMatcher(cfg)
	if m.satisfiesRequiredCapabilities([]string{"vision", "code"}, []string{"local-qwen", "cloud-opus"}) {
		t.Error("no single backend with all required capabilities should fail")
	}
}

// ---------------------------------------------------------------------------
// hasAllCapabilities
// ---------------------------------------------------------------------------

func TestHasAllCapabilities(t *testing.T) {
	cases := []struct {
		name string
		have []string
		want []string
		ok   bool
	}{
		{"empty want", []string{"vision"}, nil, true},
		{"empty want slice", []string{"vision"}, []string{}, true},
		{"all present", []string{"vision", "code"}, []string{"vision", "code"}, true},
		{"extra have", []string{"vision", "code", "long-context"}, []string{"vision", "code"}, true},
		{"missing one", []string{"vision"}, []string{"vision", "code"}, false},
		{"none present", []string{"vision"}, []string{"code"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasAllCapabilities(tc.have, tc.want)
			if got != tc.ok {
				t.Errorf("hasAllCapabilities(%v, %v) = %v, want %v", tc.have, tc.want, got, tc.ok)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// contains
// ---------------------------------------------------------------------------

func TestContains(t *testing.T) {
	cases := []struct {
		name   string
		hay    []string
		needle string
		want   bool
	}{
		{"found", []string{"pii", "phi"}, "pii", true},
		{"not found", []string{"pii", "phi"}, "secret", false},
		{"empty haystack", nil, "pii", false},
		{"empty needle", []string{"pii"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contains(tc.hay, tc.needle)
			if got != tc.want {
				t.Errorf("contains(%v, %q) = %v, want %v", tc.hay, tc.needle, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// strategyOrDefault
// ---------------------------------------------------------------------------

func TestStrategyOrDefault(t *testing.T) {
	if got := strategyOrDefault(""); got != strategyPrimaryFallback {
		t.Errorf("empty string should default to %q, got %q", strategyPrimaryFallback, got)
	}
	if got := strategyOrDefault("weighted"); got != "weighted" {
		t.Errorf("non-empty should pass through, got %q", got)
	}
	if got := strategyOrDefault("shadow"); got != "shadow" {
		t.Errorf("non-empty should pass through, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Config.BackendByName
// ---------------------------------------------------------------------------

func TestConfigBackendByName(t *testing.T) {
	cfg := validConfig()

	if got := cfg.BackendByName("local-qwen"); got == nil {
		t.Fatal("expected to find local-qwen")
	}
	if got := cfg.BackendByName("local-qwen"); got.Name != "local-qwen" {
		t.Errorf("got name %q, want local-qwen", got.Name)
	}

	if got := cfg.BackendByName("ghost"); got != nil {
		t.Errorf("expected nil for unknown backend, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Config.Validate - rule name required
// ---------------------------------------------------------------------------

func TestConfigValidateRuleNameRequired(t *testing.T) {
	cfg := validConfig()
	cfg.Rules[0].Name = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty rule name")
	}
}

// ---------------------------------------------------------------------------
// Config.SensitiveSet - empty override falls back to defaults
// ---------------------------------------------------------------------------

func TestSensitiveSetEmptyOverride(t *testing.T) {
	cfg := &Config{
		Policy: Policy{
			Classification: ClassificationPolicy{
				Sensitive: []string{},
			},
		},
	}
	got := cfg.SensitiveSet()
	// SensitiveSet only applies an override when len(Sensitive) > 0, so an
	// empty slice is treated as "not configured" and the defaults still apply.
	for _, want := range []string{"pii", "phi"} {
		if !got[want] {
			t.Errorf("empty override should fall back to defaults; missing %q (got %v)", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Config.ClassificationHeader - whitespace-only key
// ---------------------------------------------------------------------------

func TestClassificationHeaderWhitespaceOnly(t *testing.T) {
	cfg := &Config{
		Policy: Policy{
			Classification: ClassificationPolicy{HeaderKey: "   "},
		},
	}
	if got := cfg.ClassificationHeader(); got != "x-llmkube-classification" {
		t.Errorf("whitespace-only header key should fall back to default, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// joinURL
// ---------------------------------------------------------------------------

func TestJoinURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{"both empty", "", "", ""},
		{"base only", "http://x", "", "http://x"},
		{"path only", "", "/v1", "/v1"},
		{"no trailing slash", "http://x", "/v1", "http://x/v1"},
		{"trailing slash base", "http://x/", "/v1", "http://x/v1"},
		{"no leading slash path", "http://x", "v1", "http://x/v1"},
		{"both slashes", "http://x/", "v1", "http://x/v1"},
		{"no slashes", "http://x", "v1", "http://x/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := joinURL(tc.base, tc.path)
			if got != tc.want {
				t.Errorf("joinURL(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PipeBody
// ---------------------------------------------------------------------------

func TestPipeBody(t *testing.T) {
	src := bytes.NewReader([]byte("hello world"))
	var dst bytes.Buffer
	n, err := PipeBody(&dst, src, nil)
	if err != nil {
		t.Fatalf("PipeBody: %v", err)
	}
	if n != 11 {
		t.Errorf("bytes written = %d, want 11", n)
	}
	if dst.String() != "hello world" {
		t.Errorf("dst = %q, want %q", dst.String(), "hello world")
	}
}

func TestPipeBodyWithFlush(t *testing.T) {
	src := bytes.NewReader([]byte("chunked"))
	var dst bytes.Buffer
	var flushCalled int
	flush := func() { flushCalled++ }
	n, err := PipeBody(&dst, src, flush)
	if err != nil {
		t.Fatalf("PipeBody: %v", err)
	}
	if n != 7 {
		t.Errorf("bytes written = %d, want 7", n)
	}
	if flushCalled == 0 {
		t.Error("flush should have been called at least once")
	}
}

func TestPipeBodyReadError(t *testing.T) {
	src := &errorReader{err: io.ErrUnexpectedEOF}
	var dst bytes.Buffer
	_, err := PipeBody(&dst, src, nil)
	if err == nil {
		t.Fatal("expected error from PipeBody")
	}
}

type errorReader struct {
	err error
}

func (e *errorReader) Read(p []byte) (int, error) {
	return 0, e.err
}

// ---------------------------------------------------------------------------
// streamedReason
// ---------------------------------------------------------------------------

func TestStreamedReason(t *testing.T) {
	if got := streamedReason(true); got != "ok_stream" {
		t.Errorf("streamedReason(true) = %q, want ok_stream", got)
	}
	if got := streamedReason(false); got != "ok" {
		t.Errorf("streamedReason(false) = %q, want ok", got)
	}
}

// ---------------------------------------------------------------------------
// enforceFailClosed - backend not configured
// ---------------------------------------------------------------------------

func TestEnforceFailClosedBackendNotConfigured(t *testing.T) {
	cfg := validConfig()
	proxy := NewProxy(cfg, nil)

	f := &RequestFeatures{Classification: "pii"}
	dec := &MatchResult{FailClosed: true, Backends: []string{"ghost"}}
	err := proxy.enforceFailClosed(f, dec)
	if err == nil {
		t.Fatal("expected error for unconfigured backend")
	}
}

// ---------------------------------------------------------------------------
// enforceFailClosed - sensitive to cloud tier
// ---------------------------------------------------------------------------

func TestEnforceFailClosedSensitiveToCloud(t *testing.T) {
	cfg := validConfig()
	proxy := NewProxy(cfg, nil)

	f := &RequestFeatures{Classification: "pii"}
	dec := &MatchResult{FailClosed: true, Backends: []string{"cloud-opus"}}
	err := proxy.enforceFailClosed(f, dec)
	if err == nil {
		t.Fatal("expected error: sensitive classification cannot route to cloud-tier backend")
	}
}

// ---------------------------------------------------------------------------
// enforceFailClosed - non-sensitive classification passes
// ---------------------------------------------------------------------------

func TestEnforceFailClosedNonSensitivePasses(t *testing.T) {
	cfg := validConfig()
	proxy := NewProxy(cfg, nil)

	f := &RequestFeatures{Classification: "public"}
	dec := &MatchResult{FailClosed: true, Backends: []string{"cloud-opus"}}
	err := proxy.enforceFailClosed(f, dec)
	if err != nil {
		t.Fatalf("non-sensitive classification should pass fail-closed check: %v", err)
	}
}

// ---------------------------------------------------------------------------
// enforceFailClosed - fail-closed false passes
// ---------------------------------------------------------------------------

func TestEnforceFailClosedNotFailClosed(t *testing.T) {
	cfg := validConfig()
	proxy := NewProxy(cfg, nil)

	f := &RequestFeatures{Classification: "pii"}
	dec := &MatchResult{FailClosed: false, Backends: []string{"cloud-opus"}}
	err := proxy.enforceFailClosed(f, dec)
	if err != nil {
		t.Fatalf("non-fail-closed rule should pass: %v", err)
	}
}

// ---------------------------------------------------------------------------
// enforceFailClosed - sensitive to local tier passes
// ---------------------------------------------------------------------------

func TestEnforceFailClosedSensitiveToLocal(t *testing.T) {
	cfg := validConfig()
	proxy := NewProxy(cfg, nil)

	f := &RequestFeatures{Classification: "pii"}
	dec := &MatchResult{FailClosed: true, Backends: []string{"local-qwen"}}
	err := proxy.enforceFailClosed(f, dec)
	if err != nil {
		t.Fatalf("sensitive classification to local-tier backend should pass: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Matcher.BackendByName
// ---------------------------------------------------------------------------

func TestMatcherBackendByName(t *testing.T) {
	cfg := validConfig()
	m := NewMatcher(cfg)

	if got := m.BackendByName("local-qwen"); got == nil {
		t.Fatal("expected to find local-qwen")
	}
	if got := m.BackendByName("local-qwen"); got.Name != "local-qwen" {
		t.Errorf("got name %q, want local-qwen", got.Name)
	}

	if got := m.BackendByName("ghost"); got != nil {
		t.Errorf("expected nil for unknown backend, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Match - empty model with BackendNameMatch and no default
// ---------------------------------------------------------------------------

func TestMatchEmptyModelBackendNameMatchNoDefault(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = nil
	cfg.DefaultRoute = ""
	cfg.DefaultRouteStrategy = DefaultRouteStrategyBackendNameMatch
	m := NewMatcher(cfg)
	got := m.Match(&RequestFeatures{Model: ""})
	if len(got.Backends) != 0 {
		t.Errorf("empty model with no default should return no backends, got %v", got.Backends)
	}
}

// ---------------------------------------------------------------------------
// Match - multiple rules with different match dimensions
// ---------------------------------------------------------------------------

func TestMatchMultipleDimensions(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = []Rule{
		{
			Name:  "pii-and-complex",
			Match: RuleMatch{DataClassification: []string{"pii"}, TaskComplexity: "complex"},
			Route: RuleRoute{Backends: []string{"local-qwen"}},
		},
	}
	m := NewMatcher(cfg)

	// Both dimensions match.
	got := m.Match(&RequestFeatures{Classification: "pii", TaskComplexity: "complex"})
	if got.Rule == nil {
		t.Fatal("expected rule match when both dimensions match")
	}

	// Only one dimension matches.
	got = m.Match(&RequestFeatures{Classification: "pii", TaskComplexity: "simple"})
	if got.Rule != nil {
		t.Errorf("expected no match when only one dimension matches, got %q", got.Rule.Name)
	}
}

// ---------------------------------------------------------------------------
// Match - model wildcard with empty model
// ---------------------------------------------------------------------------

func TestMatchModelWildcardWithEmptyModel(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = []Rule{{
		Name:  "wildcard-rule",
		Match: RuleMatch{Models: []string{"*"}},
		Route: RuleRoute{Backends: []string{"local-qwen"}},
	}}
	m := NewMatcher(cfg)
	got := m.Match(&RequestFeatures{Model: ""})
	if got.Rule == nil {
		t.Error("wildcard model pattern should match empty model")
	}
}

// ---------------------------------------------------------------------------
// Match - rule with strategy set
// ---------------------------------------------------------------------------

func TestMatchRuleWithStrategy(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = []Rule{{
		Name:  "weighted-rule",
		Match: RuleMatch{Models: []string{"*"}},
		Route: RuleRoute{Backends: []string{"local-qwen"}, Strategy: "weighted"},
	}}
	m := NewMatcher(cfg)
	got := m.Match(&RequestFeatures{Model: "any"})
	if got.Strategy != "weighted" {
		t.Errorf("strategy = %q, want weighted", got.Strategy)
	}
}

// ---------------------------------------------------------------------------
// Match - rule with shadow strategy
// ---------------------------------------------------------------------------

func TestMatchRuleWithShadowStrategy(t *testing.T) {
	cfg := validConfig()
	cfg.Rules = []Rule{{
		Name:  "shadow-rule",
		Match: RuleMatch{Models: []string{"*"}},
		Route: RuleRoute{Backends: []string{"local-qwen"}, Strategy: "shadow"},
	}}
	m := NewMatcher(cfg)
	got := m.Match(&RequestFeatures{Model: "any"})
	if got.Strategy != "shadow" {
		t.Errorf("strategy = %q, want shadow", got.Strategy)
	}
}
