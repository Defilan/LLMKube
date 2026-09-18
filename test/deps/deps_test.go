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

package deps

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestGrpcMeetsCVE202684445Floor pins google.golang.org/grpc at or above the
// CVE-2026-84445 fix (v1.83.2, DoS via xDS). grpc reaches every binary
// transitively through otlptracegrpc, so nothing else raises it.
func TestGrpcMeetsCVE202684445Floor(t *testing.T) {
	const (
		module    = "google.golang.org/grpc"
		fixedFrom = "v1.83.2"
		cve       = "CVE-2026-84445"
	)

	got := requireVersion(t, module)
	if compareSemver(got, fixedFrom) < 0 {
		t.Fatalf("%s %s is below the %s fixed floor %s", module, got, cve, fixedFrom)
	}
}

func requireVersion(t *testing.T, module string) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file to find go.mod")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("open go.mod: %v", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == module {
			return fields[1]
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	t.Fatalf("go.mod does not require %s", module)
	return ""
}

func compareSemver(a, b string) int {
	pa, pb := parseSemver(a), parseSemver(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] - pb[i]
		}
	}
	return 0
}

func parseSemver(v string) [3]int {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		out[i], _ = strconv.Atoi(parts[i])
	}
	return out
}
