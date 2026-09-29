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
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestDigestMismatchMemo_RecordAndCheck(t *testing.T) {
	m := newDigestMismatchMemo()
	model := types.NamespacedName{Namespace: "default", Name: "m"}
	derr := &ModelDigestMismatchError{Path: "p", Expected: "aaaa", Computed: "bbbb"}

	if got := m.check(model, "src", "aaaa"); got != nil {
		t.Fatalf("check on an empty memo = %v, want nil", got)
	}

	m.record(model, "src", "aaaa", derr)
	if got := m.check(model, "src", "aaaa"); got != derr {
		t.Errorf("check after record = %v, want %v", got, derr)
	}
}

func TestDigestMismatchMemo_SourceOrSHA256ChangeInvalidates(t *testing.T) {
	model := types.NamespacedName{Namespace: "default", Name: "m"}
	derr := &ModelDigestMismatchError{Path: "p", Expected: "aaaa", Computed: "bbbb"}

	t.Run("sha256 change", func(t *testing.T) {
		m := newDigestMismatchMemo()
		m.record(model, "src", "aaaa", derr)
		if got := m.check(model, "src", "cccc"); got != nil {
			t.Errorf("check with a changed sha256 = %v, want nil", got)
		}
		// The stale entry must be deleted outright, not merely skipped: a
		// second check with the ORIGINAL sha256 must also miss.
		if got := m.check(model, "src", "aaaa"); got != nil {
			t.Errorf("check with the original sha256 after a change = %v, want nil "+
				"(stale entry must be dropped, not just skipped)", got)
		}
	})

	t.Run("source change", func(t *testing.T) {
		m := newDigestMismatchMemo()
		m.record(model, "src", "aaaa", derr)
		if got := m.check(model, "other-src", "aaaa"); got != nil {
			t.Errorf("check with a changed source = %v, want nil", got)
		}
	})
}

func TestDigestMismatchMemo_Clear(t *testing.T) {
	m := newDigestMismatchMemo()
	model := types.NamespacedName{Namespace: "default", Name: "m"}
	derr := &ModelDigestMismatchError{Path: "p", Expected: "aaaa", Computed: "bbbb"}

	m.record(model, "src", "aaaa", derr)
	m.clear(model)
	if got := m.check(model, "src", "aaaa"); got != nil {
		t.Errorf("check after clear = %v, want nil", got)
	}
}

func TestDigestMismatchMemo_ForgetISVCClearsItsModel(t *testing.T) {
	m := newDigestMismatchMemo()
	model := types.NamespacedName{Namespace: "default", Name: "m"}
	derr := &ModelDigestMismatchError{Path: "p", Expected: "aaaa", Computed: "bbbb"}

	m.record(model, "src", "aaaa", derr)
	m.noteRef("default/svc", model)
	m.forgetISVC("default/svc")

	if got := m.check(model, "src", "aaaa"); got != nil {
		t.Errorf("check after forgetISVC = %v, want nil", got)
	}
	// forgetISVC on an isvc key with no recorded ref is a harmless no-op.
	m.forgetISVC("default/never-seen")
}

// A nil *digestMismatchMemo (a MetalAgent built as a struct literal rather
// than via NewMetalAgent, as several unit tests in this package do) must not
// panic: every method is a silent no-op / reports "no memo".
func TestDigestMismatchMemo_NilReceiverIsSafe(t *testing.T) {
	var m *digestMismatchMemo
	model := types.NamespacedName{Namespace: "default", Name: "m"}
	derr := &ModelDigestMismatchError{Path: "p", Expected: "aaaa", Computed: "bbbb"}

	m.record(model, "src", "aaaa", derr)
	m.noteRef("default/svc", model)
	m.forgetISVC("default/svc")
	m.clear(model)
	if got := m.check(model, "src", "aaaa"); got != nil {
		t.Errorf("check on a nil memo = %v, want nil", got)
	}
}
