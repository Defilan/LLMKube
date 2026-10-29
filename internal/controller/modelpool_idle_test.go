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
	"errors"
	"fmt"
	"net/url"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// refusedProbeErr is the error a real net/http client returns when nothing is
// listening on the other end, wrapped the way net/http wraps it.
func refusedProbeErr() error {
	return &url.Error{
		Op:  "Get",
		URL: "http://judge.default.svc.cluster.local:8080/slots",
		Err: fmt.Errorf("dial tcp 10.0.0.7:8080: %w", syscall.ECONNREFUSED),
	}
}

func runningPod(name string, deleting bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "server",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	if deleting {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"test/finalizer"}
	}
	return pod
}

func waitingPod(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "server",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
}

// TestMemberIdleVerdict pins the classification of a member that reports Ready:
// the idle probe answers first, and the Pod state decides only when the probe is
// refused. A refusal with no serving Pod (no Pod, all terminating, no running
// container) or with exactly one running Pod is idle, a refusal among two or
// more running Pods stays unknown, and every other probe failure stays unknown.
func TestMemberIdleVerdict(t *testing.T) {
	tests := []struct {
		name    string
		pods    []corev1.Pod
		idle    bool
		probeIn error
		want    idleVerdict
	}{
		{
			name: "probe answers idle with no Pods",
			idle: true,
			want: idleVerdictIdle,
		},
		{
			// The probe answers first: a busy server whose Pod status has not
			// caught up must not be unloaded.
			name: "probe answers busy with no Pods stays busy",
			idle: false,
			want: idleVerdictBusy,
		},
		{
			name:    "no Pods and a refused dial is not serving",
			probeIn: refusedProbeErr(),
			want:    idleVerdictIdle,
		},
		{
			name:    "all pods terminating and a refused dial is not serving",
			pods:    []corev1.Pod{runningPod("judge-1", true), runningPod("judge-2", true)},
			probeIn: refusedProbeErr(),
			want:    idleVerdictIdle,
		},
		{
			name:    "no running container and a refused dial is not serving",
			pods:    []corev1.Pod{waitingPod("judge-1")},
			probeIn: refusedProbeErr(),
			want:    idleVerdictIdle,
		},
		{
			name: "one running pod answering idle is idle",
			pods: []corev1.Pod{runningPod("judge-1", false)},
			idle: true,
			want: idleVerdictIdle,
		},
		{
			name: "one running pod answering busy is busy",
			pods: []corev1.Pod{runningPod("judge-1", false)},
			idle: false,
			want: idleVerdictBusy,
		},
		{
			name:    "one running pod refusing the dial is not accepting work",
			pods:    []corev1.Pod{runningPod("judge-1", false)},
			probeIn: refusedProbeErr(),
			want:    idleVerdictIdle,
		},
		{
			name:    "refused dial with two running pods is ambiguous",
			pods:    []corev1.Pod{runningPod("judge-1", false), runningPod("judge-2", false)},
			probeIn: refusedProbeErr(),
			want:    idleVerdictUnknown,
		},
		{
			name:    "a timeout with no Pods stays ambiguous",
			probeIn: errors.New("dial tcp 10.0.0.7:8080: i/o timeout"),
			want:    idleVerdictUnknown,
		},
		{
			name:    "a non-refused probe failure stays ambiguous",
			pods:    []corev1.Pod{runningPod("judge-1", false)},
			probeIn: errors.New("dial tcp 10.0.0.7:8080: i/o timeout"),
			want:    idleVerdictUnknown,
		},
		{
			// The progress response llama.cpp returns on every path while the
			// model is still loading (server-http.cpp middleware_server_state).
			name:    "the loading 503 stays ambiguous",
			pods:    []corev1.Pod{runningPod("judge-1", false)},
			probeIn: errors.New("/slots returned status 503"),
			want:    idleVerdictUnknown,
		},
		{
			name:    "a runtime with no idle probe stays ambiguous",
			pods:    []corev1.Pod{runningPod("judge-1", false)},
			probeIn: errIdleUnsupported,
			want:    idleVerdictUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := &ModelPoolReconciler{
				IdleCheck: func(_ context.Context, _ *inferencev1alpha1.InferenceService) (bool, error) {
					calls++
					return tc.idle, tc.probeIn
				},
			}
			got := r.memberIdleVerdict(context.Background(), &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "judge", Namespace: "default"},
			}, tc.pods)
			if got != tc.want {
				t.Errorf("memberIdleVerdict = %v, want %v", got, tc.want)
			}
			// The probe answers first whatever the Pods show: a server can report
			// in-flight work before its Pod status catches up (#2130 review).
			if calls != 1 {
				t.Errorf("probe called %d times, want 1", calls)
			}
		})
	}
}
