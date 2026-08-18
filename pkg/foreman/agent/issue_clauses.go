package agent

import (
	"strings"
)

// clauseHeadings are the section headings (lower-cased, as they appear in the
// body) whose contents contribute behaviour clauses. A section runs until the
// next line beginning "## ".
var clauseHeadings = map[string]bool{
	strings.ToLower("## Expected Behavior"):   true,
	strings.ToLower("## Expected Behaviour"):  true,
	strings.ToLower("## Acceptance Criteria"): true,
}

// extractClauses pulls the list items (or prose lines) out of behaviour
// sections in an issue body. Multiple matching sections contribute, in document
// order. It returns an empty slice (never nil) when no section matches, and
// never panics.
func extractClauses(issueBody string) []string {
	clauses := make([]string, 0)
	if issueBody == "" {
		return clauses
	}

	lines := strings.Split(issueBody, "\n")
	inSection := false
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)

		// A line beginning "## " is a section boundary. A clause heading
		// opens a section; any other "## " heading closes it.
		if trimmed == "##" || strings.HasPrefix(trimmed, "## ") {
			inSection = clauseHeadings[strings.ToLower(trimmed)]
			continue
		}
		if !inSection {
			continue
		}

		// A list item contributes its text with the marker and surrounding
		// whitespace stripped.
		if item, ok := listItem(trimmed); ok {
			if item != "" {
				clauses = append(clauses, item)
			}
			continue
		}
		// Prose: keep non-empty lines, dropping blanks.
		if trimmed != "" {
			clauses = append(clauses, trimmed)
		}
	}
	return clauses
}

// listItem reports whether trimmed is a markdown list item and returns the item
// text with the marker and surrounding whitespace stripped. It recognises the
// unordered markers "-" and "*" (each followed by a space) and the ordered
// markers "N." / "N)".
func listItem(trimmed string) (string, bool) {
	if len(trimmed) < 2 {
		return "", false
	}
	c := trimmed[0]
	switch {
	case c == '-' || c == '*':
		if trimmed[1] != ' ' {
			return "", false
		}
		return strings.TrimSpace(trimmed[1:]), true
	case c >= '0' && c <= '9':
		j := 0
		for j < len(trimmed) && trimmed[j] >= '0' && trimmed[j] <= '9' {
			j++
		}
		// "N." or "N)" marker.
		if j < len(trimmed) && (trimmed[j] == '.' || trimmed[j] == ')') {
			return strings.TrimSpace(trimmed[j+1:]), true
		}
		return "", false
	default:
		return "", false
	}
}

// clauseChecklist renders clauses as a checklist suitable for pasting into a
// prompt. Empty input returns "".
func clauseChecklist(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	var b strings.Builder
	for i, c := range clauses {
		b.WriteString("- [ ] ")
		b.WriteString(c)
		if i < len(clauses)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// unsatisfiedClauses returns the 0-based indices (ascending) of clauses that
// have no entry in cited, or whose entry is empty or whitespace-only. Keys in
// cited that are out of range are ignored rather than panicking.
func unsatisfiedClauses(clauses []string, cited map[int]string) []int {
	var out []int
	for i := range clauses {
		if v, ok := cited[i]; !ok || strings.TrimSpace(v) == "" {
			out = append(out, i)
		}
	}
	return out
}
