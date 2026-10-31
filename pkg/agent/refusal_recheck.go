/*
Copyright 2026.

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

package agent

// A refused InferenceService is re-checked on a backoff (#2329). Its cause
// usually clears outside the InferenceService (a Secret is labeled, memory is
// freed, a Model is corrected), and the watcher delivers only a service whose
// resourceVersion moved. The refusal's own status write moves it once; the
// re-check that follows writes the same status, which the API server treats
// as a no-op, so without a scheduled re-check the service was never looked
// at again until someone edited it.

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The re-check backoff: the first re-check runs refusalRecheckInitial after
// the refusal, each one that refuses again for the same reason doubles the
// wait, up to refusalRecheckMax. A fixed cause is picked up within a minute,
// and a refusal that holds costs one evaluation a minute. Variables so tests
// can shorten them.
var (
	refusalRecheckInitial = 5 * time.Second
	refusalRecheckMax     = time.Minute
)

// refusalRecord is the refusal last recorded for one InferenceService.
type refusalRecord struct {
	reason     string
	message    string
	generation int64
	wait       time.Duration
}

// refusalRechecks remembers each refused InferenceService's last refusal, so
// a re-check that refuses again for the same reason emits no new Warning
// Event and waits longer before the next one. The zero value is ready to use.
// A record ends when the service starts (or is stopped or blocked rather than
// refused), when it is deleted or its process torn down, and when the agent
// restarts.
type refusalRechecks struct {
	mu      sync.Mutex
	records map[string]refusalRecord
}

// note records a refusal of key and returns whether it differs from the last
// one recorded (a first refusal, or a new reason or message), which is when
// the Warning Event is due, and how long to wait before re-checking it. The
// wait starts at refusalRecheckInitial and doubles with each repeat of the
// same refusal; a new reason or message, or a new spec generation, starts it
// over.
func (r *refusalRechecks) note(key, reason, message string, generation int64) (changed bool, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records == nil {
		r.records = make(map[string]refusalRecord)
	}
	rec, ok := r.records[key]
	switch {
	case !ok || rec.reason != reason || rec.message != message:
		changed = true
		rec = refusalRecord{reason: reason, message: message, generation: generation, wait: refusalRecheckInitial}
	case rec.generation != generation:
		rec.generation = generation
		rec.wait = refusalRecheckInitial
	default:
		rec.wait = min(2*rec.wait, refusalRecheckMax)
	}
	r.records[key] = rec
	return changed, rec.wait
}

// forget ends key's record, if it has one.
func (r *refusalRechecks) forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, key)
}

// refusalStatus is the scheduling status an InferenceService carried before
// a refusal overwrote it.
type refusalStatus struct {
	reason  string
	message string
}

// statusBeforeRefusal captures isvc's scheduling status, for publishRefusal.
func statusBeforeRefusal(isvc *inferencev1alpha1.InferenceService) refusalStatus {
	return refusalStatus{reason: isvc.Status.SchedulingStatus, message: isvc.Status.SchedulingMessage}
}

// publishRefusal finishes a refusal whose reason and message the caller has
// just set on isvc's status (each refusal site assigns its own reason
// constant, which refusal_reasons_scan_test checks). It writes the status
// unless it already read that way (prev), schedules the re-check, and
// reports whether the Warning Event is due: only when the refusal differs
// from the last one recorded for the service, so a re-check that refuses
// again for the same reason adds no Event.
func (a *MetalAgent) publishRefusal(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, prev refusalStatus,
) (emitEvent bool) {
	reason, message := isvc.Status.SchedulingStatus, isvc.Status.SchedulingMessage
	if prev.reason != reason || prev.message != message {
		if err := a.config.K8sClient.Status().Update(ctx, isvc); err != nil {
			a.logger.Warnw("failed to update InferenceService status", "error", err)
		}
	}
	key := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}.String()
	changed, wait := a.refusals.note(key, reason, message, isvc.Generation)
	a.requeueAt(key, a.clock().Add(wait))
	return changed
}
