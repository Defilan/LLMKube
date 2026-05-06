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

// Package catalog owns the curated LLMKube model catalog: the embedded YAML,
// types, and loader. Lives outside pkg/cli so subpackages (notably the TUI)
// can consume it without an import cycle.
package catalog

import (
	_ "embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

//go:embed catalog.yaml
var CatalogYAML []byte

type Catalog struct {
	Version string           `yaml:"version"`
	Models  map[string]Model `yaml:"models"`
}

type Model struct {
	Name         string       `yaml:"name"`
	Description  string       `yaml:"description"`
	Size         string       `yaml:"size"`
	Quantization string       `yaml:"quantization"`
	Source       string       `yaml:"source"`
	ContextSize  int          `yaml:"context_size"`
	GPULayers    int32        `yaml:"gpu_layers"`
	UseCases     []string     `yaml:"use_cases"`
	Resources    ResourceSpec `yaml:"resources"`
	VRAMEstimate string       `yaml:"vram_estimate"`
	Tags         []string     `yaml:"tags"`
	Homepage     string       `yaml:"homepage"`
	Notes        string       `yaml:"notes,omitempty"`
}

type ResourceSpec struct {
	CPU       string `yaml:"cpu"`
	Memory    string `yaml:"memory"`
	GPUMemory string `yaml:"gpu_memory"`
}

var instance *Catalog

// Load returns the embedded catalog. Cached after first parse so repeated
// callers (e.g., the TUI on startup + each invocation of `llmkube catalog
// list`) don't re-parse the YAML.
func Load() (*Catalog, error) {
	if instance != nil {
		return instance, nil
	}
	var c Catalog
	if err := yaml.Unmarshal(CatalogYAML, &c); err != nil {
		return nil, fmt.Errorf("failed to parse catalog: %w", err)
	}
	instance = &c
	return instance, nil
}
