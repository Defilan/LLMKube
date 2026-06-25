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

import (
	"reflect"
	"testing"
)

func TestGateProfileResolve(t *testing.T) {
	tests := []struct {
		name    string
		profile *GateProfile
		want    ResolvedGate
	}{
		{
			name:    "nil profile resolves to go preset",
			profile: nil,
			want: ResolvedGate{
				Image:            "golang:1.26",
				SourceExtensions: []string{".go"},
				Format:           "gofmt -l .",
				Lint:             "golangci-lint run ./...",
				Build:            "go build ./...",
				Test:             "go test ./...",
				CodegenCheck:     "make manifests && make chart-crds",
			},
		},
		{
			name:    "empty language resolves to go preset",
			profile: &GateProfile{},
			want: ResolvedGate{
				Image:            "golang:1.26",
				SourceExtensions: []string{".go"},
				Format:           "gofmt -l .",
				Lint:             "golangci-lint run ./...",
				Build:            "go build ./...",
				Test:             "go test ./...",
				CodegenCheck:     "make manifests && make chart-crds",
			},
		},
		{
			name: "go preset test command",
			profile: &GateProfile{
				Language: GateLanguageGo,
			},
			want: ResolvedGate{
				Image:            "golang:1.26",
				SourceExtensions: []string{".go"},
				Format:           "gofmt -l .",
				Lint:             "golangci-lint run ./...",
				Build:            "go build ./...",
				Test:             "go test ./...",
				CodegenCheck:     "make manifests && make chart-crds",
			},
		},
		{
			name: "python preset test command",
			profile: &GateProfile{
				Language: GateLanguagePython,
			},
			want: ResolvedGate{
				Image:            "python:3.13",
				SourceExtensions: []string{".py"},
				Format:           "ruff format --check .",
				Lint:             "ruff check .",
				Build:            "python -m compileall .",
				Test:             "pytest -q",
				CodegenCheck:     "",
			},
		},
		{
			name: "rust preset test command",
			profile: &GateProfile{
				Language: GateLanguageRust,
			},
			want: ResolvedGate{
				Image:            "rust:1",
				SourceExtensions: []string{".rs"},
				Format:           "cargo fmt --check",
				Lint:             "cargo clippy",
				Build:            "cargo build",
				Test:             "cargo test",
				CodegenCheck:     "",
			},
		},
		{
			name: "node preset test command",
			profile: &GateProfile{
				Language: GateLanguageNode,
			},
			want: ResolvedGate{
				Image:            "node:22",
				SourceExtensions: []string{".js", ".ts"},
				Format:           "prettier --check .",
				Lint:             "eslint .",
				Build:            "",
				Test:             "npm test",
				CodegenCheck:     "",
			},
		},
		{
			name: "generic preset is empty",
			profile: &GateProfile{
				Language: GateLanguageGeneric,
			},
			want: ResolvedGate{
				Image:            "",
				SourceExtensions: nil,
				Format:           "",
				Lint:             "",
				Build:            "",
				Test:             "",
				CodegenCheck:     "",
			},
		},
		{
			name: "explicit commands override preset",
			profile: &GateProfile{
				Language: GateLanguageGo,
				Commands: GateCommands{
					Test: "custom test",
				},
			},
			want: ResolvedGate{
				Image:            "golang:1.26",
				SourceExtensions: []string{".go"},
				Format:           "gofmt -l .",
				Lint:             "golangci-lint run ./...",
				Build:            "go build ./...",
				Test:             "custom test",
				CodegenCheck:     "make manifests && make chart-crds",
			},
		},
		{
			name: "explicit image overrides preset",
			profile: &GateProfile{
				Language: GateLanguageGo,
				Image:    "golang:1.25",
			},
			want: ResolvedGate{
				Image:            "golang:1.25",
				SourceExtensions: []string{".go"},
				Format:           "gofmt -l .",
				Lint:             "golangci-lint run ./...",
				Build:            "go build ./...",
				Test:             "go test ./...",
				CodegenCheck:     "make manifests && make chart-crds",
			},
		},
		{
			name: "explicit source extensions override preset",
			profile: &GateProfile{
				Language:         GateLanguageGo,
				SourceExtensions: []string{".go", ".proto"},
			},
			want: ResolvedGate{
				Image:            "golang:1.26",
				SourceExtensions: []string{".go", ".proto"},
				Format:           "gofmt -l .",
				Lint:             "golangci-lint run ./...",
				Build:            "go build ./...",
				Test:             "go test ./...",
				CodegenCheck:     "make manifests && make chart-crds",
			},
		},
		{
			name: "all explicit overrides on generic preset",
			profile: &GateProfile{
				Language:         GateLanguageGeneric,
				Image:            "myimage:latest",
				SourceExtensions: []string{".rs"},
				Commands: GateCommands{
					Format:       "cargo fmt --check",
					Lint:         "cargo clippy",
					Build:        "cargo build",
					Test:         "cargo test",
					CodegenCheck: "",
				},
			},
			want: ResolvedGate{
				Image:            "myimage:latest",
				SourceExtensions: []string{".rs"},
				Format:           "cargo fmt --check",
				Lint:             "cargo clippy",
				Build:            "cargo build",
				Test:             "cargo test",
				CodegenCheck:     "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.profile.Resolve()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
