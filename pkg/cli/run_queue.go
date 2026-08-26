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
	"strings"

	sigsyaml "sigs.k8s.io/yaml"
)

// QueueItem is one unit of work: an issue plus the intent the human wrote
// for it. v1 never authors intents (spec non-goal), so IntentPath is
// required.
type QueueItem struct {
	Issue      int32  `json:"issue"`
	IntentPath string `json:"intent"`
	// Repo overrides the queue-level default for this item.
	Repo string `json:"repo,omitempty"`
}

// Queue is the prepared work list handed to `llmkube foreman run`.
type Queue struct {
	Repo  string      `json:"repo,omitempty"`
	Items []QueueItem `json:"items"`
}

// ParseQueue parses queue YAML and resolves each item's repo against the
// queue default. An item with no resolvable repo is rejected: an empty slug
// bases the task branch on a possibly-stale fork HEAD (#1625).
func ParseQueue(data []byte) (Queue, error) {
	var q Queue
	if err := sigsyaml.Unmarshal(data, &q); err != nil {
		return Queue{}, fmt.Errorf("parse queue: %w", err)
	}
	if len(q.Items) == 0 {
		return Queue{}, fmt.Errorf("parse queue: no items")
	}
	for i := range q.Items {
		if q.Items[i].Issue == 0 {
			return Queue{}, fmt.Errorf("parse queue: item %d has no issue", i)
		}
		if q.Items[i].Repo == "" {
			q.Items[i].Repo = q.Repo
		}
		if q.Items[i].Repo == "" {
			return Queue{}, fmt.Errorf("parse queue: item %d (issue %d) has no repo", i, q.Items[i].Issue)
		}
		if q.Items[i].IntentPath == "" {
			return Queue{}, fmt.Errorf("parse queue: issue %d has no intent path", q.Items[i].Issue)
		}
	}
	return q, nil
}

// IntentFor reads the item's intent file.
func (q Queue) IntentFor(item QueueItem) (string, error) {
	b, err := os.ReadFile(item.IntentPath)
	if err != nil {
		return "", fmt.Errorf("read intent for issue %d: %w", item.Issue, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("intent for issue %d is empty", item.Issue)
	}
	return s, nil
}
