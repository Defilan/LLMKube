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

package v1alpha1

// GateLanguage identifies the programming language for which the gate
// profile provides commands.
// +kubebuilder:validation:Enum=go;python;rust;node;generic
type GateLanguage string

const (
	// GateLanguageGo is the Go language preset.
	GateLanguageGo GateLanguage = "go"
	// GateLanguagePython is the Python language preset.
	GateLanguagePython GateLanguage = "python"
	// GateLanguageRust is the Rust language preset.
	GateLanguageRust GateLanguage = "rust"
	// GateLanguageNode is the Node.js language preset.
	GateLanguageNode GateLanguage = "node"
	// GateLanguageGeneric is a blank preset; every command must be
	// set explicitly.
	GateLanguageGeneric GateLanguage = "generic"
)

// GateCommands holds the individual gate check commands for a language.
// An empty string means the check is not run.
type GateCommands struct {
	// Format is the command that checks source formatting.
	// +optional
	Format string `json:"format,omitempty"`

	// Lint is the command that runs the linter.
	// +optional
	Lint string `json:"lint,omitempty"`

	// Build is the command that compiles the project.
	// +optional
	Build string `json:"build,omitempty"`

	// Test is the command that runs the test suite.
	// +optional
	Test string `json:"test,omitempty"`

	// CodegenCheck is the command that verifies generated files are
	// in sync with their sources. Empty means no codegen check.
	// +optional
	CodegenCheck string `json:"codegenCheck,omitempty"`
}

// GateProfile declares the gate commands, container image, and source
// file extensions for a given language. It is consumed in a later slice
// by the gate executor; for now it is a declarative configuration only.
type GateProfile struct {
	// Language selects the built-in preset. Defaults to "go" when empty.
	// +optional
	Language GateLanguage `json:"language,omitempty"`

	// Image is the container image used to run the gate checks.
	// +optional
	Image string `json:"image,omitempty"`

	// SourceExtensions lists the file extensions the gate should
	// consider as source files (e.g. ".go", ".py").
	// +optional
	SourceExtensions []string `json:"sourceExtensions,omitempty"`

	// Commands overrides the preset commands. Only non-empty fields
	// replace the preset; empty fields keep the preset value.
	// +optional
	Commands GateCommands `json:"commands,omitempty"`
}

// ResolvedGate is the concrete gate configuration after merging a
// GateProfile with its language preset. All fields are populated.
type ResolvedGate struct {
	// Image is the container image used to run the gate checks.
	Image string `json:"image"`

	// SourceExtensions lists the file extensions the gate should
	// consider as source files.
	SourceExtensions []string `json:"sourceExtensions"`

	// Format is the command that checks source formatting.
	Format string `json:"format"`

	// Lint is the command that runs the linter.
	Lint string `json:"lint"`

	// Build is the command that compiles the project.
	Build string `json:"build"`

	// Test is the command that runs the test suite.
	Test string `json:"test"`

	// CodegenCheck is the command that verifies generated files are
	// in sync with their sources.
	CodegenCheck string `json:"codegenCheck"`
}

// builtInPresets maps GateLanguage to its default GateProfile.
// The "go" preset is byte-identical to LLMKube's current gate.
var builtInPresets = map[GateLanguage]GateProfile{
	GateLanguageGo: {
		Language:         GateLanguageGo,
		Image:            "golang:1.26",
		SourceExtensions: []string{".go"},
		Commands: GateCommands{
			Format:       "gofmt -l .",
			Lint:         "golangci-lint run ./...",
			Build:        "go build ./...",
			Test:         "go test ./...",
			CodegenCheck: "make manifests && make chart-crds",
		},
	},
	GateLanguagePython: {
		Language:         GateLanguagePython,
		Image:            "python:3.13",
		SourceExtensions: []string{".py"},
		Commands: GateCommands{
			Format: "ruff format --check .",
			Lint:   "ruff check .",
			Build:  "python -m compileall .",
			Test:   "pytest -q",
		},
	},
	GateLanguageRust: {
		Language:         GateLanguageRust,
		Image:            "rust:1",
		SourceExtensions: []string{".rs"},
		Commands: GateCommands{
			Format: "cargo fmt --check",
			Lint:   "cargo clippy",
			Build:  "cargo build",
			Test:   "cargo test",
		},
	},
	GateLanguageNode: {
		Language:         GateLanguageNode,
		Image:            "node:22",
		SourceExtensions: []string{".js", ".ts"},
		Commands: GateCommands{
			Format: "prettier --check .",
			Lint:   "eslint .",
			Test:   "npm test",
		},
	},
	GateLanguageGeneric: {
		Language: GateLanguageGeneric,
	},
}

// Resolve merges the GateProfile with its language preset and returns
// the concrete ResolvedGate. A nil receiver or an empty/unset Language
// resolves to the "go" preset. Only non-empty fields from the profile
// override the preset; empty fields keep the preset value.
//
// Resolve is a pure function with no I/O.
func (p *GateProfile) Resolve() ResolvedGate {
	// Pick the preset. Nil receiver or empty language defaults to "go".
	preset := builtInPresets[GateLanguageGo]
	if p != nil {
		if p.Language != "" {
			preset = builtInPresets[p.Language]
		}
	}

	resolved := ResolvedGate{
		Image:            preset.Image,
		SourceExtensions: preset.SourceExtensions,
		Format:           preset.Commands.Format,
		Lint:             preset.Commands.Lint,
		Build:            preset.Commands.Build,
		Test:             preset.Commands.Test,
		CodegenCheck:     preset.Commands.CodegenCheck,
	}

	if p == nil {
		return resolved
	}

	// Overlay explicit overrides from the profile.
	if p.Image != "" {
		resolved.Image = p.Image
	}
	if len(p.SourceExtensions) > 0 {
		resolved.SourceExtensions = p.SourceExtensions
	}
	if p.Commands.Format != "" {
		resolved.Format = p.Commands.Format
	}
	if p.Commands.Lint != "" {
		resolved.Lint = p.Commands.Lint
	}
	if p.Commands.Build != "" {
		resolved.Build = p.Commands.Build
	}
	if p.Commands.Test != "" {
		resolved.Test = p.Commands.Test
	}
	if p.Commands.CodegenCheck != "" {
		resolved.CodegenCheck = p.Commands.CodegenCheck
	}

	return resolved
}
