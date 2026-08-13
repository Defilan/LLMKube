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
	"strings"
	"testing"
)

func TestMetricsDisabled(t *testing.T) {
	disabled := []string{"", "0", "none", "  0  ", "None"}
	for _, addr := range disabled {
		if !metricsDisabled(addr) {
			t.Errorf("metricsDisabled(%q) = false, want true", addr)
		}
	}

	// "0" is the manager's disable sentinel (cmd/main.go). Before #1517 the
	// proxy treated it as an address, logged "metrics server listening", then
	// failed to bind with "address 0: missing port in address".
	enabled := []string{":9090", "0.0.0.0:9090", "127.0.0.1:9090", ":0"}
	for _, addr := range enabled {
		if metricsDisabled(addr) {
			t.Errorf("metricsDisabled(%q) = true, want false", addr)
		}
	}
}

func TestListenConflict(t *testing.T) {
	conflicting := []struct{ data, metrics string }{
		{":8080", ":8080"},
		{":8080", "0.0.0.0:8080"},
		{"0.0.0.0:8080", ":8080"},
		// A wildcard bind covers every specific address on that port, so it
		// contends with a loopback bind on the same one.
		{":8080", "127.0.0.1:8080"},
		{"127.0.0.1:8080", ":8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
	}
	for _, c := range conflicting {
		err := listenConflict(c.data, c.metrics)
		if err == nil {
			t.Errorf("listenConflict(%q, %q) = nil, want an error", c.data, c.metrics)
			continue
		}
		// The message has to name the conflict, since the whole point is that
		// an operator currently sees either a crash-loop or missing metrics
		// with nothing pointing at the cause.
		for _, want := range []string{c.data, c.metrics, "--listen", "--metrics-bind-address"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("listenConflict(%q, %q) error = %q, want it to mention %q",
					c.data, c.metrics, err, want)
			}
		}
	}

	ok := []struct{ data, metrics string }{
		{":8080", ":9090"},
		{"127.0.0.1:8080", "127.0.0.1:9090"},
		// Different specific hosts on one port is legitimate on a multi-homed
		// node; refusing it would be worse than the bug being fixed.
		{"127.0.0.1:8080", "192.168.1.10:8080"},
		// A malformed address is the binding listener's error to report, not
		// this check's to pre-empt.
		{"not-an-address", ":9090"},
		{":8080", "not-an-address"},
	}
	for _, c := range ok {
		if err := listenConflict(c.data, c.metrics); err != nil {
			t.Errorf("listenConflict(%q, %q) = %v, want nil", c.data, c.metrics, err)
		}
	}
}
