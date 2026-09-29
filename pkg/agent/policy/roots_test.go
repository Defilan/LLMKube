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

package policy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mkfile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNewRoots(t *testing.T) {
	store := t.TempDir()
	missing := filepath.Join(store, "nope")
	roots, ignored, err := NewRoots([]string{store, missing, ""}, "")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(store)
	if len(roots.Dirs()) != 1 || roots.Dirs()[0] != resolved {
		t.Errorf("Dirs = %v, want [%s] (resolved)", roots.Dirs(), resolved)
	}
	if len(ignored) != 1 || ignored[0] != missing {
		t.Errorf("ignored = %v, want [%s]", ignored, missing)
	}
	if _, _, err := NewRoots([]string{"relative/dir"}, ""); err == nil {
		t.Error("relative root accepted; want an error")
	}
}

// A "~" or "~/..." root is expanded against home, exactly like a checked
// path. "~user" and a bare "~"/"~/..." with home == "" fail closed rather
// than resolving relative to the working directory.
func TestNewRoots_Tilde(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	sub := filepath.Join(home, "llmkube-models")
	mkfile(t, filepath.Join(sub, "m.gguf"))
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("tilde slash expands under home", func(t *testing.T) {
		roots, _, err := NewRoots([]string{"~/llmkube-models"}, home)
		if err != nil {
			t.Fatalf("NewRoots(~/llmkube-models) = %v, want no error", err)
		}
		want := filepath.Join(resolvedHome, "llmkube-models")
		if got := roots.Dirs(); len(got) != 1 || got[0] != want {
			t.Errorf("Dirs = %v, want [%s]", got, want)
		}
	})

	t.Run("bare tilde equals home", func(t *testing.T) {
		roots, _, err := NewRoots([]string{"~"}, home)
		if err != nil {
			t.Fatalf("NewRoots(~) = %v, want no error", err)
		}
		if got := roots.Dirs(); len(got) != 1 || got[0] != resolvedHome {
			t.Errorf("Dirs = %v, want [%s]", got, resolvedHome)
		}
	})

	t.Run("tilde user errors", func(t *testing.T) {
		if _, _, err := NewRoots([]string{"~someoneelse/x"}, home); err == nil {
			t.Error("NewRoots(~someoneelse/x) succeeded, want an error")
		}
	})

	t.Run("tilde slash with unknown home errors", func(t *testing.T) {
		if _, _, err := NewRoots([]string{"~/x"}, ""); err == nil {
			t.Error("NewRoots(~/x, home=\"\") succeeded, want an error")
		}
	})

	t.Run("bare tilde with unknown home errors", func(t *testing.T) {
		if _, _, err := NewRoots([]string{"~"}, ""); err == nil {
			t.Error("NewRoots(~, home=\"\") succeeded, want an error")
		}
	})
}

