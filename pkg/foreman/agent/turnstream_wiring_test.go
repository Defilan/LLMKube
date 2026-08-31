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

package agent_test

import (
	"context"
	"path/filepath"
	"testing"

	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// These tests drive a REAL end-to-end coder pass (the same harness the envtest
// loop tests use) rather than calling Publish directly, because the thing under
// test is the wiring: that the executor actually hands loop turns to the
// stream. A unit test that publishes by hand would pass with the hook
// disconnected, which is the exact defect this guards.

// streamWiringExecutor is envtestLoopExecutor minus the envtest runner, plus a
// stream. It deliberately reuses the same fake OAI + fake registry harness so a
// change that breaks the loop's turn accounting fails here too.
func streamWiringExecutor(
	t *testing.T, root, bare, oaiURL string, stream *foremanagent.TurnStream,
) *foremanagent.NativeAgentLoopExecutor {
	t.Helper()
	agent, task := taskAndAgent("stream-wiring")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()
	reg := &seqEnvtestRegistry{verdicts: []string{"GO"}}
	return &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiURL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		CommitCommitter:          repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		RegistryFactory: func(
			_ context.Context, ws string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory: fakeAuth(t),
		Stream:      stream,
	}
}

// A run on an executor holding a stream publishes that run's real turns.
func TestNativeExecutor_PublishesTurnsToStream(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})
	stream := foremanagent.NewTurnStream(16)

	e := streamWiringExecutor(t, root, bare, oaiSrv.URL, stream)
	_, task := taskAndAgent("stream-wiring")
	if _, err := execWithAgent(t, e, task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Subscribe AFTER the run: the replay buffer is what makes a viewer that
	// attaches mid-run (or just after) still see what happened, so draining it
	// here exercises the same path the SSE handler uses.
	ch, cancel := stream.Subscribe()
	defer cancel()

	var events []foremanagent.TurnEvent
	for {
		select {
		case ev := <-ch:
			events = append(events, ev)
			continue
		default:
		}
		break
	}

	if len(events) == 0 {
		t.Fatal("stream received no turns; the executor never wired OnTurn to the stream")
	}

	// Turn 0 is the pre-loop system+user seed, not a turn the model took. It
	// leaking into the stream was a real bug in the flusher (fixed by seeding
	// emitted), so guard it here at the wired level too.
	var sawSubmit bool
	for i, ev := range events {
		if ev.Turn < 1 {
			t.Errorf("event %d has turn %d; turns are 1-based and the pre-loop seed must not publish", i, ev.Turn)
		}
		if len(ev.Messages) == 0 {
			t.Errorf("event %d (turn %d) carries no messages", i, ev.Turn)
		}
		for _, m := range ev.Messages {
			if m.Role != oai.RoleAssistant {
				continue
			}
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "submit_result" {
					sawSubmit = true
				}
			}
		}
	}

	// Proves the events carry the RUN's transcript and not empty scaffolding.
	if !sawSubmit {
		t.Errorf("no assistant submit_result tool call in %d published event(s); "+
			"the stream is not carrying the real transcript", len(events))
	}
}

// A nil stream must leave the loop untouched: the streaming path is opt-in and
// every existing deployment runs with it unset.
func TestNativeExecutor_NilStreamDoesNotPanic(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, []string{submitGoBody})

	e := streamWiringExecutor(t, root, bare, oaiSrv.URL, nil)
	_, task := taskAndAgent("stream-wiring")
	res, err := execWithAgent(t, e, task)
	if err != nil {
		t.Fatalf("Execute with nil stream: %v", err)
	}
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict with nil stream: want GO got %s", res.Verdict)
	}
}
