package agent

import (
	"reflect"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

func TestContext7Evidence_CollectsMCPToolResults(t *testing.T) {
	tr := []oai.Message{
		{Role: oai.RoleUser, Content: "fix it"},
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{
			{ID: "c1", Type: "function",
				Function: oai.ToolCallFunction{Name: "mcp/context7/query-docs"}},
		}},
		{Role: oai.RoleTool, ToolCallID: "c1", Name: "mcp/context7/query-docs",
			Content: "vllm:request_success_total{finished_reason}"},
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{
			{ID: "c2", Type: "function", Function: oai.ToolCallFunction{Name: "read_file"}},
		}},
		{Role: oai.RoleTool, ToolCallID: "c2", Name: "read_file", Content: "some file"},
	}
	got := context7Evidence(tr)
	want := []string{"vllm:request_success_total{finished_reason}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("context7Evidence = %v, want %v", got, want)
	}
}

// Correlation fallback: some backends leave Name empty on the tool message;
// the call name must still be recovered via ToolCallID.
func TestContext7Evidence_CorrelatesByToolCallIDWhenNameEmpty(t *testing.T) {
	tr := []oai.Message{
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{
			{ID: "c1", Type: "function", Function: oai.ToolCallFunction{Name: "mcp/context7/resolve-library-id"}},
		}},
		{Role: oai.RoleTool, ToolCallID: "c1", Content: "/websites/vllm"},
	}
	if got := context7Evidence(tr); len(got) != 1 || got[0] != "/websites/vllm" {
		t.Fatalf("context7Evidence = %v, want [/websites/vllm]", got)
	}
}

func TestGroundingViolations_FlagsHallucinatedSameNamespaceMetric(t *testing.T) {
	evidence := []string{
		"# HELP vllm:request_success_total ...\nvllm:request_success_total{finished_reason=\"stop\"} 1.0",
	}
	added := []string{
		`- record: llmkube:err`,
		`  expr: rate(vllm:request_failure_total{status_class="5xx"}[5m])`,
	}
	got := groundingViolations(evidence, added)
	if len(got) != 1 {
		t.Fatalf("want 1 violation, got %d: %+v", len(got), got)
	}
	if got[0].Written != "vllm:request_failure_total" || got[0].Namespace != "vllm" {
		t.Fatalf("violation = %+v", got[0])
	}
	// It should offer the retrieved same-namespace name as the alternative.
	found := false
	for _, a := range got[0].RetrievedAlternatives {
		if a == "vllm:request_success_total" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alternatives = %v, want to include vllm:request_success_total", got[0].RetrievedAlternatives)
	}
}

func TestGroundingViolations_CleanWhenWrittenMetricIsInEvidence(t *testing.T) {
	evidence := []string{"vllm:request_success_total{finished_reason}"}
	added := []string{`  expr: rate(vllm:request_success_total[5m])`}
	if got := groundingViolations(evidence, added); len(got) != 0 {
		t.Fatalf("want 0 violations, got %+v", got)
	}
}

func TestGroundingViolations_IgnoresDifferentNamespace(t *testing.T) {
	// context7 was queried about vllm; a go_gc_* metric was never grounded, so
	// it must NOT be flagged even though it is absent from the evidence.
	evidence := []string{"vllm:request_success_total"}
	added := []string{`  expr: go_gc_duration_seconds{quantile="0.5"}`}
	if got := groundingViolations(evidence, added); len(got) != 0 {
		t.Fatalf("want 0 violations for ungrounded namespace, got %+v", got)
	}
}

func TestGroundingViolations_EmptyEvidenceIsNoOp(t *testing.T) {
	if got := groundingViolations(nil, []string{"vllm:request_failure_total"}); len(got) != 0 {
		t.Fatalf("empty evidence must yield no violations, got %+v", got)
	}
}
