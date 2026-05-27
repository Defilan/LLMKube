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

package repomap

import (
	"fmt"
	"path/filepath"
	"strings"
)

// charsPerTokenApprox mirrors the agent loop's heuristic: 4 chars ≈
// 1 token for English + code mixed text. The empirical finding (the
// agent loop's masking research) is that precise tokenization is not
// required for this kind of budget bound.
const charsPerTokenApprox = 4

// maxDeclsPerFile bounds how many top-level declarations we list per
// file in the summary. A file with 80 functions is rarely usefully
// described by listing all 80; the top 15 give the model the API
// shape and it can read_file for the rest.
const maxDeclsPerFile = 15

// format builds the final markdown summary from the scored files.
// Files are emitted in score order until the token budget is hit.
// The output is intended for prepending to a coder Agent's user
// prompt (see executor wiring in pkg/foreman/agent/executor_native.go).
//
// Shape:
//
//	## Repository overview
//
//	<count> files listed, weighted by relevance to the task. Re-read
//	any file with `read_file` when you need more context than this
//	summary provides.
//
//	### path/to/file.go
//	package summary, when present.
//	- func Exported(args) ret
//	- type Foo struct
//	- func (f *Foo) Method()
//
//	### path/to/other.go
//	...
//
// Budget is enforced by approximate char count (chars/4 ≈ tokens).
// When a file's full entry would exceed the remaining budget, we
// either include a truncated form (header + first few decls) or skip
// the file entirely, whichever leaves a coherent boundary.
func format(workspace string, scored []scoredFile, tokenBudget int) string {
	if len(scored) == 0 {
		return ""
	}
	charBudget := tokenBudget * charsPerTokenApprox

	var b strings.Builder
	b.WriteString("## Repository overview\n\n")
	b.WriteString(fmt.Sprintf(
		"%s, %d source files indexed, top entries weighted by relevance to the task. "+
			"Re-read any file with the `read_file` tool when you need more context "+
			"than this summary provides.\n\n",
		filepath.Base(strings.TrimRight(workspace, string(filepath.Separator))),
		len(scored),
	))

	included := 0
	for _, sf := range scored {
		entry := renderFileEntry(sf)
		if b.Len()+len(entry) > charBudget {
			// Stop including new files when we'd exceed the budget;
			// abrupt-truncation is fine because the trailing summary
			// note tells the model the list is bounded.
			break
		}
		b.WriteString(entry)
		included++
	}

	if included < len(scored) {
		b.WriteString(fmt.Sprintf(
			"\n_(showing %d of %d ranked files; budget exhausted. "+
				"Use `grep` or `read_file` for any other path you need.)_\n",
			included, len(scored),
		))
	}

	return b.String()
}

// renderFileEntry produces the per-file markdown block. Always
// returns the same compact shape: header + optional package doc +
// bulleted declarations. Truncates the decl list to maxDeclsPerFile
// to keep one giant file from monopolizing the summary.
func renderFileEntry(sf scoredFile) string {
	var b strings.Builder
	b.WriteString("### ")
	b.WriteString(sf.Path)
	b.WriteString("\n")

	if sf.Extracted.PackageDoc != "" {
		b.WriteString(sf.Extracted.PackageDoc)
		b.WriteString("\n")
	}

	decls := sf.Extracted.Decls
	if len(decls) > maxDeclsPerFile {
		decls = decls[:maxDeclsPerFile]
	}
	for _, d := range decls {
		b.WriteString("- ")
		b.WriteString(d)
		b.WriteString("\n")
	}

	if len(sf.Extracted.Decls) > maxDeclsPerFile {
		b.WriteString(fmt.Sprintf("- _(+%d more declarations)_\n",
			len(sf.Extracted.Decls)-maxDeclsPerFile))
	}

	b.WriteString("\n")
	return b.String()
}
