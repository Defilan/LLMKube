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

// Command machoplist embeds an Info.plist into a Go-built macOS executable as
// a __TEXT,__info_plist section (see internal/machoplist). The release runs
// it from hack/macos-sign.sh, the GoReleaser post-build hook, before the
// binary is signed.
//
// Usage:
//
//	machoplist -plist <Info.plist> <binary>
//
// The binary is rewritten in place atomically (a temp file in the same
// directory, renamed over it) and keeps its permissions; on any error it is
// left untouched and the command exits 1. Usage errors exit 2. The result
// must be signed again before it can run on Apple Silicon.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/defilantech/llmkube/internal/machoplist"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("machoplist", flag.ContinueOnError)
	fs.SetOutput(stderr)
	plistPath := fs.String("plist", "", "path to the Info.plist (XML) to embed")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: machoplist -plist <Info.plist> <binary>")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *plistPath == "" || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	bin := fs.Arg(0)
	if err := rewrite(bin, *plistPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "machoplist: %s: %v\n", bin, err)
		return 1
	}
	return 0
}

func rewrite(bin, plistPath string) error {
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		return fmt.Errorf("read plist: %w", err)
	}
	st, err := os.Lstat(bin)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to rewrite a symlink; pass the binary itself")
	}
	if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	in, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	out, err := machoplist.Inject(in, plist)
	if err != nil {
		return err
	}
	return replaceFile(bin, out, st.Mode().Perm())
}

// replaceFile writes data next to path and renames it over path, so a crash
// or a failed write never leaves a half-written binary behind.
func replaceFile(path string, data []byte, perm os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".machoplist-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
