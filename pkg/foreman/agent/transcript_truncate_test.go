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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// truncScheme is a package-internal scheme: the shared newScheme helper lives
// in executor_native_test.go, which is package agent_test and therefore not
// importable from these internal tests.
func truncScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgo scheme: %v", err)
	}
	if err := foremanv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("foreman scheme: %v", err)
	}
	return s
}

// bigTranscript builds a transcript of n turns whose marshalled size comfortably
// exceeds transcriptCapBytes, mirroring the shape of a real coder run: an
// alternating assistant/tool sequence with reasoning content.
func bigTranscript(n, msgBytes int) []oai.Message {
	msgs := []oai.Message{
		{Role: oai.RoleSystem, Content: "you are a coder"},
		{Role: oai.RoleUser, Content: "fix the bug"},
	}
	filler := strings.Repeat("x", msgBytes)
	for i := 0; i < n; i++ {
		msgs = append(msgs,
			oai.Message{Role: oai.RoleAssistant, Content: fmt.Sprintf("turn-%d %s", i, filler)},
			oai.Message{Role: oai.RoleTool, ToolCallID: fmt.Sprintf("c%d", i), Content: filler},
		)
	}
	return msgs
}

// TestWriteTranscript_TruncationFillsTheBudget is the regression for #1672.
//
// truncateMessages was a FIXED split -- system + first user + last 10 -- so a
// transcript one byte over the 1008 KiB cap was cut to 13 messages regardless
// of how far over it was. Measured on a real run (wl-1331-ornith-v3-code-1331,
// 198 turns): 13 messages and 95 KB stored against a 1032192-byte cap, 9.5%
// utilisation, while two runs that fit under the cap kept 220 and 241 messages.
//
// The bias is systematic: only long runs exceed the cap, and long runs are
// disproportionately the failures, so this gutted exactly the transcripts worth
// keeping.
func TestWriteTranscript_TruncationFillsTheBudget(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "trunc-budget", Namespace: "default"},
		Spec:       foremanv1alpha1.AgenticTaskSpec{Kind: foremanv1alpha1.AgenticTaskKindIssueFix},
	}
	c := fake.NewClientBuilder().WithScheme(truncScheme(t)).WithObjects(task).Build()

	// ~2400 messages of ~1 KB: several times over the cap, like a long run.
	msgs := bigTranscript(1200, 1000)
	ref, err := WriteTranscript(context.Background(), c, task, &LoopResult{
		Turns: 1200, Transcript: msgs,
	})
	if err != nil {
		t.Fatalf("WriteTranscript: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, &cm); err != nil {
		t.Fatalf("get transcript ConfigMap: %v", err)
	}
	stored := len(cm.Data[transcriptDataKey])

	var doc TranscriptDoc
	if err := json.Unmarshal([]byte(cm.Data[transcriptDataKey]), &doc); err != nil {
		t.Fatalf("unmarshal transcript: %v", err)
	}
	if !doc.Truncated {
		t.Fatalf("fixture did not exceed the cap; stored=%d cap=%d", stored, transcriptCapBytes)
	}

	// Must still fit.
	if stored > transcriptCapBytes {
		t.Fatalf("stored %d exceeds cap %d", stored, transcriptCapBytes)
	}

	utilisation := float64(stored) / float64(transcriptCapBytes) * 100
	t.Logf("retained %d messages, %d bytes, %.1f%% of the %d-byte budget",
		len(doc.Messages), stored, utilisation, transcriptCapBytes)

	// The defect: a fixed 13-message split. A budget-aware truncator should
	// fill most of the space it is allowed.
	if utilisation < 50 {
		t.Errorf("truncation used only %.1f%% of the budget (%d bytes of %d); "+
			"a marginally-over transcript should lose middle messages, not 90%% of them",
			utilisation, stored, transcriptCapBytes)
	}
	if len(doc.Messages) <= 13 {
		t.Errorf("retained %d messages; the fixed head+marker+10-tail split is still in effect",
			len(doc.Messages))
	}
}

// TestWriteTranscript_TruncationKeepsHeadMarkerAndTail pins the structure the
// budget-aware truncator must preserve: the system prompt and first user
// message at the head, a marker naming how much was dropped, and the final
// messages (where submit_result lives) at the tail.
func TestWriteTranscript_TruncationKeepsHeadMarkerAndTail(t *testing.T) {
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "trunc-shape", Namespace: "default"},
		Spec:       foremanv1alpha1.AgenticTaskSpec{Kind: foremanv1alpha1.AgenticTaskKindIssueFix},
	}
	c := fake.NewClientBuilder().WithScheme(truncScheme(t)).WithObjects(task).Build()

	msgs := bigTranscript(1200, 1000)
	msgs[len(msgs)-1] = oai.Message{Role: oai.RoleTool, Content: "SUBMIT_RESULT_SENTINEL"}

	ref, err := WriteTranscript(context.Background(), c, task, &LoopResult{
		Turns: 1200, Transcript: msgs,
	})
	if err != nil {
		t.Fatalf("WriteTranscript: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, &cm); err != nil {
		t.Fatalf("get: %v", err)
	}
	var doc TranscriptDoc
	if err := json.Unmarshal([]byte(cm.Data[transcriptDataKey]), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if doc.Messages[0].Role != oai.RoleSystem {
		t.Errorf("head[0]: want the system prompt, got role %q", doc.Messages[0].Role)
	}
	last := doc.Messages[len(doc.Messages)-1]
	if !strings.Contains(last.Content, "SUBMIT_RESULT_SENTINEL") {
		t.Errorf("tail must end at the final message; got %q", last.Content)
	}
	marker := false
	for _, m := range doc.Messages {
		if strings.Contains(m.Content, "transcript truncated") {
			marker = true
			break
		}
	}
	if !marker {
		t.Error("no truncation marker naming the dropped middle")
	}
}
