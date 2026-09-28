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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoClusterKubeconfig has two contexts on different API servers. The current
// context is "work"; the agent's cluster is "lab".
const twoClusterKubeconfig = `apiVersion: v1
kind: Config
current-context: work
clusters:
- name: work
  cluster:
    server: https://work.example:6443
- name: lab
  cluster:
    server: https://lab.example:6443
users:
- name: agent
  user:
    token: test-token
contexts:
- name: work
  context:
    cluster: work
    user: agent
- name: lab
  context:
    cluster: lab
    user: agent
`

func writeKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(twoClusterKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

func TestLoadKubeconfig(t *testing.T) {
	writeKubeconfig(t)

	tests := []struct {
		name     string
		context  string
		wantHost string
	}{
		{name: "empty uses current context", context: "", wantHost: "https://work.example:6443"},
		{name: "explicit context wins over current", context: "lab", wantHost: "https://lab.example:6443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadKubeconfig(tt.context)
			if err != nil {
				t.Fatalf("loadKubeconfig(%q): %v", tt.context, err)
			}
			if cfg.Host != tt.wantHost {
				t.Errorf("loadKubeconfig(%q).Host = %q, want %q", tt.context, cfg.Host, tt.wantHost)
			}
		})
	}
}

func TestLoadKubeconfig_UnknownContext(t *testing.T) {
	writeKubeconfig(t)

	_, err := loadKubeconfig("prod")
	if err == nil {
		t.Fatal("loadKubeconfig with an unknown context succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("error %q does not name the missing context", err)
	}
}
