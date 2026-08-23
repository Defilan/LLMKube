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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// checkDecisionKind rejects a kind that would steer the path out of dir. Task 7
// takes the kind from the command line, so this is reachable input, and it is
// not a privilege boundary so much as a way to avoid silently destroying an
// unrelated file: a kind with a ".." in it makes ParkDecision write outside the
// decisions directory, and makes AnswerDecision rewrite whatever YAML it lands
// on as a Decision, dropping every key it did not recognise.
func checkDecisionKind(kind string) error {
	if strings.ContainsAny(kind, `/\`) {
		return fmt.Errorf("decision kind %q must not contain a path separator", kind)
	}
	if kind == "." || kind == ".." {
		return fmt.Errorf("decision kind %q must be a name, not a path segment", kind)
	}
	return nil
}

func decisionPath(dir string, issue int32, kind string) string {
	return filepath.Join(dir, fmt.Sprintf("%d-%s.yaml", issue, kind))
}

// writeDecisionFile replaces p with b atomically. A decision file is
// human-owned: a truncating write that is interrupted by a crash or a full
// disk leaves a half-written answer behind, which is exactly the damage the
// park guard then has to refuse to touch. Staging in the same directory and
// renaming means a reader sees either the whole old file or the whole new one.
func writeDecisionFile(p string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".decision-*.tmp")
	if err != nil {
		return fmt.Errorf("write decision %s: %w", p, err)
	}
	tmp := f.Name()
	// A no-op once the rename has happened.
	defer func() { _ = os.Remove(tmp) }()
	// Pinned here rather than left to how the temp file happened to be made.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("write decision %s: %w", p, err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("write decision %s: %w", p, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write decision %s: %w", p, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("write decision %s: %w", p, err)
	}
	return nil
}

// ParkDecision writes a decision and returns its path. Parking is how the
// loop declines to block: the caller records this and moves to the next item.
func ParkDecision(dir string, d Decision) (string, error) {
	if err := checkDecisionKind(d.Kind); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create decisions dir: %w", err)
	}
	p := decisionPath(dir, d.Issue, d.Kind)
	// An answer already on disk is the human's, and nothing consumes it before
	// the loop re-parks the same item, so writing over it would silently
	// destroy typed input. The item genuinely is parked either way, so leaving
	// it alone is not an error for the caller. An unanswered decision is still
	// refreshed with the current reason and evidence.
	//
	// This narrows the window, it does not close it: a human answering at the
	// same instant as a park can still lose the answer. That is acceptable for
	// a single-process loop and is not what this guard claims to prevent.
	//
	// A decision that cannot be read or parsed is never overwritten. The
	// likeliest way one gets truncated is an interrupted write, and the answer
	// may well still be sitting in it in plain text. Refusing until a human
	// looks at the file beats destroying what is left of it.
	existing, err := os.ReadFile(p)
	switch {
	case err == nil:
		var prev Decision
		if err := yaml.Unmarshal(existing, &prev); err != nil {
			return "", fmt.Errorf("refusing to overwrite unparseable decision %s: %w", p, err)
		}
		if prev.Answer != "" {
			return p, nil
		}
	case !os.IsNotExist(err):
		return "", fmt.Errorf("refusing to overwrite unreadable decision %s: %w", p, err)
	}
	if d.Opened.IsZero() {
		d.Opened = time.Now().UTC()
	}
	b, err := yaml.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshal decision: %w", err)
	}
	if err := writeDecisionFile(p, b); err != nil {
		return "", err
	}
	return p, nil
}

// ListDecisions reads every decision in dir, oldest first. A missing
// directory is an empty list, not an error: no parked decisions is the
// normal state.
//
// One unreadable file does not hide the rest. The decisions that parsed come
// back along with an error naming the ones that did not, because the caller is
// the command whose whole job is showing a human what is parked, and showing
// nothing at all is the worst answer it can give.
func ListDecisions(dir string) ([]Decision, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read decisions dir: %w", err)
	}
	var out []Decision
	var bad []error
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			bad = append(bad, fmt.Errorf("read %s: %w", e.Name(), err))
			continue
		}
		var d Decision
		if err := yaml.Unmarshal(b, &d); err != nil {
			bad = append(bad, fmt.Errorf("parse %s: %w", e.Name(), err))
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Opened.Before(out[j].Opened) })
	return out, errors.Join(bad...)
}

// AnswerDecision records a human's answer. The answer must be one of the
// options the decision offered, so a typo cannot silently become an action.
//
// The answer is trimmed, and an answer that is empty once trimmed is rejected:
// returning nil having recorded nothing reads as success to the human while
// every consumer still sees the decision as unanswered, and a stored answer of
// pure whitespace is worse still, since it satisfies the park guard forever and
// the decision can never be refreshed again.
func AnswerDecision(dir string, issue int32, kind, answer string) error {
	if err := checkDecisionKind(kind); err != nil {
		return err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return fmt.Errorf("decision answer must not be empty")
	}
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
	return writeDecisionFile(p, out)
}
