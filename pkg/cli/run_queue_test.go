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

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseQueue(t *testing.T) {
	data := []byte(`
repo: defilantech/LLMKube
items:
  - issue: 1602
    intent: ./intents/1602.md
  - issue: 1601
    intent: ./intents/1601.md
    repo: defilantech/other
`)
	q, err := ParseQueue(data)
	if err != nil {
		t.Fatalf("ParseQueue: %v", err)
	}
	if q.Repo != "defilantech/LLMKube" {
		t.Errorf("Repo = %q, want defilantech/LLMKube", q.Repo)
	}
	if len(q.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(q.Items))
	}
	if q.Items[0].Issue != 1602 {
		t.Errorf("Items[0].Issue = %d, want 1602", q.Items[0].Issue)
	}
	// Per-item repo overrides the queue default; unset inherits it.
	if q.Items[0].Repo != "defilantech/LLMKube" {
		t.Errorf("Items[0].Repo = %q, want the queue default", q.Items[0].Repo)
	}
	if q.Items[1].Repo != "defilantech/other" {
		t.Errorf("Items[1].Repo = %q, want the per-item override", q.Items[1].Repo)
	}
}

func TestParseQueue_RejectsMissingIssue(t *testing.T) {
	if _, err := ParseQueue([]byte("repo: a/b\nitems:\n  - intent: ./x.md\n")); err == nil {
		t.Fatal("want error for an item with no issue")
	}
}

func TestParseQueue_RejectsMissingRepo(t *testing.T) {
	// #1625: an empty repo slug silently bases a branch on a stale fork HEAD.
	// Refuse it here rather than dispatching work that will be cut wrong.
	if _, err := ParseQueue([]byte("items:\n  - issue: 1\n    intent: ./x.md\n")); err == nil {
		t.Fatal("want error when neither queue nor item names a repo")
	}
}

func TestIntentFor_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "1602.md")
	if err := os.WriteFile(p, []byte("do the thing"), 0o600); err != nil {
		t.Fatal(err)
	}
	q := Queue{Repo: "a/b"}
	got, err := q.IntentFor(QueueItem{Issue: 1602, IntentPath: p, Repo: "a/b"})
	if err != nil {
		t.Fatalf("IntentFor: %v", err)
	}
	if got != "do the thing" {
		t.Errorf("IntentFor = %q, want %q", got, "do the thing")
	}
}

func TestIntentFor_MissingFileIsAnError(t *testing.T) {
	q := Queue{Repo: "a/b"}
	if _, err := q.IntentFor(QueueItem{Issue: 1, IntentPath: "/nope/missing.md", Repo: "a/b"}); err == nil {
		t.Fatal("want error for a missing intent file")
	}
}
