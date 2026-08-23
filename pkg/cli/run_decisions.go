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

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"sigs.k8s.io/yaml"
)

// Decision is one parked judgment call. Files rather than a CRD: inspectable,
// diffable, no schema guessed at before the shape is known, and a future
// Command Center can read the same directory without either side committing
// to an API.
type Decision struct {
	Issue    int32             `json:"issue"`
	Workload string            `json:"workload,omitempty"`
	Stage    string            `json:"stage,omitempty"`
	Kind     string            `json:"kind"`
	Opened   time.Time         `json:"opened,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	Evidence map[string]string `json:"evidence,omitempty"`
	Options  []string          `json:"options,omitempty"`
	// Answer is empty until a human answers it.
	Answer string `json:"answer,omitempty"`
}

func decisionPath(dir string, issue int32, kind string) string {
	return filepath.Join(dir, fmt.Sprintf("%d-%s.yaml", issue, kind))
}

// ParkDecision writes a decision and returns its path. Parking is how the
// loop declines to block: the caller records this and moves to the next item.
func ParkDecision(dir string, d Decision) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create decisions dir: %w", err)
	}
	if d.Opened.IsZero() {
		d.Opened = time.Now().UTC()
	}
	b, err := yaml.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshal decision: %w", err)
	}
	p := decisionPath(dir, d.Issue, d.Kind)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return "", fmt.Errorf("write decision: %w", err)
	}
	return p, nil
}

// ListDecisions reads every decision in dir, oldest first. A missing
// directory is an empty list, not an error: no parked decisions is the
// normal state.
func ListDecisions(dir string) ([]Decision, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read decisions dir: %w", err)
	}
	var out []Decision
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var d Decision
		if err := yaml.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Opened.Before(out[j].Opened) })
	return out, nil
}

// AnswerDecision records a human's answer. The answer must be one of the
// options the decision offered, so a typo cannot silently become an action.
func AnswerDecision(dir string, issue int32, kind, answer string) error {
	p := decisionPath(dir, issue, kind)
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("read decision: %w", err)
	}
	var d Decision
	if err := yaml.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("parse decision: %w", err)
	}
	ok := len(d.Options) == 0
	for _, o := range d.Options {
		if o == answer {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("answer %q is not one of %v", answer, d.Options)
	}
	d.Answer = answer
	out, err := yaml.Marshal(d)
	if err != nil {
		return fmt.Errorf("marshal decision: %w", err)
	}
	return os.WriteFile(p, out, 0o600)
}