func TestCheckPath(t *testing.T) {
	// Resolve the temp dir itself up front: on macOS t.TempDir() lives under
	// /var, which is a symlink to /private/var. Building every fixture path
	// from an already-resolved base keeps bite checks honest: disabling a
	// specific guard below should fail only the subtest that guard protects,
	// not every subtest, via an unrelated /var-vs-/private/var alias.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "store")
	outside := filepath.Join(base, "outside")
	mkfile(t, filepath.Join(store, "m.gguf"))
	mkfile(t, filepath.Join(outside, "secret"))
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(store, "link-out")); err != nil {
		t.Fatal(err)
	}
	// A store entry that is a symlink to another allowed root (the HF-cache case).
	hf := filepath.Join(base, "hf")
	mkfile(t, filepath.Join(hf, "snap", "model.safetensors"))
	if err := os.Symlink(filepath.Join(hf, "snap"), filepath.Join(store, "linked-model")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	mkfile(t, filepath.Join(home, "notes"))

	// A symlinked directory, entirely outside the store, whose target exists
	// but whose child does not yet. Exercises the walk-up-to-a-symlink path
	// with no dangling target involved.
	if err := os.Symlink(outside, filepath.Join(store, "link-out-dir")); err != nil {
		t.Fatal(err)
	}

	// A symlink whose target does not exist (dangling).
	if err := os.Symlink(filepath.Join(outside, "nope"), filepath.Join(store, "dangle")); err != nil {
		t.Fatal(err)
	}

	// A symlink to a real directory, used to build a "<symlink>/../escape"
	// path via string concatenation (filepath.Join would collapse the ".."
	// before it ever reached ResolvePath).
	deep := filepath.Join(outside, "deep")
	mkfile(t, filepath.Join(deep, "placeholder"))
	if err := os.Symlink(deep, filepath.Join(store, "L")); err != nil {
		t.Fatal(err)
	}

	roots, _, err := NewRoots([]string{store, hf}, home)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		path  string
		allow bool
	}{
		{"inside", filepath.Join(store, "m.gguf"), true},
		{"root itself", store, true},
		{"outside", filepath.Join(outside, "secret"), false},
		{"dotdot escape", filepath.Join(store, "..", "outside", "secret"), false},
		{"symlink out of store", filepath.Join(store, "link-out"), false},
		{"symlink into another root", filepath.Join(store, "linked-model"), true},
		{"not yet existing under store", filepath.Join(store, "new", "file.bin"), true},
		{"not yet existing outside", filepath.Join(outside, "new", "file.bin"), false},
		{"tilde to home", "~/notes", false},
		{"relative resolves in workdir", "m.gguf", true},
		{"dot relative", "./m.gguf", true},
		{"dotdot relative escapes", "../outside/secret", false},
		{"file scheme", "file://" + filepath.Join(store, "m.gguf"), true},
		{"FILE scheme outside", "FILE://" + filepath.Join(outside, "secret"), false},
		// New path under a directory reached only through a symlink that
		// itself points outside every root.
		{"new path under symlinked dir pointing outside", filepath.Join(store, "link-out-dir", "new", "file"), false},
		// A dangling symlink, and a path under one, must fail closed rather
		// than be treated as "not yet existing under store".
		{"dangling symlink", filepath.Join(store, "dangle"), false},
		{"dangling symlink child", filepath.Join(store, "dangle", "x"), false},
		// ".." after a symlink component: built by string concatenation so
		// the ".." survives to ResolvePath instead of being collapsed by
		// filepath.Join/Clean before it gets there.
		{"dotdot after symlink", store + "/L/../secret", false},
		// A harmless-looking "sub/../file" with no symlink involved at all:
		// refused anyway, per the documented fail-closed limit on any ".."
		// component.
		{"dotdot no symlink fail closed", store + "/sub/../m.gguf", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := roots.CheckPath("model path", tt.path, store, home)
			if tt.allow && err != nil {
				t.Errorf("CheckPath(%q) = %v, want allowed", tt.path, err)
			}
			if !tt.allow {
				var pe *PathError
				if !errors.As(err, &pe) {
					t.Errorf("CheckPath(%q) = %v, want *PathError", tt.path, err)
				}
			}
		})
	}
}

// A "~"-prefixed path must fail closed when the caller could not determine a
// home directory, rather than silently resolving relative to workDir.
func TestResolvePath_TildeWithUnknownHome(t *testing.T) {
	workDir := t.TempDir()
	for _, p := range []string{"~", "~/x"} {
		if _, err := ResolvePath(p, workDir, ""); err == nil {
			t.Errorf("ResolvePath(%q, _, \"\") succeeded, want an error", p)
		}
	}

	store := filepath.Join(workDir, "store")
	mkfile(t, filepath.Join(store, "m.gguf"))
	roots, _, err := NewRoots([]string{store}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"~", "~/x"} {
		err := roots.CheckPath("model path", p, workDir, "")
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Errorf("CheckPath(%q, home=\"\") = %v, want *PathError", p, err)
		}
	}
}

func TestContainsRootSeparator(t *testing.T) {
	r := Roots{dirs: []string{"/"}}
	if !r.Contains("/etc/hosts") {
		t.Error(`Contains("/etc/hosts") = false, want true for root "/"`)
	}
	if !r.Contains("/") {
		t.Error(`Contains("/") = false, want true for root "/"`)
	}
}
