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

package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// transcriptRefExtra returns the Result.Extra map a real run produces for a
// task that wrote a transcript, by driving the actual producer chain rather
// than by asserting a shape from memory.
//
// This is the whole point of the fixture. Status.TranscriptRef went unassigned
// in production for its entire life, and the reason nothing caught it is that
// the consumer-side tests hand-set the field, manufacturing an input no
// producer supplies. So this helper calls WriteTranscript, which creates the
// ConfigMap and returns the ObjectReference, then objRefAsMap, which is what
// every executor path stamps into Extra["transcriptRef"]. If either end of
// that chain changes shape -- a renamed key, a flattened reference, a
// namespaced name -- these tests fail rather than continuing to pass against
// a fixture that has drifted away from the code.
func transcriptRefExtra(t *testing.T, c client.Client, task *foremanv1alpha1.AgenticTask) (map[string]any, string) {
	t.Helper()
	tref, err := WriteTranscript(context.Background(), c, task, &LoopResult{
		Turns: 2,
		Transcript: []oai.Message{
			{Role: oai.RoleUser, Content: "fix the bug"},
			{Role: oai.RoleAssistant, Content: "fixed it"},
		},
	})
	if err != nil {
		t.Fatalf("WriteTranscript: %v", err)
	}
	if tref.Name == "" {
		t.Fatalf("WriteTranscript returned an unnamed reference; the fixture cannot prove anything")
	}
	return map[string]any{
		"outcome":       "",
		"branch":        "foreman/issue-1654",
		"commitSHA":     "abc1234def5678",
		"transcriptRef": objRefAsMap(tref),
	}, tref.Name
}

// patchTerminal must lift the transcript ConfigMap's name out of the Result
// envelope and onto AgenticTask.status.transcriptRef.
//
// Regression for defilantech/LLMKube#1654. The executor stamps the reference
// into Result.Extra as an ObjectReference map; nothing lifted it, so
// status.transcriptRef was empty on every task ever run. Both readers of the
// field -- audit.BuildRecord and the terminal-task archiver -- take the empty
// value as "this run wrote no transcript", which is exactly what a
// deterministic run looks like. A fleet losing 100% of its transcripts was
// therefore byte-indistinguishable from a fleet of deterministic runs.
//
// The assertion resolves the lifted name through the fake client the way the
// archiver does, because a name that lands on status but does not resolve to
// the ConfigMap is the same data loss one step later.
func TestPatchTerminal_LiftsTranscriptRef(t *testing.T) {
	c := newRecoveryClient(t, pendingTask("code-1654"))
	w := &AgenticTaskWatcher{Client: c, NodeName: "coder", Namespace: "default"}

	extra, wantName := transcriptRefExtra(t, c, pendingTask("code-1654"))
	res := NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictGo,
		"fixed the bug", time.Second)
	res.Extra = extra

	if err := w.patchTerminal(context.Background(), pendingTask("code-1654"), res, nil); err != nil {
		t.Fatalf("patchTerminal: %v", err)
	}

	got := getTask(t, c, "code-1654")
	if got.Status.TranscriptRef != wantName {
		t.Fatalf("status.transcriptRef = %q, want %q; the transcript reference never reached status, "+
			"so every archived bundle records hasTranscript:false", got.Status.TranscriptRef, wantName)
	}

	// The archiver resolves the field as a bare name in the task's own
	// namespace (agentictask_controller.go archiveTerminalTask). Lifting
	// "namespace/name", or the ObjectReference's Kind, would satisfy the
	// comparison above only if the fixture were hand-written; here it would
	// still fail this Get.
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: got.Namespace, Name: got.Status.TranscriptRef}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("status.transcriptRef %q does not resolve to a ConfigMap in %s: %v",
			got.Status.TranscriptRef, got.Namespace, err)
	}
	if cm.Data[transcriptDataKey] == "" {
		t.Errorf("the referenced ConfigMap holds nothing under %q", transcriptDataKey)
	}
}

// The Job-mode executor carries Result.Extra through a JSON hop
// (coderJobResultToResult seeds it from the in-pod envelope, which arrives
// decoded), so the transcriptRef value the watcher sees there is
// json.Unmarshal's map[string]any, not the one objRefAsMap built in this
// process. The two are the same shape, and this pins that: a lift written
// against a concrete corev1.ObjectReference type assertion would pass the
// in-process test above and drop every Job-mode transcript.
func TestPatchTerminal_LiftsTranscriptRefAfterJSONRoundTrip(t *testing.T) {
	c := newRecoveryClient(t, pendingTask("code-1654-job"))
	w := &AgenticTaskWatcher{Client: c, NodeName: "coder", Namespace: "default"}

	extra, wantName := transcriptRefExtra(t, c, pendingTask("code-1654-job"))
	raw, err := json.Marshal(extra)
	if err != nil {
		t.Fatalf("marshal extra: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal extra: %v", err)
	}

	res := NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictGo,
		"fixed in a Job", time.Second)
	res.Extra = decoded

	if err := w.patchTerminal(context.Background(), pendingTask("code-1654-job"), res, nil); err != nil {
		t.Fatalf("patchTerminal: %v", err)
	}

	got := getTask(t, c, "code-1654-job")
	if got.Status.TranscriptRef != wantName {
		t.Fatalf("status.transcriptRef = %q, want %q after a JSON round-trip",
			got.Status.TranscriptRef, wantName)
	}
}

// A deterministic run writes no transcript ConfigMap, so objRefAsMap returns
// nil and the key is absent. status.transcriptRef must stay empty rather than
// naming a ConfigMap that does not exist, which the archiver would count as a
// transcript_read failure on every deterministic task in the fleet.
//
// The malformed cases are here because Extra is a map[string]any that survives
// a JSON hop: a producer-side change that flattens the reference to a string,
// or emits an explicit null, must leave the field empty rather than panic on a
// status-write path. A panic there takes down the watcher goroutine and the
// task never reaches a terminal phase at all.
func TestPatchTerminal_TranscriptRefEmptyWhenAbsentOrMalformed(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
	}{
		{"deterministic run: no key at all", map[string]any{"outcome": ""}},
		{"explicit nil, as objRefAsMap returns for an unnamed ref", map[string]any{"transcriptRef": nil}},
		{"flattened to a scalar", map[string]any{"transcriptRef": "foreman-transcript-code-x"}},
		{"map with no name field", map[string]any{"transcriptRef": map[string]any{"kind": "ConfigMap"}}},
		{"name is not a string", map[string]any{"transcriptRef": map[string]any{"name": 42}}},
		{"nil Extra entirely", nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := taskNameForCase(i)
			c := newRecoveryClient(t, pendingTask(name))
			w := &AgenticTaskWatcher{Client: c, NodeName: "coder", Namespace: "default"}

			res := NewResult("gate", foremanv1alpha1.AgenticTaskVerdictGo,
				"deterministic", time.Second)
			res.Extra = tc.extra

			if err := w.patchTerminal(context.Background(), pendingTask(name), res, nil); err != nil {
				t.Fatalf("patchTerminal: %v", err)
			}
			if got := getTask(t, c, name).Status.TranscriptRef; got != "" {
				t.Errorf("status.transcriptRef = %q, want empty", got)
			}
		})
	}
}

// taskNameForCase gives each subtest its own task so the fake clients cannot
// share state.
func taskNameForCase(i int) string {
	return "det-" + string(rune('a'+i))
}
