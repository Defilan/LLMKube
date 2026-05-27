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
	"io/fs"
	"path/filepath"
	"strings"
)

// indexableFile captures the minimum the scorer needs from each file
// found during the walk: its workspace-relative path and the
// extracted top-level structure (package doc + declarations).
//
// Path is always workspace-relative (no leading slash) so the final
// summary's headings render naturally as "### internal/foo/bar.go".
//
// Extracted is the result of running extractGo (or, in v0.3.x, the
// per-language extractor matched by extension). Empty Extracted is a
// valid value: the file passed the walk filters but didn't contribute
// any structured declarations (e.g. an empty Go file with only
// `package foo`).
type indexableFile struct {
	Path      string
	Size      int64
	Extracted extracted
}

// defaultSkipDirs are workspace-relative directory names that the
// walker never enters. These match the patterns Aider, Agentless, and
// the SWE-agent repo-map all exclude by default: vendored
// dependencies, build caches, version control, and IDE state.
//
// generated/auto-generated files are filtered at the file level
// (see isLikelyGenerated) rather than the directory level so we
// catch zz_generated.deepcopy.go but still index the surrounding
// directory.
var defaultSkipDirs = map[string]bool{
	"vendor":       true,
	"node_modules": true,
	".git":         true,
	".idea":        true,
	".vscode":      true,
	".cache":       true,
	"bin":          true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"_output":      true,
	"_build":       true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
}

// walk traverses workspace and returns every indexable Go file, with
// its top-level declarations already extracted. Errors on a single
// file (parse error, unreadable file) are logged via skipped-and-
// counted but never abort the walk; partial coverage is more useful
// than a hard failure mid-summary.
func walk(workspace string, extraSkipPatterns []string) ([]indexableFile, error) {
	var out []indexableFile

	err := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A broken symlink, permission error, etc. Skip the
			// offending entry but keep walking.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Compute the workspace-relative path early; the skip-dir
		// check and downstream summary both use it.
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			name := d.Name()
			if defaultSkipDirs[name] {
				return filepath.SkipDir
			}
			// Hidden dirs (.github excepted: it carries workflow
			// definitions that are useful to surface).
			if strings.HasPrefix(name, ".") && name != ".github" {
				return filepath.SkipDir
			}
			for _, pat := range extraSkipPatterns {
				if matched, _ := filepath.Match(pat, name); matched {
					return filepath.SkipDir
				}
				if matched, _ := filepath.Match(pat, rel); matched {
					return filepath.SkipDir
				}
			}
			return nil
		}

		if !isIndexableFile(rel) {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}

		ex, extractErr := extractGo(path)
		if extractErr != nil {
			// Parse errors are not fatal; the file gets indexed with
			// an empty Extracted and the scorer can still consider
			// its path + size.
			ex = extracted{}
		}

		out = append(out, indexableFile{
			Path:      rel,
			Size:      info.Size(),
			Extracted: ex,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// isIndexableFile reports whether the file is worth extracting + scoring.
// v0.3 supports only Go source; the file-extension filter here also
// excludes obvious noise (binaries, lockfiles, large generated YAML).
// Adding new languages is a matter of widening this filter and
// adding a sibling extractX function in extract.go.
func isIndexableFile(relPath string) bool {
	if !strings.HasSuffix(relPath, ".go") {
		return false
	}
	if strings.HasSuffix(relPath, "_test.go") {
		// Tests are useful evidence in some cases, but their volume
		// would dominate the budget on a well-tested repo and the
		// signal-per-byte is lower than for production code. Skip
		// them in v0.3; a follow-up can add a "include_tests" knob.
		return false
	}
	if isLikelyGenerated(relPath) {
		return false
	}
	return true
}

// isLikelyGenerated catches the common generated-file naming
// conventions Kubebuilder + controller-gen + protoc produce. These
// files are large, structurally repetitive, and almost never the
// answer to "which file is relevant to this issue."
func isLikelyGenerated(relPath string) bool {
	base := filepath.Base(relPath)
	switch {
	case strings.HasPrefix(base, "zz_generated"):
		return true
	case strings.HasSuffix(base, ".pb.go"):
		return true
	case strings.HasSuffix(base, "_string.go"):
		// go:generate stringer outputs (e.g. EnumName_string.go);
		// short and structurally trivial.
		return true
	}
	return false
}
