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

package main

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
)

// TestClientsetPodLogReader_ReadPodLogs covers the production PodLogReader
// implementation (cmd's clientsetPodLogReader) that backs
// status.acceleration (#1385). It is the only code path that turns a
// kubernetes clientset into the log text the controller parses, so a test that
// deletes it must fail. The fake clientset's Pods.GetLogs returns a canned
// "fake logs" body for a matched action and honors a reactor's error, so both
// the happy and error paths are exercised without a live cluster.
func TestClientsetPodLogReader_ReadPodLogs(t *testing.T) {
	ctx := context.Background()
	ns, pod, container := "default", "svc-1", "llama-server"

	t.Run("returns the log body", func(t *testing.T) {
		cs := fakeclientset.NewSimpleClientset()
		r := &clientsetPodLogReader{cs: cs}

		got, err := r.ReadPodLogs(ctx, ns, pod, container, 100)
		if err != nil {
			t.Fatalf("ReadPodLogs returned error: %v", err)
		}
		if got != "fake logs" {
			t.Errorf("ReadPodLogs = %q, want %q", got, "fake logs")
		}
	})

	t.Run("propagates the clientset error", func(t *testing.T) {
		cs := fakeclientset.NewSimpleClientset()
		boom := errors.New("log stream unavailable")
		cs.PrependReactor("get", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
			return true, nil, boom
		})
		r := &clientsetPodLogReader{cs: cs}

		got, err := r.ReadPodLogs(ctx, ns, pod, container, 100)
		if !errors.Is(err, boom) {
			t.Errorf("ReadPodLogs error = %v, want %v", err, boom)
		}
		if got != "" {
			t.Errorf("ReadPodLogs = %q, want empty string on error", got)
		}
	})

	t.Run("passes the container and tail options through", func(t *testing.T) {
		cs := fakeclientset.NewSimpleClientset()
		r := &clientsetPodLogReader{cs: cs}

		_, _ = r.ReadPodLogs(ctx, ns, pod, container, 2048)

		var opts *corev1.PodLogOptions
		for _, a := range cs.Actions() {
			impl, ok := a.(clientgotesting.GenericActionImpl)
			if !ok || impl.Verb != "get" || impl.Subresource != "log" {
				continue
			}
			if o, ok := impl.Value.(*corev1.PodLogOptions); ok {
				opts = o
			}
		}
		if opts == nil {
			t.Fatal("no 'get pods/log' action with PodLogOptions recorded; the reader did not call the expected subresource")
		}
		if opts.Container != container {
			t.Errorf("PodLogOptions.Container = %q, want %q", opts.Container, container)
		}
		if opts.TailLines == nil || *opts.TailLines != 2048 {
			t.Errorf("PodLogOptions.TailLines = %v, want 2048", opts.TailLines)
		}
	})
}
