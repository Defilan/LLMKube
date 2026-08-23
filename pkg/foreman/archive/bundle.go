/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

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
// The layout is <root>/<repo>/<issue>/<taskName>-<recordedAt>. Repo keeps its
// slash, so an owner/name slug occupies two levels and a prefix listing for one
// repo works naturally. Colons are stripped from the timestamp because they are
// awkward in paths on some tools and filesystems.
//
// Every segment is validated against path traversal. Record fields come from a
// cluster object and an operator could set Repo to anything; a bundle must
// never land outside root. This is the same class of defect as issue #1625,
// where an unvalidated repo slug silently changed which base a branch was cut
// from.
func BundleDir(root string, rec audit.Record) (string, error) {
	if filepath.IsAbs(rec.Repo) {
		return "", fmt.Errorf("archive: repo %q must be relative", rec.Repo)
	}
	issue := "no-issue"
	if rec.Issue != 0 {
		issue = fmt.Sprintf("%d", rec.Issue)
	}
	stamp := strings.ReplaceAll(rec.RecordedAt, ":", "-")
	rel := filepath.Join(rec.Repo, issue, rec.Task.Name+"-"+stamp)

	dir := filepath.Join(root, rel)
	// filepath.Join cleans, so a traversing segment shows up as a path that no
	// longer sits under root. Compare with a trailing separator so /arch-evil
	// does not pass a prefix test against /arch.
	if !strings.HasPrefix(dir, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive: bundle path %q escapes root %q", dir, root)
	}
	return dir, nil
}

// WriteBundle writes audit.json, meta.json, and transcript.json (when
// transcript is non-empty) into the record's bundle directory.
//
// An existing bundle is left untouched: bundles are immutable, and a retried
// task writes a different bundle because its RecordedAt differs. A failed write
// removes the partial directory so the next reconcile retries rather than
// finding a half-written bundle and skipping it.
func WriteBundle(root string, rec audit.Record, transcript []byte) error {
	dir, err := BundleDir(root, rec)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		return nil
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
