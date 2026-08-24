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
		Task:          audit.TaskRef{Name: "wl-1602-code", Kind: "issue-fix", UID: "task-123"},
	}
}

func TestBundleDir_LayoutAndTimestampSanitising(t *testing.T) {
	got, err := BundleDir("/arch", testRecord())
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if !strings.HasSuffix(got, "defilantech/LLMKube/1602/wl-1602-code-2026-08-23T18-44-22Z") {
		t.Errorf("BundleDir = %q, want suffix %q", got, "defilantech/LLMKube/1602/wl-1602-code-2026-08-23T18-44-22Z")
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

func TestBundleDir_EmptyRepoNormalizesToNoRepo(t *testing.T) {
	rec := testRecord()
	rec.Repo = ""
	got, err := BundleDir("/arch", rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if !strings.Contains(got, "/no-repo/") {
		t.Errorf("BundleDir with empty repo = %q, want /no-repo/ segment", got)
	}
}

func TestBundleDir_DotRepoNormalizesToNoRepo(t *testing.T) {
	rec := testRecord()
	rec.Repo = "."
	got, err := BundleDir("/arch", rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if !strings.Contains(got, "/no-repo/") {
		t.Errorf("BundleDir with dot repo = %q, want /no-repo/ segment", got)
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

func TestBundleDir_EmptyRecordedAtFallsBackToUID(t *testing.T) {
	rec := testRecord()
	rec.RecordedAt = ""
	rec.Task.UID = "stable-uid-123"
	got, err := BundleDir("/arch", rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if !strings.Contains(got, "stable-uid-123") {
		t.Errorf("BundleDir with empty RecordedAt = %q, want to contain UID", got)
	}
}

func TestBundleDir_RejectsEmptyRecordedAtAndUID(t *testing.T) {
	rec := testRecord()
	rec.RecordedAt = ""
	rec.Task.UID = ""
	if _, err := BundleDir("/arch", rec); err == nil {
		t.Fatal("BundleDir with no RecordedAt or UID = nil error, want a rejection")
	}
}

func TestBundleDir_RejectsNULInRepo(t *testing.T) {
	rec := testRecord()
	rec.Repo = "foo\x00bar"
	if _, err := BundleDir("/arch", rec); err == nil {
		t.Fatal("BundleDir with NUL in repo = nil error, want a rejection")
	}
}

func TestBundleDir_RejectsNULInTaskName(t *testing.T) {
	rec := testRecord()
	rec.Task.Name = "task\x00name"
	if _, err := BundleDir("/arch", rec); err == nil {
		t.Fatal("BundleDir with NUL in task name = nil error, want a rejection")
	}
}

func TestBundleDir_RejectsNULInRecordedAt(t *testing.T) {
	rec := testRecord()
	rec.RecordedAt = "2026-08-23T18:44:22\x00Z"
	if _, err := BundleDir("/arch", rec); err == nil {
		t.Fatal("BundleDir with NUL in recordedAt = nil error, want a rejection")
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
	if meta.SchemaVersion != "foreman.archive.v1" {
		t.Errorf("meta.schemaVersion = %q, want literal \"foreman.archive.v1\"", meta.SchemaVersion)
	}
	if !meta.HasTranscript {
		t.Error("meta.hasTranscript = false, want true")
	}
	if meta.TaskName != "wl-1602-code" {
		t.Errorf("meta.taskName = %q, want wl-1602-code", meta.TaskName)
	}
	if meta.RecordedAt != "2026-08-23T18:44:22Z" {
		t.Errorf("meta.recordedAt = %q, want 2026-08-23T18:44:22Z", meta.RecordedAt)
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

func TestWriteBundle_StrayFileRefusesBundle(t *testing.T) {
	root := t.TempDir()
	rec := testRecord()
	dir, _ := BundleDir(root, rec)
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dir, []byte("stray"), 0o640); err != nil {
		t.Fatalf("create stray file: %v", err)
	}

	if err := WriteBundle(root, rec, nil); err == nil {
		t.Fatal("WriteBundle with stray regular file at path = nil error, want error")
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

func TestWriteBundle_FileModes(t *testing.T) {
	root := t.TempDir()
	if err := WriteBundle(root, testRecord(), []byte("test")); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	dir, _ := BundleDir(root, testRecord())

	// Check directory mode.
	dirInfo, _ := os.Stat(dir)
	dirMode := dirInfo.Mode().Perm()
	if dirMode != 0o750 {
		t.Errorf("bundle dir mode = %03o, want 0750", dirMode)
	}

	// Check file modes.
	for _, name := range []string{"audit.json", "transcript.json", "meta.json"} {
		path := filepath.Join(dir, name)
		fi, err := os.Stat(path)
		if err != nil {
			continue // transcript might not exist for some tests
		}
		fileMode := fi.Mode().Perm()
		if fileMode != 0o640 {
			t.Errorf("%s mode = %03o, want 0640", name, fileMode)
		}
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

func TestWriteBundle_RefusesASymlinkedSegment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// The bundle path starts <root>/defilantech/LLMKube/... , so a symlink at
	// the first segment sends MkdirAll outside the archive root entirely.
	if err := os.Symlink(outside, filepath.Join(root, "defilantech")); err != nil {
		t.Skipf("symlink creation failed: %v (platform limitation)", err)
	}

	if err := WriteBundle(root, testRecord(), []byte(`{"x":1}`)); err == nil {
		t.Fatal("WriteBundle through a symlinked segment = nil error, want a refusal")
	}

	var leaked []string
	_ = filepath.WalkDir(outside, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			leaked = append(leaked, p)
		}
		return nil
	})
	if len(leaked) != 0 {
		t.Errorf("files written outside the archive root: %v", leaked)
	}
}
