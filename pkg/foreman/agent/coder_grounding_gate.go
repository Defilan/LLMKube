package agent

import (
	"regexp"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// metricIdentRe matches Prometheus/vLLM metric-name-shaped identifiers:
// a namespace, a colon, then the metric path (e.g. vllm:request_failure_total,
// llmkube:inference:ttft_seconds:p95_5m). v1 checks exactly this class -- the
// tokens most prone to hallucination and the ones that failed on #409/#850.
var metricIdentRe = regexp.MustCompile(`[a-z_][a-z0-9_]*:[a-z0-9_:]+`)

// metricIdents returns the metric-name-shaped identifiers in s, EXCLUDING
// host:port network addresses (whose path after the first colon is all digits,
// e.g. "vllm:8000") -- those are not Prometheus metrics and must not be flagged.
func metricIdents(s string) []string {
	var out []string
	for _, m := range metricIdentRe.FindAllString(s, -1) {
		i := strings.IndexByte(m, ':')
		if i < 0 {
			continue
		}
		path := m[i+1:]
		if path == "" || isAllDigits(path) {
			continue // host:port, not a metric
		}
		out = append(out, m)
	}
	return out
}

// isAllDigits returns true if s contains only ASCII digit characters.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// groundingViolation is one written external identifier that contradicts the
// retrieved docs: its namespace was the subject of a context7 lookup this run,
// yet the identifier itself does not appear anywhere in the retrieved evidence.
type groundingViolation struct {
	Written               string
	Namespace             string
	RetrievedAlternatives []string
}

// context7Evidence gathers the content of every mcp/* tool result in the
// transcript into an evidence corpus. The tool message's Name is the function
// name; when a backend leaves Name empty we recover it from the assistant's
// tool_call by ToolCallID.
func context7Evidence(transcript []oai.Message) []string {
	callName := make(map[string]string)
	for _, m := range transcript {
		if m.Role == oai.RoleAssistant {
			for _, tc := range m.ToolCalls {
				callName[tc.ID] = tc.Function.Name
			}
		}
	}
	var ev []string
	for _, m := range transcript {
		if m.Role != oai.RoleTool {
			continue
		}
		name := m.Name
		if name == "" {
			name = callName[m.ToolCallID]
		}
		if strings.HasPrefix(name, "mcp/") {
			ev = append(ev, m.Content)
		}
	}
	return ev
}

func namespaceOf(ident string) string {
	if i := strings.IndexByte(ident, ':'); i > 0 {
		return ident[:i]
	}
	return ""
}

// groundingViolations flags each metric-shaped identifier in addedLines whose
// namespace appears in the evidence (so context7 was queried about that domain)
// but whose full identifier does NOT appear verbatim in the evidence -- a
// likely hallucination the coder wrote despite the docs it fetched.
func groundingViolations(evidence []string, addedLines []string) []groundingViolation {
	if len(evidence) == 0 {
		return nil
	}
	corpus := strings.Join(evidence, "\n")

	// All metric identifiers present in the evidence, grouped by namespace.
	retrievedByNS := make(map[string][]string)
	retrievedSet := make(map[string]bool)
	for _, e := range metricIdents(corpus) {
		if retrievedSet[e] {
			continue
		}
		retrievedSet[e] = true
		if ns := namespaceOf(e); ns != "" {
			retrievedByNS[ns] = append(retrievedByNS[ns], e)
		}
	}

	var out []groundingViolation
	seen := make(map[string]bool)
	for _, line := range addedLines {
		for _, w := range metricIdents(line) {
			if seen[w] {
				continue
			}
			ns := namespaceOf(w)
			// Only check a namespace context7 was actually queried about.
			if len(retrievedByNS[ns]) == 0 {
				continue
			}
			// Grounded if the exact identifier is in the evidence.
			if strings.Contains(corpus, w) {
				continue
			}
			seen[w] = true
			out = append(out, groundingViolation{
				Written:               w,
				Namespace:             ns,
				RetrievedAlternatives: retrievedByNS[ns],
			})
		}
	}
	return out
}
