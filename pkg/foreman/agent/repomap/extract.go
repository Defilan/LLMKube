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
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// extracted holds the structured signal repomap pulls from one Go
// source file. The scorer reads these fields directly; the formatter
// renders them into the markdown summary.
//
// PackageDoc is the first paragraph (one or two lines) of the
// package-level comment when present. Often the most concise
// description of what a file or package is for.
//
// Decls is the ordered list of top-level declarations: functions,
// methods, type definitions. Each entry is a single-line signature
// rendered as the model would see it (e.g. `func Foo(s string) error`,
// `type Workload struct`, `func (w *Workload) Reconcile(ctx ...)`).
//
// Identifiers is the bag of bare identifiers (Foo, Workload, etc.)
// the scorer uses for issue-text overlap. Built once during extract
// so the scorer doesn't re-walk the file.
type extracted struct {
	PackageName string
	PackageDoc  string
	Decls       []string
	Identifiers []string
}

// extractGo parses path as a Go source file and returns its top-level
// structure. Parse errors are non-fatal: the caller (walk) treats
// any error as "skip the structured signal but keep the file in the
// index" so the scorer can still consider its path.
//
// Parse mode is ParseComments because the package-level doc is the
// single most useful 1-2 lines of context for the summary; everything
// else uses the AST positions directly.
func extractGo(path string) (extracted, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return extracted{}, err
	}

	out := extracted{
		PackageName: f.Name.Name,
	}
	if f.Doc != nil {
		out.PackageDoc = firstParagraph(f.Doc.Text())
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sig := renderFuncDecl(d)
			if sig != "" {
				out.Decls = append(out.Decls, sig)
			}
			if d.Name != nil {
				out.Identifiers = append(out.Identifiers, d.Name.Name)
			}
		case *ast.GenDecl:
			// Type, const, var blocks. We only surface exported
			// type / const declarations in the summary; per-symbol
			// var/const detail is rarely the answer to "what's
			// relevant" and bloats the output.
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					sig := renderTypeSpec(s)
					if sig != "" {
						out.Decls = append(out.Decls, sig)
					}
					if s.Name != nil {
						out.Identifiers = append(out.Identifiers, s.Name.Name)
					}
				case *ast.ValueSpec:
					// Surface exported consts only; unexported and
					// var-typed declarations are noise in a summary.
					if d.Tok != token.CONST {
						continue
					}
					for _, n := range s.Names {
						if !n.IsExported() {
							continue
						}
						out.Decls = append(out.Decls, "const "+n.Name)
						out.Identifiers = append(out.Identifiers, n.Name)
					}
				}
			}
		}
	}

	return out, nil
}

// firstParagraph returns the first non-empty paragraph of a godoc
// comment. The full comment can be many paragraphs of detail; the
// summary only needs the lead sentence or two. A paragraph is
// terminated by a blank line.
func firstParagraph(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	parts := strings.SplitN(text, "\n\n", 2)
	para := parts[0]
	// Collapse internal newlines to single spaces so the summary
	// can render the paragraph on one line.
	para = strings.ReplaceAll(para, "\n", " ")
	para = strings.Join(strings.Fields(para), " ")
	return para
}

// renderFuncDecl produces a single-line signature for a top-level
// function or method. The format mirrors what a developer would type
// at the package level so the model reads it naturally.
//
// Examples:
//
//	func New() (*Registry, error)
//	func (r *Registry) Dispatch(ctx context.Context, name string, args json.RawMessage) (*Result, error)
//
// Unexported functions are skipped: the summary's purpose is to
// signal the API surface; internal helpers dilute the signal.
func renderFuncDecl(d *ast.FuncDecl) string {
	if d.Name == nil || !d.Name.IsExported() {
		return ""
	}

	var b strings.Builder
	b.WriteString("func ")

	if d.Recv != nil && len(d.Recv.List) > 0 {
		b.WriteString("(")
		b.WriteString(renderFieldList(d.Recv))
		b.WriteString(") ")
	}

	b.WriteString(d.Name.Name)
	b.WriteString("(")
	if d.Type.Params != nil {
		b.WriteString(renderFieldList(d.Type.Params))
	}
	b.WriteString(")")

	if d.Type.Results != nil && len(d.Type.Results.List) > 0 {
		results := renderFieldList(d.Type.Results)
		b.WriteString(" ")
		if len(d.Type.Results.List) > 1 || (len(d.Type.Results.List) == 1 && len(d.Type.Results.List[0].Names) > 0) {
			b.WriteString("(")
			b.WriteString(results)
			b.WriteString(")")
		} else {
			b.WriteString(results)
		}
	}

	return b.String()
}

// renderTypeSpec renders a top-level type declaration as one line.
// Examples:
//
//	type Workload struct
//	type Registry interface
//	type AgentRole string
//
// Field-level detail is intentionally omitted (the summary is meant
// to fit a 4K-token budget; full struct dumps would blow it on a
// medium repo). The model has read_file to pull the body when it
// matters.
func renderTypeSpec(s *ast.TypeSpec) string {
	if s.Name == nil || !s.Name.IsExported() {
		return ""
	}
	var kind string
	switch s.Type.(type) {
	case *ast.StructType:
		kind = "struct"
	case *ast.InterfaceType:
		kind = "interface"
	default:
		kind = renderExpr(s.Type)
	}
	return "type " + s.Name.Name + " " + kind
}

// renderFieldList renders a parameter or result list as a comma-joined
// string. Names are included when present (Go allows unnamed return
// values); types are rendered via renderExpr.
func renderFieldList(fl *ast.FieldList) string {
	if fl == nil {
		return ""
	}
	parts := make([]string, 0, len(fl.List))
	for _, f := range fl.List {
		typeStr := renderExpr(f.Type)
		if len(f.Names) == 0 {
			parts = append(parts, typeStr)
			continue
		}
		names := make([]string, 0, len(f.Names))
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
		parts = append(parts, strings.Join(names, ", ")+" "+typeStr)
	}
	return strings.Join(parts, ", ")
}

// renderExpr is a best-effort string rendering of an AST type
// expression. We avoid go/printer (which would balloon the
// dependency surface and produce multi-line output on complex types);
// the heuristic here is good enough for top-level declarations and
// keeps the summary compact.
func renderExpr(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.SelectorExpr:
		return renderExpr(n.X) + "." + n.Sel.Name
	case *ast.StarExpr:
		return "*" + renderExpr(n.X)
	case *ast.ArrayType:
		return "[]" + renderExpr(n.Elt)
	case *ast.MapType:
		return "map[" + renderExpr(n.Key) + "]" + renderExpr(n.Value)
	case *ast.Ellipsis:
		return "..." + renderExpr(n.Elt)
	case *ast.FuncType:
		var b strings.Builder
		b.WriteString("func(")
		if n.Params != nil {
			b.WriteString(renderFieldList(n.Params))
		}
		b.WriteString(")")
		if n.Results != nil && len(n.Results.List) > 0 {
			b.WriteString(" ")
			b.WriteString(renderFieldList(n.Results))
		}
		return b.String()
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	case *ast.ChanType:
		return "chan " + renderExpr(n.Value)
	}
	return "?"
}
