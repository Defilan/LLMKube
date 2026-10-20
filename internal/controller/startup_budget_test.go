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
	"math"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestStartupBudget pins the arithmetic spec.startupTimeout relies on. The
// expected thresholds are the kubelet's own window rule (a startup probe
// covers failureThreshold * periodSeconds and the container is restarted once
// failureThreshold probes fail in a row), so 90m at a 10s period needs 540.
func TestStartupBudget(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		period  int32
		want    int32
	}{
		{name: "90m at 10s", timeout: 90 * time.Minute, period: 10, want: 540},
		{name: "30m at 10s matches the built-in budget", timeout: 30 * time.Minute, period: 10, want: 180},
		{name: "25s at 10s rounds up", timeout: 25 * time.Second, period: 10, want: 3},
		{name: "exactly one period", timeout: 10 * time.Second, period: 10, want: 1},
		{name: "below one period floors at 1", timeout: time.Second, period: 10, want: 1},
		{name: "uses the probe's own period", timeout: 90 * time.Minute, period: 30, want: 180},
		{name: "unset period defaults to 10s", timeout: 90 * time.Minute, period: 0, want: 540},
		{name: "zero is a no-op", timeout: 0, period: 10, want: 0},
		{name: "negative is a no-op", timeout: -5 * time.Minute, period: 10, want: 0},
		{name: "huge clamps instead of wrapping", timeout: time.Duration(math.MaxInt64), period: 1, want: math.MaxInt32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := startupFailureThreshold(tc.timeout, &corev1.Probe{PeriodSeconds: tc.period}); got != tc.want {
				t.Errorf("startupFailureThreshold(%v, period %d) = %d, want %d", tc.timeout, tc.period, got, tc.want)
			}
		})
	}

	windows := []struct {
		name  string
		probe *corev1.Probe
		want  time.Duration
	}{
		{name: "nil probe", probe: nil, want: 0},
		{name: "90m", probe: &corev1.Probe{FailureThreshold: 540, PeriodSeconds: 10}, want: 90 * time.Minute},
		{name: "unset period defaults to 10s", probe: &corev1.Probe{FailureThreshold: 180}, want: 30 * time.Minute},
		{name: "no threshold", probe: &corev1.Probe{PeriodSeconds: 10}, want: 0},
	}
	for _, tc := range windows {
		t.Run("window "+tc.name, func(t *testing.T) {
			if got := startupWindow(tc.probe); got != tc.want {
				t.Errorf("startupWindow(%+v) = %v, want %v", tc.probe, got, tc.want)
			}
		})
	}
}

// TestApplyStartupTimeout checks the helper touches only the threshold.
func TestApplyStartupTimeout(t *testing.T) {
	newProbe := func() *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health_generate"},
			},
			PeriodSeconds:    10,
			TimeoutSeconds:   5,
			FailureThreshold: 180,
		}
	}

	t.Run("widens the threshold only", func(t *testing.T) {
		p := newProbe()
		applyStartupTimeout(&metav1.Duration{Duration: 90 * time.Minute}, p)
		if p.FailureThreshold != 540 {
			t.Errorf("failureThreshold = %d, want 540", p.FailureThreshold)
		}
		if p.PeriodSeconds != 10 || p.TimeoutSeconds != 5 {
			t.Errorf("period/timeout changed: %d/%d", p.PeriodSeconds, p.TimeoutSeconds)
		}
		if p.HTTPGet == nil || p.HTTPGet.Path != "/health_generate" {
			t.Errorf("handler changed: %+v", p.ProbeHandler)
		}
	})

	t.Run("nil timeout leaves the probe alone", func(t *testing.T) {
		p := newProbe()
		applyStartupTimeout(nil, p)
		if p.FailureThreshold != 180 {
			t.Errorf("failureThreshold = %d, want the built-in 180", p.FailureThreshold)
		}
	})

	t.Run("nil probe does not panic", func(t *testing.T) {
		applyStartupTimeout(&metav1.Duration{Duration: 90 * time.Minute}, nil)
	})
}
