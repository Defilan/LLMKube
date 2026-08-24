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

package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
	"github.com/defilantech/llmkube/pkg/foreman/archive"
	"github.com/defilantech/llmkube/pkg/foreman/audit"
)

// TestArchiveTerminalTask_DisabledWritesNothing pins the opt-in: an empty
// ArchiveDir must do nothing at all, not "write somewhere harmless".
//
// The assertion runs against the process working directory because that is
// where a missing guard would actually put the bundle: archive.BundleDir
// resolves its root with filepath.Abs, and filepath.Abs("") is the working
// directory. Asserting on a temp dir that was never handed to the reconciler
// would pass whether or not the guard exists. t.Chdir therefore does double
// duty here, giving the test something real to observe and keeping a
// regression from scattering bundles through the source tree.
func TestArchiveTerminalTask_DisabledWritesNothing(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	task := terminalTask()
	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task), ArchiveDir: ""}
	r.archiveTerminalTask(context.Background(), task, logr.Discard())

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("disabled archiver created %d entries, want 0", len(entries))
	}
}

// TestArchiveTerminalTask_WritesABundle is the happy path: a terminal task
// with archival enabled lands one bundle at the record's key.
//
// The repo/issue path check is a fixture guard. audit.BuildRecord reads
// Spec.Payload.Repo and Spec.Payload.Issue, so a fixture that set those
// anywhere else would archive every task under "no-repo/no-issue" and the
// layout would be asserted against a path production never emits.
func TestArchiveTerminalTask_WritesABundle(t *testing.T) {
	root := t.TempDir()
	task := terminalTask()
	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task), ArchiveDir: root}
	r.archiveTerminalTask(context.Background(), task, logr.Discard())

	rec := audit.BuildRecord(task, nil)
	dir, err := archive.BundleDir(root, rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.json")); err != nil {
		t.Errorf("audit.json not written: %v", err)
	}
	if want := filepath.Join("defilantech", "LLMKube", "1602"); !strings.Contains(dir, want) {
		t.Errorf("bundle dir %q does not contain %q; the task's repo and issue did not reach the record", dir, want)
	}
}

// TestArchiveTerminalTask_WritesTranscriptFromConfigMap pins the ConfigMap
// contract: Status.TranscriptRef names a ConfigMap in the task's namespace and
// the transcript bytes live under the "transcript.json" data key. Reading any
// other key yields an empty transcript, which WriteBundle silently declines to
// write, so the loss would be invisible without this test.
func TestArchiveTerminalTask_WritesTranscriptFromConfigMap(t *testing.T) {
	root := t.TempDir()
	task := terminalTask()
	task.Status.TranscriptRef = "foreman-transcript-archive-me"
	want := `{"turns":[{"role":"user","content":"fix it"}]}`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: task.Status.TranscriptRef, Namespace: task.Namespace},
		Data:       map[string]string{"transcript.json": want},
	}
	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task, cm), ArchiveDir: root}
	r.archiveTerminalTask(context.Background(), task, logr.Discard())

	rec := audit.BuildRecord(task, nil)
	dir, err := archive.BundleDir(root, rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "transcript.json"))
	if err != nil {
		t.Fatalf("transcript.json not written: %v", err)
	}
	if string(got) != want {
		t.Errorf("transcript.json = %q, want %q", string(got), want)
	}
}

// TestArchiveTerminalTask_MissingTranscriptStillArchives: a deterministic run
// writes no transcript and a transcript ConfigMap can be reaped before this
// runs. Neither is a reason to drop the compliance record, and both are
// counted so a broken transcript path is visible rather than silent.
func TestArchiveTerminalTask_MissingTranscriptStillArchives(t *testing.T) {
	root := t.TempDir()
	task := terminalTask()
	task.Status.TranscriptRef = "cm-that-does-not-exist"
	before := archiveFailureCount(t, "transcript_read")

	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task), ArchiveDir: root}
	r.archiveTerminalTask(context.Background(), task, logr.Discard())

	rec := audit.BuildRecord(task, nil)
	dir, err := archive.BundleDir(root, rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.json")); err != nil {
		t.Errorf("a task whose transcript is gone must still archive its record: %v", err)
	}
	if got := archiveFailureCount(t, "transcript_read"); got <= before {
		t.Errorf("foreman_archive_failures_total{reason=\"transcript_read\"} = %v, want an increment after an unreadable transcript", got)
	}
}

