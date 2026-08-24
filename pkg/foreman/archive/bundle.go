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

// Package archive writes durable task execution records as immutable on-disk
// bundles for compliance and debugging. Each bundle is a directory containing
// audit.json (the Record), transcript.json (optional), and meta.json.
package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/audit"
)

// BundleSchemaVersion identifies the on-disk bundle layout. Bump it only for
// a change a reader cannot handle transparently.
const BundleSchemaVersion = "foreman.archive.v1"

// BundleMeta is meta.json. It carries only what the writer can know without
// reaching outside its arguments, so the writer stays a pure function.
type BundleMeta struct {
	SchemaVersion string `json:"schemaVersion"`
	TaskName      string `json:"taskName"`
	RecordedAt    string `json:"recordedAt"`
	HasTranscript bool   `json:"hasTranscript"`
}

// BundleDir returns the directory a record's bundle belongs in.
//
// The layout is <root>/<repo>/<issue>/<taskName>-<recordedAt> (or
// <taskName>-<taskUID> if RecordedAt is empty). Repo keeps its slash, so an
// owner/name slug occupies two levels and a prefix listing for one repo works
// naturally; an empty repo is normalized to "no-repo". Colons are stripped
// from the timestamp because they are awkward in paths on some tools and
// filesystems.
//
// Every segment is validated against path traversal and symlink escape.
// Record fields come from a cluster object and an operator could set Repo to
// anything; a bundle must never land outside root. This is the same class of
// defect as issue #1625, where an unvalidated repo slug silently changed which
// base a branch was cut from.
func BundleDir(root string, rec audit.Record) (string, error) {
	if err := validateKey(rec); err != nil {
		return "", err
	}

	if filepath.IsAbs(rec.Repo) {
		return "", fmt.Errorf("archive: repo %q must be relative", rec.Repo)
	}

	issue := "no-issue"
	if rec.Issue != 0 {
		issue = fmt.Sprintf("%d", rec.Issue)
	}

	repo := rec.Repo
	if repo == "" || repo == "." {
		repo = "no-repo"
	}

	timestamp := rec.RecordedAt
	if timestamp == "" {
		timestamp = rec.Task.UID
	}
	stamp := strings.ReplaceAll(timestamp, ":", "-")

	rel := filepath.Join(repo, issue, rec.Task.Name+"-"+stamp)

	// Resolve root to its canonical form. Use Abs which doesn't require the
	// path to exist, then EvalSymlinks if it does to catch symlink escapes.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("archive: abs root: %w", err)
	}

	resolvedRoot := absRoot
	if _, err := os.Stat(absRoot); err == nil {
		// Root exists, resolve symlinks.
		resolvedRoot, err = filepath.EvalSymlinks(absRoot)
		if err != nil {
			return "", fmt.Errorf("archive: resolve root symlinks: %w", err)
		}
	} else if !os.IsNotExist(err) {
		// Stat failed for a reason other than not-exist.
		return "", fmt.Errorf("archive: stat root: %w", err)
	}

	// Check containment using filepath.Rel instead of string prefix.
	dir := filepath.Join(resolvedRoot, rel)
	relPath, err := filepath.Rel(resolvedRoot, dir)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive: bundle path %q escapes root %q", dir, resolvedRoot)
	}

	return dir, nil
}

// WriteBundle writes audit.json, meta.json, and transcript.json (when
// transcript is non-empty) into the record's bundle directory.
//
// An existing bundle is left untouched: bundles are immutable, and a retried
// task writes a different bundle because its RecordedAt or UID differs. A
// failed write removes the partial directory so the next reconcile retries
// rather than finding a half-written bundle and skipping it. A partial
// MkdirAll failure leaves orphaned directories, but the leaf is never created
// and the retry still succeeds.
func WriteBundle(root string, rec audit.Record, transcript []byte) error {
	dir, err := BundleDir(root, rec)
	if err != nil {
		return err
	}

	// Check if the bundle directory already exists and is a directory.
	fi, err := os.Stat(dir)
	if err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("archive: bundle path %q is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("archive: stat bundle dir: %w", err)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("archive: create bundle dir: %w", err)
	}

	if err := writeAll(dir, rec, transcript); err != nil {
		// Leave nothing behind: a partial bundle would look complete to the
		// skip check above and the run would never be archived.
		_ = os.RemoveAll(dir)
		return err
	}

	// Post-create verification: ensure the directory we wrote to is still under root.
	finalResolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// Directory was created but verification failed; clean up.
		_ = os.RemoveAll(dir)
		return fmt.Errorf("archive: verify bundle containment: %w", err)
	}

	// Re-check containment of the final resolved path.
	absRoot, _ := filepath.Abs(root)
	resolvedRoot := absRoot
	if _, err := os.Stat(absRoot); err == nil {
		resolvedRoot, _ = filepath.EvalSymlinks(absRoot)
	}

	relPath, _ := filepath.Rel(resolvedRoot, finalResolved)
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		// A symlink escape occurred post-create; clean up.
		_ = os.RemoveAll(dir)
		return fmt.Errorf("archive: bundle escaped root after creation at %q", finalResolved)
	}

	return nil
}

// validateKey checks for impossible or unsafe values in the record fields
// that form the bundle path key.
func validateKey(rec audit.Record) error {
	if strings.ContainsRune(rec.Repo, '\x00') {
		return fmt.Errorf("archive: repo contains NUL byte")
	}
	if strings.ContainsRune(rec.Task.Name, '\x00') {
		return fmt.Errorf("archive: task name contains NUL byte")
	}
	if strings.ContainsRune(rec.RecordedAt, '\x00') {
		return fmt.Errorf("archive: recordedAt contains NUL byte")
	}
	if rec.RecordedAt == "" && rec.Task.UID == "" {
		return fmt.Errorf("archive: neither recordedAt nor task UID provided; no stable key")
	}
	return nil
}

func writeAll(dir string, rec audit.Record, transcript []byte) error {
	if err := writeJSON(filepath.Join(dir, "audit.json"), rec); err != nil {
		return err
	}
	if len(transcript) > 0 {
		p := filepath.Join(dir, "transcript.json")
		if err := os.WriteFile(p, transcript, 0o640); err != nil {
			return fmt.Errorf("archive: write %s: %w", p, err)
		}
	}
	return writeJSON(filepath.Join(dir, "meta.json"), BundleMeta{
		SchemaVersion: BundleSchemaVersion,
		TaskName:      rec.Task.Name,
		RecordedAt:    rec.RecordedAt,
		HasTranscript: len(transcript) > 0,
	})
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("archive: marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, b, 0o640); err != nil {
		return fmt.Errorf("archive: write %s: %w", path, err)
	}
	return nil
}
