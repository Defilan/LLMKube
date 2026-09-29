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

package agent

import (
	"os"
	"testing"
)

// TestMain clears the proxy environment so the SSRF guard tests are
// hermetic: a developer's HTTP_PROXY would otherwise send test hostnames
// such as mirror.test to a real proxy. Tests that exercise proxy behavior
// set these variables themselves with t.Setenv.
//
// It also moves t.TempDir() out of /tmp when that is where it lives (Linux
// CI, where TMPDIR is unset), because the model store check refuses any
// store under /tmp and many tests build a store in t.TempDir(). On macOS
// TMPDIR is under /var/folders and nothing moves.
func TestMain(m *testing.M) {
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy", "REQUEST_METHOD"} {
		_ = os.Unsetenv(k)
	}
	cleanup := moveTempDirOutOfTmp()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// moveTempDirOutOfTmp points TMPDIR at a fresh directory that passes the
// model store check when the current temp dir does not, trying the user
// cache dir and then /dev/shm. It returns a cleanup that removes the
// directory it made, and leaves TMPDIR alone when no candidate works (the
// store tests then fail and name /tmp).
func moveTempDirOutOfTmp() func() {
	if storeCheckPassesUnder(os.TempDir()) {
		return func() {}
	}
	var candidates []string
	if cache, err := os.UserCacheDir(); err == nil {
		candidates = append(candidates, cache)
	}
	candidates = append(candidates, "/dev/shm")
	for _, parent := range candidates {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			continue
		}
		dir, err := os.MkdirTemp(parent, "llmkube-agent-test-")
		if err != nil {
			continue
		}
		if storeCheckPassesUnder(dir) {
			_ = os.Setenv("TMPDIR", dir)
			return func() { _ = os.RemoveAll(dir) }
		}
		_ = os.RemoveAll(dir)
	}
	return func() {}
}

// storeCheckPassesUnder reports whether a private directory created under
// parent passes CheckModelStore.
func storeCheckPassesUnder(parent string) bool {
	probe, err := os.MkdirTemp(parent, "store-probe-")
	if err != nil {
		return false
	}
	defer func() { _ = os.RemoveAll(probe) }()
	return CheckModelStore(probe) == nil
}
