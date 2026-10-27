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

package main

import (
	"bytes"
	"debug/macho"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const plist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>CFBundleIdentifier</key>
    <string>dev.llmkube.cmd-test</string>
</dict>
</plist>
`

// buildDarwinARM64 builds a trivial program with the internal linker, as
// the release does, so the command runs against a real Go Mach-O.
func buildDarwinARM64(t *testing.T, dir string) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not available; cannot build a darwin test binary")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module hello\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "hello")
	cmd := exec.Command(goTool, "build", "-trimpath", "-ldflags=-s -w", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestRunRewritesBinaryInPlace(t *testing.T) {
	dir := t.TempDir()
	bin := buildDarwinARM64(t, dir)
	if err := os.Chmod(bin, 0o750); err != nil { //nolint:gosec // G302: the rewrite must keep a non-default mode
		t.Fatal(err)
	}
	plistPath := filepath.Join(t.TempDir(), "Info.plist")
	writeFile(t, plistPath, plist, 0o644)

	var stderr bytes.Buffer
	if code := run([]string{"-plist", plistPath, bin}, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
	}

	f, err := macho.Open(bin)
	if err != nil {
		t.Fatalf("rewritten binary does not parse: %v", err)
	}
	defer func() { _ = f.Close() }()
	sect := f.Section("__info_plist")
	if sect == nil {
		t.Fatal("rewritten binary has no __info_plist section")
	}
	data, err := sect.Data()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != plist {
		t.Fatalf("__info_plist = %q", data)
	}
	st, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o750 {
		t.Fatalf("mode = %v, want the original 0750", st.Mode().Perm())
	}
	if got := dirEntries(t, dir); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("directory holds %v after the rewrite, want only the binary (no temp files left)", got)
	}
}

func TestRunFailureLeavesBinaryUntouched(t *testing.T) {
	dir := t.TempDir()
	bin := buildDarwinARM64(t, dir)
	plistPath := filepath.Join(t.TempDir(), "Info.plist")
	writeFile(t, plistPath, plist, 0o644)

	// A first run embeds the plist; the second must refuse and change nothing.
	var stderr bytes.Buffer
	if code := run([]string{"-plist", plistPath, bin}, &stderr); code != 0 {
		t.Fatalf("first run: exit %d, stderr:\n%s", code, stderr.String())
	}
	before, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}

	stderr.Reset()
	code := run([]string{"-plist", plistPath, bin}, &stderr)
	if code != 1 {
		t.Fatalf("second run: exit %d, want 1", code)
	}
	msg := stderr.String()
	if !strings.Contains(msg, "already has an __info_plist section") || !strings.Contains(msg, bin) {
		t.Fatalf("stderr does not name the binary and the cause:\n%s", msg)
	}
	after, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed run modified the binary")
	}
	if got := dirEntries(t, dir); len(got) != 1 {
		t.Fatalf("directory holds %v after a failed run, want only the binary", got)
	}
}

func TestRunErrors(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "script.sh")
	writeFile(t, script, "#!/bin/sh\necho hi\n", 0o755)
	plistPath := filepath.Join(dir, "Info.plist")
	writeFile(t, plistPath, plist, 0o644)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(script, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantMsg  string
	}{
		{"no arguments", nil, 2, "usage"},
		{"missing -plist", []string{script}, 2, "usage"},
		{"two binaries", []string{"-plist", plistPath, script, script}, 2, "usage"},
		{"unknown flag", []string{"-bogus", script}, 2, "usage"},
		{"unreadable plist", []string{"-plist", filepath.Join(dir, "missing.plist"), script}, 1, "missing.plist"},
		{"missing binary", []string{"-plist", plistPath, filepath.Join(dir, "nope")}, 1, "nope"},
		{"not a Mach-O", []string{"-plist", plistPath, script}, 1, "not a Mach-O"},
		{"symlinked binary", []string{"-plist", plistPath, link}, 1, "symlink"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := run(tc.args, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d; stderr:\n%s", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantMsg) {
				t.Fatalf("stderr lacks %q:\n%s", tc.wantMsg, stderr.String())
			}
		})
	}
	if b, _ := os.ReadFile(script); string(b) != "#!/bin/sh\necho hi\n" {
		t.Fatal("a failed run modified the input file")
	}
}
