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
	"context"
	"encoding/json"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/audit"
)

// BaselineFor returns the median wall-clock duration of an agent's
// cleanly-successful issue-fix tasks, read from the durable audit-record
// ConfigMaps the controller already writes. With no history it returns
// DefaultBaseline: a fleet with no track record should not have its first
// run declared stalled.
//
// There is deliberately no recency window in v1: every matching record in
// the namespace counts, so a six-month-old run weighs exactly as much as
// yesterday's. Audit CMs outlive their tasks on purpose and their retention
// is operator-tunable (disableable with --audit-retention=0), so the window
// is really whatever the reaper leaves behind. A real half-life belongs
// here eventually; it is not v1.
//
// The List is intentionally NOT paginated. Scoped to one namespace and the
// audit label, the result is small in practice. If audit volume ever grows
// past the apiserver's --max-objects-per-list (default 500) this single
// call would silently truncate, and a truncated sample shifts the median
// rather than erroring; if that ever happens, switch to a Continue loop
// with a per-page limit. Same trade-off the sibling audit.Sweep documents.
func BaselineFor(ctx context.Context, c client.Client, namespace, agent string) (time.Duration, error) {
	var list corev1.ConfigMapList
	if err := c.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingLabels{audit.AuditLabel: "true"},
	); err != nil {
		return 0, err
	}
	var secs []float64
	for i := range list.Items {
		raw, ok := list.Items[i].Data["audit.json"]
		if !ok {
			continue
		}
		var rec audit.Record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue // a malformed record is not a reason to fail the run
		}
		if rec.Agent == nil || rec.Agent.Name != agent {
			continue
		}
		// audit.TaskRef.Kind is a plain string on the wire, so the constant is
		// converted rather than the comparison reshaped: the record carries
		// whatever the writer serialised, not a typed kind.
		if rec.Task.Kind != string(foremanv1alpha1.AgenticTaskKindIssueFix) {
			continue
		}
		// SucceededOnTarget is this exact predicate, precomputed by
		// audit.BuildRecord from AgenticTask.SucceededOnTarget(). It is
		// stricter than comparing Verdict against GO/GATE-PASS because it
		// also requires Phase == Succeeded, so a GO verdict on a task that
		// never finished does not pollute the baseline. Always serialized
		// (no omitempty), so an absent field reads as false.
		if !rec.SucceededOnTarget {
			continue
		}
		if rec.ElapsedSec > 0 {
			secs = append(secs, rec.ElapsedSec)
		}
	}
	if len(secs) == 0 {
		return DefaultBaseline, nil
	}
	sort.Float64s(secs)
	// A true median: average the middle two on an even count. Taking the
	// upper-middle element instead makes a two-record history return the
	// slower run, and at DefaultStallFactor a single pathological outlier
	// would then buy every later run an enormous stall budget.
	n := len(secs)
	med := secs[n/2]
	if n%2 == 0 {
		med = (secs[n/2-1] + secs[n/2]) / 2
	}
	return time.Duration(med * float64(time.Second)), nil
}