// TestArchiveTerminalTask_FailureDoesNotPanicAndCounts: archival is a side
// effect and must never change a verdict, so a write failure returns quietly.
// The counter is then the only signal that a full or misconfigured volume is
// dropping records, which makes counting the failure load-bearing rather than
// decorative.
func TestArchiveTerminalTask_FailureDoesNotPanicAndCounts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(root, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	before := archiveFailureCount(t, "write")
	task := terminalTask()
	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task), ArchiveDir: root}
	r.archiveTerminalTask(context.Background(), task, logr.Discard())

	if got := archiveFailureCount(t, "write"); got <= before {
		t.Errorf("foreman_archive_failures_total = %v, want an increment after a failed write", got)
	}
}

// TestArchiveTerminalTask_SecondCallDoesNotDuplicate: the archive call is
// deliberately not gated on firstTerminal, so it runs on every terminal
// reconcile. That is only safe because the bundle key is derived from the
// record (FinishedAt, or the task UID) rather than from wall-clock time, so a
// repeat call resolves to the same directory and WriteBundle skips it.
func TestArchiveTerminalTask_SecondCallDoesNotDuplicate(t *testing.T) {
	root := t.TempDir()
	task := terminalTask()
	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task), ArchiveDir: root}
	r.archiveTerminalTask(context.Background(), task, logr.Discard())
	r.archiveTerminalTask(context.Background(), task, logr.Discard())

	rec := audit.BuildRecord(task, nil)
	dir, err := archive.BundleDir(root, rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	parent := filepath.Dir(dir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("two archive calls produced %d bundles, want 1", len(entries))
	}
}

// TestReconcile_TerminalTaskArchivesOnALaterReconcile is the only test that
// reaches archiveTerminalTask through Reconcile, and it exists because every
// other test in this file calls the method directly: without it the call site
// can be deleted, or moved inside the firstTerminal branch, and the whole
// feature disconnects with every gate still green.
//
// The audited annotation is pre-stamped, so firstTerminal is false on this
// pass. That is the reconcile a resync produces for an already-recorded task,
// and it is exactly the pass a firstTerminal gate would skip. Asserting the
// bundle exists after it pins both the call site and its placement outside
// that branch, which is what makes a failed write retryable rather than
// permanent data loss.
func TestReconcile_TerminalTaskArchivesOnALaterReconcile(t *testing.T) {
	root := t.TempDir()
	task := terminalTask()
	task.Annotations = map[string]string{audit.AuditedAnnotation: "true"}

	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task), ArchiveDir: root}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	rec := audit.BuildRecord(task, nil)
	dir, err := archive.BundleDir(root, rec)
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.json")); err != nil {
		t.Errorf("Reconcile did not archive a terminal task on a non-first terminal pass: %v", err)
	}
}

// TestArchiveTerminalTask_RecordsResolvedAgent pins that the resolved Agent
// reaches the bundle. Replacing audit.ResolveAgent with a literal nil is
// otherwise invisible: the record still writes, the bundle still lands, and
// only the agent block quietly disappears.
//
// The endpoint assertion is the point. record.go calls it "the
// inference-provenance proof", and a bundle that cannot say which model at
// which endpoint produced the run answers much less of what archival is for.
func TestArchiveTerminalTask_RecordsResolvedAgent(t *testing.T) {
	root := t.TempDir()
	task := terminalTask()
	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task), ArchiveDir: root}
	r.archiveTerminalTask(context.Background(), task, logr.Discard())

	dir, err := archive.BundleDir(root, audit.BuildRecord(task, nil))
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "audit.json"))
	if err != nil {
		t.Fatalf("audit.json not written: %v", err)
	}
	var got audit.Record
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode audit.json: %v", err)
	}
	if got.Agent == nil {
		t.Fatalf("audit.json carries no agent block; the resolved Agent never reached the record")
	}
	if got.Agent.Name != archiveAgentName {
		t.Errorf("agent.name = %q, want %q", got.Agent.Name, archiveAgentName)
	}
	if got.Agent.Model != archiveAgentModel {
		t.Errorf("agent.model = %q, want %q", got.Agent.Model, archiveAgentModel)
	}
	if got.Agent.Endpoint != archiveAgentEndpoint {
		t.Errorf("agent.endpoint = %q, want %q; the bundle lost its inference-provenance proof",
			got.Agent.Endpoint, archiveAgentEndpoint)
	}
}

