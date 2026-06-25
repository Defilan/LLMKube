/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestUsesGenericGate(t *testing.T) {
	cases := []struct {
		name    string
		profile *foremanv1alpha1.GateProfile
		want    bool
	}{
		{"nil profile -> go path", nil, false},
		{"empty language -> go path", &foremanv1alpha1.GateProfile{}, false},
		{"explicit go -> go path", &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguageGo}, false},
		{"python -> generic", &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguagePython}, true},
		{"rust -> generic", &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguageRust}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesGenericGate(tc.profile); got != tc.want {
				t.Errorf("usesGenericGate = %v, want %v", got, tc.want)
			}
		})
	}
}

type gateCall struct {
	name string
	args []string
}

func TestRunGenericGateAllPassRunsEachViaShC(t *testing.T) {
	var calls []gateCall
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		calls = append(calls, gateCall{name, args})
		return "", nil
	}
	gate := foremanv1alpha1.ResolvedGate{
		Format: "ruff format --check .",
		Lint:   "ruff check .",
		Build:  "python -m compileall .",
		Test:   "pytest -q",
	}
	pass, fb := RunGenericGate(context.Background(), "/ws", gate, run)
	if !pass || fb != "" {
		t.Fatalf("want pass with empty feedback; got pass=%v feedback=%q", pass, fb)
	}
	if len(calls) != 4 {
		t.Fatalf("want 4 commands run (empty codegen skipped), got %d: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if c.name != "sh" || len(c.args) != 2 || c.args[0] != "-c" {
			t.Errorf("command not run via `sh -c`: %+v", c)
		}
	}
}

func TestRunGenericGateSkipsEmptyAndCollectsFailures(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		if strings.Contains(cmd, "ruff check") || strings.Contains(cmd, "pytest") {
			return "boom", errors.New("exit status 1")
		}
		return "", nil
	}
	gate := foremanv1alpha1.ResolvedGate{
		// Format, Build, Codegen empty -> skipped.
		Lint: "ruff check .",
		Test: "pytest -q",
	}
	pass, fb := RunGenericGate(context.Background(), "/ws", gate, run)
	if pass {
		t.Fatal("want fail when lint and test fail")
	}
	if !strings.Contains(fb, "ruff check") || !strings.Contains(fb, "pytest") {
		t.Errorf("feedback should name both failing checks; got %q", fb)
	}
}
