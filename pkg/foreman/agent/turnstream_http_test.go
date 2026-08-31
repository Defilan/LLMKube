package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// readEvent reads one SSE "data:" frame and decodes it. Decoding rather than
// substring-matching means a schema change fails the test instead of passing
// by luck.
func readEvent(t *testing.T, r *bufio.Reader) TurnEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue // blank separator or keepalive comment
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			t.Fatalf("unexpected SSE line: %q", line)
		}
		var ev TurnEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("decode event %q: %v", payload, err)
		}
		return ev
	}
	t.Fatal("timed out waiting for an SSE event")
	return TurnEvent{}
}

func TestTurnStreamHandler_ReplaysThenTails(t *testing.T) {
	s := NewTurnStream(8)
	s.Publish(1, []oai.Message{{
		Role:      oai.RoleAssistant,
		ToolCalls: []oai.ToolCall{{Function: oai.ToolCallFunction{Name: "write_file", Arguments: `{"path":"a.go"}`}}},
	}})

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	r := bufio.NewReader(resp.Body)

	// The buffered turn must arrive even though it was published before the
	// client connected: that is the whole point of replay.
	ev := readEvent(t, r)
	if ev.Turn != 1 {
		t.Fatalf("replayed turn = %d, want 1", ev.Turn)
	}
	// Tool CALL arguments must survive the wire. Showing what the agent wrote,
	// not merely that it wrote, is what separates this from a log tail.
	if len(ev.Messages) != 1 || len(ev.Messages[0].ToolCalls) != 1 {
		t.Fatalf("tool calls lost in transit: %+v", ev.Messages)
	}
	if got := ev.Messages[0].ToolCalls[0].Function.Name; got != "write_file" {
		t.Errorf("tool call name = %q, want write_file", got)
	}
	if !strings.Contains(ev.Messages[0].ToolCalls[0].Function.Arguments, "a.go") {
		t.Errorf("tool call arguments lost: %q", ev.Messages[0].ToolCalls[0].Function.Arguments)
	}

	// And a turn published after connect must arrive live.
	s.Publish(2, []oai.Message{{Role: oai.RoleAssistant, Content: "second"}})
	if ev := readEvent(t, r); ev.Turn != 2 {
		t.Errorf("live turn = %d, want 2", ev.Turn)
	}
}

// TestTurnStreamHandler_ClientDisconnectUnsubscribes: a closed browser tab must
// not leak a subscriber for the life of the agent process.
func TestTurnStreamHandler_ClientDisconnectUnsubscribes(t *testing.T) {
	s := NewTurnStream(4)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	s.Publish(1, []oai.Message{{Role: oai.RoleAssistant, Content: "x"}})
	r := bufio.NewReader(resp.Body)
	_ = readEvent(t, r)

	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.SubscriberCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("subscriber still registered after disconnect: count = %d", s.SubscriberCount())
}
