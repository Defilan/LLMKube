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

package agent

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// digestMismatchEntry is one Model's remembered SHA256 mismatch, keyed (by
// the map holding it) on the Model's namespace/name, and self-describing the
// source+sha256 it was observed against so a spec edit invalidates it.
type digestMismatchEntry struct {
	source string
	sha256 string
	err    *ModelDigestMismatchError
}

// digestMismatchMemo remembers, per Model, the most recent SHA256 mismatch
// this agent observed for it, so reconcileProcess's pre-flight check
// (checkModelDigestMemo) can refuse a persistently-mismatching Model without
// re-downloading it every reconcile. Without this, the mismatch is only
// discovered inside StartProcess, which runs after checkMemoryAdmission:
// admission's success path clears Status.SchedulingStatus (a Status().Update)
// and the subsequent refusal writes it back (another Status().Update); each
// write bumps the InferenceService's resourceVersion, the watcher sees
// UPDATED, and the next reconcile re-downloads the entire model (tens of GB)
// only to fail the identical check again — a download storm.
//
// Each entry records the source+sha256 it was observed against, so editing
// the Model (a corrected source or a corrected sha256) invalidates the memo
// automatically the next time it is checked, with no explicit "clear on spec
// change" step needed. Model deletion is detected and cleared at its own call
// site (the Get in reconcileProcess returning NotFound), which also needs no
// bookkeeping here. InferenceService deletion is different: a persistently
// refused InferenceService never appears in MetalAgent.processes (refuseStart
// returns before a process is ever stored there), so deleteProcess has no
// other way to learn which Model it referenced; isvcRefs exists solely to
// answer that question so forgetISVC can clear the right entry.
type digestMismatchMemo struct {
	mu       sync.Mutex
	entries  map[types.NamespacedName]digestMismatchEntry
	isvcRefs map[string]types.NamespacedName // InferenceService key -> Model
}

func newDigestMismatchMemo() *digestMismatchMemo {
	return &digestMismatchMemo{
		entries:  make(map[types.NamespacedName]digestMismatchEntry),
		isvcRefs: make(map[string]types.NamespacedName),
	}
}

// noteRef records that isvcKey currently resolves to model, so a later
// forgetISVC(isvcKey) knows which entry (if any) to clear. Called on every
// reconcile that successfully fetches a Model, not only a refused one, since
// an InferenceService can start referencing a different Model over its
// lifetime. A nil receiver (a MetalAgent built as a struct literal rather
// than via NewMetalAgent, as several unit tests do) is a silent no-op.
func (m *digestMismatchMemo) noteRef(isvcKey string, model types.NamespacedName) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isvcRefs[isvcKey] = model
}

// forgetISVC drops isvcKey's remembered Model reference and clears that
// Model's memo entry. Called when the InferenceService is deleted. A Model
// another still-live InferenceService also points at simply re-populates its
// entry on its own next failed reconcile; this is not a shared refcount. A
// nil receiver is a silent no-op (see noteRef).
func (m *digestMismatchMemo) forgetISVC(isvcKey string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	model, ok := m.isvcRefs[isvcKey]
	if !ok {
		return
	}
	delete(m.isvcRefs, isvcKey)
	delete(m.entries, model)
}

// record stores a digest mismatch for model, keyed by the source+sha256 it
// was observed against. A nil receiver is a silent no-op (see noteRef).
func (m *digestMismatchMemo) record(model types.NamespacedName, source, sha256 string, err *ModelDigestMismatchError) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[model] = digestMismatchEntry{source: source, sha256: sha256, err: err}
}

// check returns the remembered mismatch for model if one is on file and it
// still matches the model's current source+sha256. A stale entry (the source
// or sha256 changed since it was recorded) is dropped and nil is returned, so
// a spec edit retries the download normally instead of being blocked by a
// memo of a different, already-fixed problem. A nil receiver reports no
// memo, exactly like an empty one (see noteRef).
func (m *digestMismatchMemo) check(model types.NamespacedName, source, sha256 string) *ModelDigestMismatchError {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[model]
	if !ok {
		return nil
	}
	if entry.source != source || entry.sha256 != sha256 {
		delete(m.entries, model)
		return nil
	}
	return entry.err
}

// clear drops any remembered mismatch for model directly. Called when the
// Model itself is deleted (detected at the Get call site in
// reconcileProcess, which already knows the Model's namespace/name even when
// the Get fails). A nil receiver is a silent no-op (see noteRef).
func (m *digestMismatchMemo) clear(model types.NamespacedName) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, model)
}