// TestArchiveTerminalTask_TranscriptPresentButKeyMissingIsCounted covers the
// case between "no ConfigMap" and "a transcript": the ConfigMap resolves and
// holds nothing under the key we read.
//
// Left uncounted this is the worst failure of the three, because it is the
// only one with no signal at all. meta.json records hasTranscript:false, which
// is exactly what a deterministic run that produced no transcript writes, so a
// producer-side key rename would drop every transcript in the fleet and look
// like normal operation.
func TestArchiveTerminalTask_TranscriptPresentButKeyMissingIsCounted(t *testing.T) {
	root := t.TempDir()
	task := terminalTask()
	task.Status.TranscriptRef = "foreman-transcript-archive-me"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: task.Status.TranscriptRef, Namespace: task.Namespace},
		Data:       map[string]string{"transcript": `{"turns":[]}`},
	}
	before := archiveFailureCount(t, "transcript_empty")

	r := &AgenticTaskReconciler{Client: fakeClientWithTask(t, task, cm), ArchiveDir: root}
	r.archiveTerminalTask(context.Background(), task, logr.Discard())

	dir, err := archive.BundleDir(root, audit.BuildRecord(task, nil))
	if err != nil {
		t.Fatalf("BundleDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.json")); err != nil {
		t.Errorf("a transcript ConfigMap missing its key must still archive the record: %v", err)
	}
	if got := archiveFailureCount(t, "transcript_empty"); got <= before {
		t.Errorf("foreman_archive_failures_total{reason=\"transcript_empty\"} = %v, want an increment: "+
			"a resolved ConfigMap with no transcript under %q is silent data loss otherwise",
			got, "transcript.json")
	}
}

// --- archive test helpers ---

const (
	archiveAgentName     = "archive-coder"
	archiveAgentModel    = "qwopus-fusion-27b"
	archiveAgentEndpoint = "http://foundation-router.lan:4000/v1"
)

// terminalTask returns a Succeeded AgenticTask carrying exactly the fields the
// bundle key is built from.
//
// Repo and Issue live under Spec.Payload, not directly on Spec: BuildRecord
// reads task.Spec.Payload.Repo and int(task.Spec.Payload.Issue). FinishedAt is
// load-bearing for the same reason: BuildRecord sets RecordedAt only when
// FinishedAt is non-nil, and RecordedAt is the timestamp half of the key. A
// fixture missing either one still archives, but under a "no-repo/no-issue"
// or UID-keyed path that production would not produce.
//
// The AgentRef is load-bearing too, though not for the path: without it
// audit.ResolveAgent short-circuits to nil and the bundle carries no agent
// block, so nothing would notice the resolution being dropped. See
// TestArchiveTerminalTask_RecordsResolvedAgent. fakeClientWithTask seeds the
// matching Agent.
func terminalTask() *foremanv1alpha1.AgenticTask {
	finished := metav1.Date(2026, time.August, 23, 17, 4, 5, 0, time.UTC)
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "archive-me",
			Namespace: "default",
			UID:       types.UID("11111111-2222-3333-4444-555555555555"),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
			AgentRef: &corev1.LocalObjectReference{Name: archiveAgentName},
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:  "defilantech/LLMKube",
				Issue: int32(1602),
			},
		},
	}
	task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
	task.Status.FinishedAt = &finished
	return task
}

// fakeClientWithTask builds a fake client holding the task, the Agent its
// AgentRef names, and any extra objects a test needs (a transcript ConfigMap,
// say). corev1 is registered because archiveTerminalTask reads the transcript
// ConfigMap through it.
//
// The Agent is seeded by default so the resolvable path, which is the
// production-typical one, is what the tests exercise. A test that wants the
// unresolvable path clears task.Spec.AgentRef before calling.
func fakeClientWithTask(
	t *testing.T,
	task *foremanv1alpha1.AgenticTask,
	extra ...client.Object,
) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := foremanv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add foreman scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	objs := append([]client.Object{task}, extra...)
	if ref := task.Spec.AgentRef; ref != nil && ref.Name != "" {
		objs = append(objs, archiveAgent(ref.Name, task.Namespace))
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// archiveAgent is the Agent terminalTask points at. It is a cloud-proxy agent
// so ProviderConfig.BaseURL is populated: record.go calls that endpoint "the
// inference-provenance proof", and for archival it is much of the point of
// keeping a bundle at all.
func archiveAgent(name, namespace string) *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: foremanv1alpha1.AgentSpec{
			Role:     foremanv1alpha1.AgentRoleCoder,
			Model:    archiveAgentModel,
			Provider: foremanv1alpha1.AgentProviderCloudProxy,
			ProviderConfig: &foremanv1alpha1.ProviderConfig{
				BaseURL: archiveAgentEndpoint,
				Model:   archiveAgentModel,
			},
		},
	}
}

// archiveFailureCount reads the archive failure counter for one bounded
// reason label.
func archiveFailureCount(t *testing.T, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(llmkubemetrics.ForemanArchiveFailuresTotal.WithLabelValues(reason))
}
