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
	"path/filepath"
	"testing"
)

// TestMain clears the proxy environment so the SSRF guard tests are
// hermetic: a developer's HTTP_PROXY would otherwise send test hostnames
// such as mirror.test to a real proxy. Tests that exercise proxy behavior
// set these variables themselves with t.Setenv.
//
// It also clears systemTempRoots: many tests build a model store in
// t.TempDir(), which is under /tmp wherever TMPDIR is unset (Linux CI), and
// the store check would refuse all of them. The /tmp refusal tests set it
// back explicitly with withSystemTempRoots.
func TestMain(m *testing.M) {
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy", "REQUEST_METHOD"} {
		_ = os.Unsetenv(k)
	}
	systemTempRoots = nil
	os.Exit(m.Run())
}

// withSystemTempRoots restores the production systemTempRoots for one test.
func withSystemTempRoots(t *testing.T) {
	t.Helper()
	prev := systemTempRoots
	systemTempRoots = []string{"/private/tmp", "/tmp", "/private/var/tmp", "/var/tmp"}
	t.Cleanup(func() { systemTempRoots = prev })
}

// The production default refuses every shared temporary directory, not just
// /tmp: checkStoreNotInTmp is judged on both spellings of each root.
func TestCheckStoreNotInTmp_RefusesAllProductionRoots(t *testing.T) {
	withSystemTempRoots(t)
	for _, root := range []string{"/private/tmp", "/tmp", "/private/var/tmp", "/var/tmp"} {
		where := filepath.Join(root, "llmkube-models")
		if err := checkStoreNotInTmp(where, where, where); err == nil {
			t.Errorf("checkStoreNotInTmp(%s) = nil, want a refusal", where)
		}
	}
}
