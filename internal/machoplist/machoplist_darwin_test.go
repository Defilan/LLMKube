//go:build darwin

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

package machoplist

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPlistEntries is the number of keys in testPlist, which codesign reports
// as "Info.plist entries=N" once it has bound the plist.
const testPlistEntries = 7

// TestInjectedPlistIsBoundByCodesign is the oracle check: Apple's own codesign
// must accept the edited binary, bind the embedded plist (it reports the
// entry count and takes the identifier from CFBundleIdentifier when none is
// given), verify it strictly, and the binary must still run.
func TestInjectedPlistIsBoundByCodesign(t *testing.T) {
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		t.Skip("codesign not available")
	}
	for _, tg := range []target{{arch: "arm64", stripped: true}, {arch: "amd64", stripped: true}} {
		t.Run(tg.String(), func(t *testing.T) {
			out, err := Inject(darwinBinary(t, tg), []byte(testPlist))
			if err != nil {
				t.Fatalf("Inject: %v", err)
			}
			bin := filepath.Join(t.TempDir(), "hello")
			if err := os.WriteFile(bin, out, 0o755); err != nil {
				t.Fatalf("write: %v", err)
			}

			runOK(t, codesign, "--force", "--sign", "-", bin)
			info := runOK(t, codesign, "-dv", "--verbose=4", bin)
			for _, want := range []string{
				"Identifier=dev.llmkube.machoplist-test",
				fmt.Sprintf("Info.plist entries=%d", testPlistEntries),
			} {
				if !strings.Contains(info, want) {
					t.Fatalf("codesign -dv output lacks %q:\n%s", want, info)
				}
			}
			runOK(t, codesign, "--verify", "--strict", "--verbose=2", bin)

			if tg.arch != runtime.GOARCH && !canRunAMD64() {
				t.Logf("skipping execution: %s binaries cannot run on this %s host", tg.arch, runtime.GOARCH)
				return
			}
			if got := runOK(t, bin, "--version"); strings.TrimSpace(got) != "hello 1.2.3" {
				t.Fatalf("--version printed %q", got)
			}
		})
	}
}

// canRunAMD64 reports whether Rosetta (or a native Intel CPU) can execute
// x86_64 binaries here.
func canRunAMD64() bool {
	if runtime.GOARCH == "amd64" {
		return true
	}
	return exec.Command("/usr/bin/arch", "-x86_64", "/usr/bin/true").Run() == nil
}

func runOK(t *testing.T, name string, args ...string) string {
	t.Helper()
	b, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, b)
	}
	return string(b)
}
