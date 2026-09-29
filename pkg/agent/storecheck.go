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

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// CheckModelStore refuses a model store another local user could tamper with.
// The store holds the model files engines load and the per-process engine
// logs, and it is always an allowed model root, so a store someone else owns
// or can write lets them swap a model, plant a symlink the agent then writes
// through, or widen what the root policy admits.
//
// path is resolved through symlinks first and the target is judged: a link
// the agent owns pointing at a shared directory is still a shared store. The
// resolved directory must exist, be owned by the agent's uid, and have no
// group or other write bit. The error names the path, the owner uid, the mode
// and the fix.
func CheckModelStore(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("model store %s: %w", path, err)
	}
	where := describeStore(path, resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("model store %s: %w", where, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("model store %s is not a directory", where)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("model store %s: cannot read its owner", where)
	}
	uid := os.Getuid()
	owner := int(st.Uid)
	mode := info.Mode().Perm()
	if owner != uid {
		return fmt.Errorf("model store %s has owner uid %d and mode %04o, but the agent runs as uid %d "+
			"and refuses a store another user owns: run `chown %d %s` or pass a --model-store the agent owns",
			where, owner, mode, uid, uid, resolved)
	}
	if mode&0o022 != 0 {
		return fmt.Errorf("model store %s has owner uid %d and mode %04o, which lets other users write to it: "+
			"run `chmod go-w %s` or pass a private --model-store",
			where, owner, mode, resolved)
	}
	return nil
}

// describeStore names the configured path and, when it differs, the
// directory it resolved to, so an error about a symlinked store points at
// the directory that actually needs fixing.
func describeStore(path, resolved string) string {
	if resolved == path {
		return path
	}
	return fmt.Sprintf("%s (resolves to %s)", path, resolved)
}

// createNoFollow creates or truncates path for writing with mode 0600 and
// refuses to follow a symlink at path. Files the agent creates inside the
// model store go through it (or appendNoFollow): a symlink planted there
// would otherwise make the agent truncate and write whatever it points at.
func createNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
}

// appendNoFollow opens an existing path for appending and refuses to follow
// a symlink at path.
func appendNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
}

// openEngineLog opens an engine's per-process log (truncated, one run per
// file) without following a symlink. A symlinked log path fails the start
// with an error naming the path rather than writing through the link.
func openEngineLog(engine, logPath string) (*os.File, error) {
	f, err := createNoFollow(logPath)
	if err == nil {
		return f, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("refusing to open %s log file %s: it is a symlink (remove it and retry): %w",
			engine, logPath, err)
	}
	return nil, fmt.Errorf("failed to open %s log file %s: %w", engine, logPath, err)
}
