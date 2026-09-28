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

// Package policy decides which paths and engine flags the metal-agent will
// accept from InferenceService and Model specs. It has no dependency on the
// agent package so both the agent and future admission code can use it.
package policy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Roots is the set of directories model files and engine path arguments must
// resolve into. Each entry is absolute, cleaned and symlink-resolved.
type Roots struct{ dirs []string }

// NewRoots resolves the configured roots. Empty entries are skipped, a
// leading "~" or "~/" is expanded against home before the absolute check (a
// bare "~" or a "~user" form with home == "" is an error, failing closed
// rather than silently treating the entry as relative), a relative entry is
// an error, and an entry that does not exist is returned in ignored so the
// caller can log it.
func NewRoots(paths []string, home string) (Roots, []string, error) {
	var r Roots
	var ignored []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		switch {
		case p == "~":
			if home == "" {
				return Roots{}, nil, fmt.Errorf("allowed model root %q: cannot expand ~: home directory unknown", p)
			}
			p = home
		case strings.HasPrefix(p, "~/"):
			if home == "" {
				return Roots{}, nil, fmt.Errorf("allowed model root %q: cannot expand ~: home directory unknown", p)
			}
			p = home + string(filepath.Separator) + strings.TrimPrefix(p, "~/")
		case strings.HasPrefix(p, "~"):
			return Roots{}, nil, fmt.Errorf("allowed model root %q: ~user paths are not supported", p)
		}
		if !filepath.IsAbs(p) {
			return Roots{}, nil, fmt.Errorf("allowed model root %q must be an absolute path", p)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(p))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				ignored = append(ignored, p)
				continue
			}
			return Roots{}, nil, fmt.Errorf("resolve allowed model root %q: %w", p, err)
		}
		r.dirs = append(r.dirs, resolved)
	}
	return r, ignored, nil
}

// Dirs returns the resolved root directories.
func (r Roots) Dirs() []string { return append([]string(nil), r.dirs...) }

// IsPathShaped reports whether a flag value names a filesystem path by its
// prefix: absolute, home-relative, explicitly relative, or a file:// URI.
func IsPathShaped(v string) bool {
	switch {
	case strings.HasPrefix(v, "/"), strings.HasPrefix(v, "~"),
		strings.HasPrefix(v, "./"), strings.HasPrefix(v, "../"):
		return true
	}
	return len(v) >= len("file://") && strings.EqualFold(v[:len("file://")], "file://")
}

// StripFileScheme removes a case-insensitive file:// prefix.
func StripFileScheme(v string) string {
	if len(v) >= len("file://") && strings.EqualFold(v[:len("file://")], "file://") {
		return v[len("file://"):]
	}
	return v
}

// ResolvePath turns p into an absolute, symlink-resolved path: a file://
// prefix is stripped, "~" or "~/..." expands to home, a relative path is
// joined to workDir, and symlinks are resolved. A path that does not exist
// yet is resolved through its deepest existing parent, so a file an engine is
// about to create is judged by where it would land. workDir must already be
// absolute; callers pass the agent's own working directory or a value they
// derived from it. A "~"-prefixed p is refused outright when home is empty:
// expanding to "" would turn "~/x" into the workDir-relative path "x", silently
// changing what the path means instead of failing closed.
//
// Any ".." path component in p is refused outright, before any filepath.Join
// or filepath.Clean runs. Clean collapses ".." lexically, but a ".." that
// follows a symlink component must be resolved against the symlink's target,
// not against its literal path on disk, and the standard library has no call
// that does that safely. Refusing ".." unconditionally is therefore
// deliberately blunt: it also refuses a harmless "sub/../file" that would in
// fact stay inside a root, in exchange for never letting "link/../escape"
// resolve to somewhere outside one.
//
// A dangling symlink (one whose target does not exist) is refused rather
// than treated as a not-yet-existing path: only a component that genuinely
// does not exist is walked past.
func ResolvePath(p, workDir, home string) (string, error) {
	p = StripFileScheme(p)
	if p == "" {
		return "", errors.New("path is empty")
	}
	if strings.HasPrefix(p, "~") {
		if home == "" {
			return "", errors.New("cannot expand ~: home directory unknown")
		}
		switch {
		case p == "~":
			p = home
		case strings.HasPrefix(p, "~/"):
			p = home + string(filepath.Separator) + strings.TrimPrefix(p, "~/")
		default:
			return "", fmt.Errorf("path %q: ~user paths are not supported", p)
		}
	}
	if !filepath.IsAbs(p) {
		p = workDir + string(filepath.Separator) + p
	}
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("path %q contains a %q component, which is refused", p, "..")
		}
	}
	p = filepath.Clean(p)
	cur, rest := p, ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %q: %w", p, err)
		}
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("dangling symlink %q", cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Contains reports whether a resolved path is a root or lies beneath one.
// The comparison is a byte-for-byte prefix match and therefore
// case-sensitive: on a case-insensitive filesystem (the default for APFS) a
// path that differs from a root only in case is treated as outside it and
// fails closed.
func (r Roots) Contains(resolved string) bool {
	for _, d := range r.dirs {
		if resolved == d {
			return true
		}
		prefix := d
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if strings.HasPrefix(resolved, prefix) {
			return true
		}
	}
	return false
}

// PathError reports a path that either could not be resolved (Err is set) or
// that resolved cleanly but lies outside every allowed root (Resolved is
// set).
type PathError struct {
	What     string // e.g. "model path", "extraArgs value for --lora"
	Path     string
	Resolved string
	Roots    []string
	Err      error
}

// Unwrap exposes the underlying resolve error, if any, to errors.Is/As.
func (e *PathError) Unwrap() error { return e.Err }

func (e *PathError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s %s cannot be resolved: %v", e.What, e.Path, e.Err)
	}
	return fmt.Sprintf("%s %s resolves to %s, outside the allowed model roots %v; "+
		"add its directory to the agent's --allowed-model-roots to allow it", e.What, e.Path, e.Resolved, e.Roots)
}

// CheckPath resolves p and returns a *PathError if it cannot be resolved or
// resolves outside the roots.
func (r Roots) CheckPath(what, p, workDir, home string) error {
	resolved, err := ResolvePath(p, workDir, home)
	if err != nil {
		return &PathError{What: what, Path: p, Err: err, Roots: r.Dirs()}
	}
	if !r.Contains(resolved) {
		return &PathError{What: what, Path: p, Resolved: resolved, Roots: r.Dirs()}
	}
	return nil
}
