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
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// A 64 GiB Mac at the 0.75 fraction has a 48 GiB budget. A 33 GiB file with no
// GGUF shape estimates at 33 * 1.2 + 0.5 = 40.1 GiB (fallbackEstimate), so one
// such model fits and a second one does not fit next to it (#2080).
const (
	cumulativeTotal  = 64 << 30
	cumulativeBudget = 48 << 30
)

func gib(n float64) uint64 { return uint64(n * (1 << 30)) }

// sizedModel is an admission test Model whose only size signal is status.size.
func sizedModel(size string) *inferencev1alpha1.Model {
	return newAdmissionTestModel("https://model-host.invalid/m.gguf", size)
}

// newCumulativeAgent is an admission agent on a 64 GiB Mac with a fake event
// recorder; isvcs are stored with the status subresource enabled.
func newCumulativeAgent(
	t *testing.T, cfg MetalAgentConfig, isvcs ...*inferencev1alpha1.InferenceService,
) (*MetalAgent, *record.FakeRecorder) {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{})
	for _, isvc := range isvcs {
		b = b.WithObjects(isvc)
	}
	rec := record.NewFakeRecorder(64)
	cfg.K8sClient = b.Build()
	cfg.EventRecorder = rec
	cfg.MemoryProvider = &mockMemoryProvider{totalBytes: cumulativeTotal}
	cfg.MemoryFraction = 0.75
	cfg.ModelStorePath = t.TempDir()
	return NewMetalAgent(cfg), rec
}

func cumulativeISVC(name string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "test-model"},
	}
}

func schedulingStatus(t *testing.T, a *MetalAgent, name string) (string, string) {
	t.Helper()
	got := &inferencev1alpha1.InferenceService{}
	key := types.NamespacedName{Namespace: "default", Name: name}
	if err := a.config.K8sClient.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get InferenceService %s: %v", name, err)
	}
	return got.Status.SchedulingStatus, got.Status.SchedulingMessage
}

// The definition of done: with a 40 GiB model running on a 48 GiB budget, a
// second 40 GiB model is refused, and the status and a Warning event name the
// request, the free budget and the service holding the rest.
func TestCheckMemoryAdmission_RefusesWhenRunningModelsFillBudget(t *testing.T) {
	isvc := cumulativeISVC("second")
	a, rec := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
	a.processes["default/first"] = &ManagedProcess{Name: "first", Namespace: "default", AdmittedBytes: gib(40.1)}

	err := a.checkMemoryAdmission(context.Background(), isvc, sizedModel("33.0 GiB"), 2048, "", "")
	if err == nil {
		t.Fatal("second 40 GiB model admitted next to a running 40 GiB model on a 48 GiB budget")
	}
	status, msg := schedulingStatus(t, a, "second")
	if status != EventReasonInsufficientMemory {
		t.Errorf("SchedulingStatus = %q, want %q", status, EventReasonInsufficientMemory)
	}
	for _, want := range []string{"40.1 GB required", "already committed", "default/first", "free", "48.0 GB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("SchedulingMessage %q does not mention %q", msg, want)
		}
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+EventReasonInsufficientMemory) ||
		!strings.Contains(events[0], "default/first") {
		t.Errorf("events = %q, want one Warning %s naming default/first", events, EventReasonInsufficientMemory)
	}
	if _, held := a.reservedMemory["default/second"]; held {
		t.Error("a refused admission left a reservation behind")
	}
}

// A single model that fits is admitted exactly as before, and its estimate is
// reserved until the start claims or releases it.
func TestCheckMemoryAdmission_ReservesAdmittedEstimate(t *testing.T) {
	isvc := cumulativeISVC("only")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvc)

	if err := a.checkMemoryAdmission(context.Background(), isvc, sizedModel("33.0 GiB"), 2048, "", ""); err != nil {
		t.Fatalf("a single 40 GiB model on a 48 GiB budget was refused: %v", err)
	}
	if got := a.reservedMemory["default/only"]; got < gib(40) || got > gib(40.2) {
		t.Errorf("reservation = %d bytes, want the ~40.1 GiB estimate", got)
	}
	if got := testutil.ToFloat64(memoryCommittedBytes); got != float64(a.reservedMemory["default/only"]) {
		t.Errorf("committed gauge = %v, want the reserved estimate", got)
	}
}

// Two starts in flight for different services cannot both pass: the first
// one's reservation counts against the second.
func TestCheckMemoryAdmission_ReservationCountsAgainstConcurrentStart(t *testing.T) {
	first, second := cumulativeISVC("first"), cumulativeISVC("second")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, first, second)
	model := sizedModel("33.0 GiB")

	if err := a.checkMemoryAdmission(context.Background(), first, model, 2048, "", ""); err != nil {
		t.Fatalf("first admission refused: %v", err)
	}
	if err := a.checkMemoryAdmission(context.Background(), second, model, 2048, "", ""); err == nil {
		t.Fatal("second admission passed while the first one's estimate was still reserved")
	}

	// Once the first start gives its reservation back, the second fits.
	a.releaseAdmission("default/first")
	if err := a.checkMemoryAdmission(context.Background(), second, model, 2048, "", ""); err != nil {
		t.Fatalf("second admission refused after the first reservation was released: %v", err)
	}
}

