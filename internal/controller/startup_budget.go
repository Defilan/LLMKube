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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultProbePeriodSeconds is the kubelet's periodSeconds default for a probe
// that leaves it unset. No built-in runtime does, but a probeOverrides.startup
// in a stored object can.
const defaultProbePeriodSeconds int32 = 10

// startupProbePeriod returns the probe's period, falling back to the kubelet
// default when it is unset.
func startupProbePeriod(p *corev1.Probe) int32 {
	if p != nil && p.PeriodSeconds > 0 {
		return p.PeriodSeconds
	}
	return defaultProbePeriodSeconds
}

// startupFailureThreshold converts a startup budget into the failureThreshold
// that covers it at the probe's period: the kubelet's startup window is
// failureThreshold * periodSeconds. A single probe that is still running when
// the threshold is reached restarts the container, so the budget is rounded up
// to the next period and floored at 1. A non-positive timeout returns 0,
// meaning the probe's own budget stands.
func startupFailureThreshold(timeout time.Duration, p *corev1.Probe) int32 {
	if timeout <= 0 {
		return 0
	}
	period := int64(startupProbePeriod(p))
	total := int64(timeout / time.Second)
	if time.Duration(total)*time.Second < timeout {
		total++
	}
	threshold := total / period
	if total%period != 0 {
		threshold++
	}
	if threshold < 1 {
		threshold = 1
	}
	if threshold > math.MaxInt32 {
		threshold = math.MaxInt32
	}
	return int32(threshold)
}

// startupWindow is the budget a startup probe covers: failureThreshold times
// period. Zero for a nil probe or an unset threshold.
func startupWindow(p *corev1.Probe) time.Duration {
	if p == nil || p.FailureThreshold <= 0 {
		return 0
	}
	return time.Duration(p.FailureThreshold) * time.Duration(startupProbePeriod(p)) * time.Second
}

// applyStartupTimeout widens the startup probe's budget to spec.startupTimeout,
// leaving its handler, period and timeout as the runtime built them.
func applyStartupTimeout(timeout *metav1.Duration, startup *corev1.Probe) {
	if timeout == nil || startup == nil {
		return
	}
	if threshold := startupFailureThreshold(timeout.Duration, startup); threshold > 0 {
		startup.FailureThreshold = threshold
	}
}
