/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package archive

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/audit"
)

func testRecord() audit.Record {
	return audit.Record{
		SchemaVersion: "foreman.audit.v1",
		RecordedAt:    "2026-08-23T18:44:22Z",
		Repo:          "defilantech/LLMKube",
		Issue:         1602,
		Verdict:       "GO",
		Task:          audit.TaskRef{Name: "wl-1602-code", Kind: "issue-fix"},
	}
}

func TestBundleDir_LayoutAndTimestampSanitising(t *testing.T) {
	got, err := BundleDir("/arch", testRecord())
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	want := "/arch/defilantech/LLMKube/1602/wl-1602-code-2026-08-23T18-44-22Z"
	if got != want {
		t.Errorf("BundleDir = %q, want %q", got, want)
	}
}

func TestBundleDir_ZeroIssueGetsAWellFormedKey(t *testing.T) {
	rec := testRecord()
	rec.Issue = 0
	got, err := BundleDir("/arch", rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if !strings.Contains(got, "/no-issue/") {
		t.Errorf("BundleDir = %q, want a no-issue segment", got)
	}
}

func TestBundleDir_RefusesToEscapeTheRoot(t *testing.T) {
	cases := map[string]string{
		"traversing repo": "../../etc",
		"absolute repo":   "/etc/passwd",
		"dot-dot segment": "a/../../b",
	}
	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			rec := testRecord()
			rec.Repo = repo
			if _, err := BundleDir("/arch", rec); err == nil {
				t.Errorf("BundleDir(repo=%q) = nil error, want a refusal", repo)
			}
		})
	}
}

func TestWriteBundle_WritesAuditTranscriptAndMeta(t *testing.T) {
	root := t.TempDir()
	if err := WriteBundle(root, testRecord(), []byte(`{"truncated":true}`)); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	dir, err := BundleDir(root, testRecord())
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}

	var rec audit.Record
	readJSON(t, filepath.Join(dir, "audit.json"), &rec)
	if rec.Task.Name != "wl-1602-code" || rec.Verdict != "GO" {
		t.Errorf("audit.json round-trip = %+v, want the record we wrote", rec)
	}

	tr, err := os.ReadFile(filepath.Join(dir, "transcript.json"))
	if err != nil {
		t.Fatalf("read transcript.json: %v", err)
	}
	if string(tr) != `{"truncated":true}` {
		t.Errorf("transcript.json = %q, want the bytes verbatim including the truncated flag", tr)
	}

	var meta BundleMeta
	readJSON(t, filepath.Join(dir, "meta.json"), &meta)
	if meta.SchemaVersion != BundleSchemaVersion || !meta.HasTranscript {
		t.Errorf("meta.json = %+v, want schema %q and hasTranscript true", meta, BundleSchemaVersion)
	}
}

func TestWriteBundle_NoTranscriptStillArchivesTheRecord(t *testing.T) {
	root := t.TempDir()
	if err := WriteBundle(root, testRecord(), nil); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	dir, _ := BundleDir(root, testRecord())
	if _, err := os.Stat(filepath.Join(dir, "audit.json")); err != nil {
		t.Errorf("audit.json missing for a transcript-less run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "transcript.json")); !os.IsNotExist(err) {
		t.Errorf("transcript.json exists for a run that had none (err=%v)", err)
	}
	var meta BundleMeta
	readJSON(t, filepath.Join(dir, "meta.json"), &meta)
	if meta.HasTranscript {
		t.Error("meta.hasTranscript = true for a run with no transcript")
	}
}

func TestWriteBundle_ExistingBundleIsNotRewritten(t *testing.T) {
	root := t.TempDir()
	if err := WriteBundle(root, testRecord(), []byte(`original`)); err != nil {
		t.Fatalf("first WriteBundle: %v", err)
	}
	if err := WriteBundle(root, testRecord(), []byte(`REPLACED`)); err != nil {
		t.Fatalf("second WriteBundle: %v", err)
	}
	dir, _ := BundleDir(root, testRecord())
	tr, err := os.ReadFile(filepath.Join(dir, "transcript.json"))
	if err != nil {
		t.Fatalf("read transcript.json: %v", err)
	}
	if string(tr) != "original" {
		t.Errorf("transcript.json = %q, want the first write preserved; bundles are immutable", tr)
	}
}

func TestWriteBundle_PartialWriteLeavesNoBundleSoItRetries(t *testing.T) {
	root := t.TempDir()
	rec := testRecord()
	// json.Marshal refuses a non-finite float, so the bundle directory is
	// created and then audit.json fails to write. That is the only path that
	// exercises the cleanup, and without it a half-written bundle would look
	// complete to the skip check and never be retried.
	rec.ElapsedSec = math.Inf(1)

	if err := WriteBundle(root, rec, nil); err == nil {
		t.Fatal("WriteBundle with a non-finite ElapsedSec = nil error, want a marshal failure")
	}
	dir, err := BundleDir(root, rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a failed write left a bundle directory behind, which would suppress the retry")
	}
}

func TestWriteBundle_UnwritableRootFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "read-only")
	if err := os.Mkdir(root, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := WriteBundle(root, testRecord(), nil); err == nil {
		t.Fatal("WriteBundle into an unwritable root = nil error, want a failure")
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}
