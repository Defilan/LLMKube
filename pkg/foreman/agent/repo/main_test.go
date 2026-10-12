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

package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustEnv sets name=value or exits: a failed Setenv would leave git
// reading the developer's configuration and the suite silently
// non-hermetic.
func mustEnv(name, value string) {
	if err := os.Setenv(name, value); err != nil {
		fmt.Fprintf(os.Stderr, "repo tests: set %s: %v\n", name, err)
		os.Exit(1)
	}
}

// TestMain pins git's configuration sources to empty files so a
// developer's ~/.gitconfig cannot rewrite URLs or otherwise change
// results. HOME and XDG_CONFIG_HOME alone do not defeat a
// shell-exported GIT_CONFIG_GLOBAL, which baseEnv forwards, so all
// three location variables are pinned too; GIT_CONFIG_NOSYSTEM covers
// /etc/gitconfig, which HOME cannot (#2013).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "llmkube-repo-git-env-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "repo tests: create hermetic home: %v\n", err)
		os.Exit(1)
	}
	global := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "repo tests: create empty gitconfig: %v\n", err)
		os.Exit(1)
	}
	system := filepath.Join(dir, "gitconfig-system")
	if err := os.WriteFile(system, nil, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "repo tests: create empty system gitconfig: %v\n", err)
		os.Exit(1)
	}
	mustEnv("HOME", dir)
	mustEnv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	mustEnv("GIT_CONFIG_GLOBAL", global)
	mustEnv("GIT_CONFIG_SYSTEM", system)
	mustEnv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestGitEnvIsHermetic pins TestMain: if git's configuration sources are
// not redirected to temp files, a developer's ~/.gitconfig leaks into
// every git call this package makes (issue #2013).
func TestGitEnvIsHermetic(t *testing.T) {
	if home := os.Getenv("HOME"); !strings.HasPrefix(home, os.TempDir()) {
		t.Errorf("HOME = %q, want a directory under %q so no developer gitconfig can leak in", home, os.TempDir())
	}
	if nosystem := os.Getenv("GIT_CONFIG_NOSYSTEM"); nosystem != "1" {
		t.Errorf("GIT_CONFIG_NOSYSTEM = %q, want \"1\" so /etc/gitconfig cannot leak in", nosystem)
	}
}