// A respawn (spec change, health restart) is admitted against everything but
// its own previous record, so it is not refused for overlapping itself.
func TestCheckMemoryAdmission_RespawnExcludesItsOwnRecord(t *testing.T) {
	isvc := cumulativeISVC("svc")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
	a.processes["default/svc"] = &ManagedProcess{Name: "svc", Namespace: "default", AdmittedBytes: gib(40.1)}

	if err := a.checkMemoryAdmission(context.Background(), isvc, sizedModel("33.0 GiB"), 2048, "", ""); err != nil {
		t.Fatalf("respawn refused against its own record: %v", err)
	}
}

// A service that leaves the process table (delete, eviction, scale to zero)
// stops counting against the budget.
func TestCheckMemoryAdmission_StoppedProcessFreesBudget(t *testing.T) {
	isvc := cumulativeISVC("second")
	a, _ := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
	a.executors[runtimeLlamaServer] = stubExecutor{}
	a.registry = NewServiceRegistry(a.config.K8sClient, "", newNopLogger(), "")
	a.processes["default/first"] = &ManagedProcess{
		Name: "first", Namespace: "default", PID: 999990, AdmittedBytes: gib(40.1), Runtime: runtimeLlamaServer,
	}
	model := sizedModel("33.0 GiB")

	if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err == nil {
		t.Fatal("admitted while the first service was running")
	}
	if err := a.deleteProcess(context.Background(), "default/first"); err != nil {
		t.Fatalf("deleteProcess: %v", err)
	}
	if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
		t.Fatalf("still refused after the first service stopped: %v", err)
	}
}

// A Model memoryBudget above the Mac's budget is clamped to it, with a Warning
// event; --allow-model-memory-budget-override restores the old behavior.
func TestCheckMemoryAdmission_ClampsModelBudgetToHost(t *testing.T) {
	// 50 GiB file: 60.5 GiB estimate, under the Model's 100Gi but over the
	// Mac's 48 GiB.
	model := sizedModel("50.0 GiB")
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryBudget: "100Gi"}

	t.Run("clamped by default", func(t *testing.T) {
		isvc := cumulativeISVC("big")
		a, rec := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
		if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err == nil {
			t.Fatal("a Model memoryBudget above the Mac's budget let a 60 GiB model onto a 48 GiB budget")
		}
		events := strings.Join(drainEvents(rec), "\n")
		if !strings.Contains(events, "Warning "+EventReasonMemoryBudgetClamped) {
			t.Errorf("events = %q, want a Warning %s", events, EventReasonMemoryBudgetClamped)
		}
	})

	t.Run("memoryFraction above the agent's is clamped too", func(t *testing.T) {
		isvc := cumulativeISVC("frac")
		a, rec := newCumulativeAgent(t, MetalAgentConfig{}, isvc)
		one := 1.0
		m := sizedModel("50.0 GiB")
		m.Spec.Hardware = &inferencev1alpha1.HardwareSpec{MemoryFraction: &one}
		if err := a.checkMemoryAdmission(context.Background(), isvc, m, 2048, "", ""); err == nil {
			t.Fatal("memoryFraction 1.0 let a 60 GiB model onto a 48 GiB budget")
		}
		if events := strings.Join(drainEvents(rec), "\n"); !strings.Contains(events, EventReasonMemoryBudgetClamped) {
			t.Errorf("events = %q, want %s", events, EventReasonMemoryBudgetClamped)
		}
	})

	t.Run("override flag allows it", func(t *testing.T) {
		isvc := cumulativeISVC("big")
		a, rec := newCumulativeAgent(t, MetalAgentConfig{AllowModelMemoryBudgetOverride: true}, isvc)
		if err := a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
			t.Fatalf("override flag set, Model budget 100Gi, 60 GiB model refused: %v", err)
		}
		if events := strings.Join(drainEvents(rec), "\n"); strings.Contains(events, EventReasonMemoryBudgetClamped) {
			t.Errorf("clamp event emitted with the override flag set: %q", events)
		}
	})
}

// The start path claims the reservation onto the process record when the
// engine starts and gives it back when the start fails.
func TestEnsureProcess_ClaimsOrReleasesAdmission(t *testing.T) {
	t.Run("started engine carries its admitted bytes", func(t *testing.T) {
		f := newRestartFixture(t, "claim")
		delete(f.agent.processes, f.key)
		if err := f.agent.ensureProcess(context.Background(), f.isvc); err != nil {
			t.Fatalf("ensureProcess: %v", err)
		}
		proc := f.agent.processes[f.key]
		if proc == nil || proc.AdmittedBytes == 0 {
			t.Fatalf("process = %+v, want a record carrying its admitted estimate", proc)
		}
		if _, held := f.agent.reservedMemory[f.key]; held {
			t.Error("reservation still held after the engine started")
		}
	})

	t.Run("failed start releases the reservation", func(t *testing.T) {
		f := newRestartFixture(t, "release")
		delete(f.agent.processes, f.key)
		f.exec.startErr = errors.New("spawn failed")
		_ = f.agent.ensureProcess(context.Background(), f.isvc)
		if _, held := f.agent.reservedMemory[f.key]; held {
			t.Error("a failed start left its reservation behind, holding budget forever")
		}
	})
}
