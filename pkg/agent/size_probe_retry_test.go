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

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"

	"github.com/defilantech/llmkube/internal/safehttp"
)

// sizeProbeModelBytes is the size every test origin reports for the model.
const sizeProbeModelBytes = 1 << 30

// probeOrigin is a model origin (an https server or a MinIO endpoint) the
// test takes down and brings back on one fixed loopback address. While down
// nothing listens there, so a probe fails with "connection refused"; while up
// it answers every HEAD with status (200 carries the model size).
type probeOrigin struct {
	t    *testing.T
	addr string
	mu   sync.Mutex
	code int
	srv  *httptest.Server
}

// setStatus sets the status the origin answers with while up.
func (o *probeOrigin) setStatus(code int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.code = code
}

func (o *probeOrigin) status() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.code
}

// newProbeOrigin reserves a loopback address and leaves the origin down.
func newProbeOrigin(t *testing.T) *probeOrigin {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	o := &probeOrigin{t: t, addr: ln.Addr().String()}
	o.setStatus(http.StatusOK)
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}
	t.Cleanup(func() {
		if o.srv != nil {
			o.srv.Close()
		}
	})
	return o
}

func (o *probeOrigin) url() string { return "http://" + o.addr }

// up starts serving on the reserved address.
func (o *probeOrigin) up() {
	o.t.Helper()
	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		o.t.Fatalf("listen again on %s: %v", o.addr, err)
	}
	o.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		status := o.status()
		if status == http.StatusOK {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", sizeProbeModelBytes))
		}
		w.WriteHeader(status)
	}))
	o.srv.Listener = ln
	o.srv.Start()
}

// sizeProbeFixture is an agent that enforces memory admission and records
// starts with a recordingExecutor, over an InferenceService "svc" whose
// unsized Model's source is origin: an http(s) URL, or (s3) an s3:// object
// whose credential Secret points at origin. Its watcher records requeues and
// its clock is the returned fakeClock.
func sizeProbeFixture(
	t *testing.T, kind, origin string,
) (*MetalAgent, *recordingExecutor, *record.FakeRecorder, *fakeClock) {
	t.Helper()
	model := refusalModel()
	model.Spec.Source = origin + "/m.gguf"
	objs := []runtimeObject{refusalISVC("svc"), model}
	if kind == "s3" {
		model.Spec.Source = s3ObjectSource
		model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
		objs = append(objs, s3CredentialSecret("default", "creds", origin, modelSourceCredentialsLabels()))
	}
	a, ex, rec := refusalFixtureWithInterceptor(t, MetalAgentConfig{
		AllowedDownloadHosts: []string{"127.0.0.1"},
		MemoryProvider:       &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
		MemoryFraction:       0.75,
	}, nil, objs...)
	a.memoryCheckWarnOnly = false
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, "default", newNopLogger())
	clk := &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	a.now = clk.now
	a.watcher.now = clk.now
	return a, ex, rec, clk
}

// memoryCheckEvents returns the MemoryCheckFailed Events rec holds.
func memoryCheckEvents(rec *record.FakeRecorder) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, EventReasonMemoryCheckFailed) {
			out = append(out, e)
		}
	}
	return out
}

// A size probe that fails because the origin cannot be reached right now (no
// listener, a 5xx, a 429) is not a refusal (#2326): no status write, no
// MemoryCheckFailed Event, nothing started, and the service is queued for
// the next poll, which starts it once the probe works.
func TestEnsureProcess_SizeProbeTransientFailureRetriedQuietly(t *testing.T) {
	failures := []struct {
		name   string
		status int // 0: nothing listening
		want   string
	}{
		{"connection refused", 0, "connection refused"},
		{"503", http.StatusServiceUnavailable, "503"},
		{"429", http.StatusTooManyRequests, "429"},
	}
	for _, kind := range []string{"https", "s3"} {
		for _, fc := range failures {
			t.Run(kind+"/"+fc.name, func(t *testing.T) {
				origin := newProbeOrigin(t)
				if fc.status != 0 {
					origin.setStatus(fc.status)
					origin.up()
				}
				a, ex, rec, _ := sizeProbeFixture(t, kind, origin.url())
				before := freshISVC(t, a).ResourceVersion

				err := a.ensureProcess(context.Background(), freshISVC(t, a))
				if err == nil || !strings.Contains(err.Error(), fc.want) ||
					strings.Contains(err.Error(), "memory admission check failed") {
					t.Fatalf("ensureProcess error = %v, want a plain retryable error naming %q", err, fc.want)
				}
				cur := freshISVC(t, a)
				if cur.Status.SchedulingStatus != "" || cur.ResourceVersion != before {
					t.Errorf("status = %q, resourceVersion %s -> %s; want no status write for a transient failure",
						cur.Status.SchedulingStatus, before, cur.ResourceVersion)
				}
				if got := memoryCheckEvents(rec); len(got) != 0 {
					t.Errorf("events = %q, want no %s", got, EventReasonMemoryCheckFailed)
				}
				if ex.starts != 0 {
					t.Errorf("engine started %d times while the size was unknown, want 0", ex.starts)
				}
				if !a.watcher.takeRequeue("default/svc") {
					t.Error("the deferred service was not queued for the next poll")
				}

				if fc.status == 0 {
					origin.up()
				} else {
					origin.setStatus(http.StatusOK)
				}
				if err := a.ensureProcess(context.Background(), freshISVC(t, a)); err != nil {
					t.Fatalf("ensureProcess once the origin answers: %v", err)
				}
				if ex.starts != 1 {
					t.Errorf("engine started %d times once the size was known, want 1", ex.starts)
				}
				if reason, _ := isvcStatus(t, a); reason != "" {
					t.Errorf("SchedulingStatus = %q after the start, want empty", reason)
				}
				if got := memoryCheckEvents(rec); len(got) != 0 {
					t.Errorf("events = %q, want no %s", got, EventReasonMemoryCheckFailed)
				}
			})
		}
	}
}

// A definitive answer (403, 404) still refuses at once with MemoryCheckFailed
// and its Warning Event, and is not queued: retrying it would only hide a
// configuration the operator has to fix.
func TestEnsureProcess_SizeProbeDefinitiveFailureRefused(t *testing.T) {
	for _, kind := range []string{"https", "s3"} {
		for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
			t.Run(fmt.Sprintf("%s/%d", kind, status), func(t *testing.T) {
				origin := newProbeOrigin(t)
				origin.setStatus(status)
				origin.up()
				a, ex, rec, _ := sizeProbeFixture(t, kind, origin.url())

				err := a.ensureProcess(context.Background(), freshISVC(t, a))
				if err == nil || !strings.Contains(err.Error(), "memory admission check failed") {
					t.Fatalf("ensureProcess error = %v, want a MemoryCheckFailed refusal", err)
				}
				reason, msg := isvcStatus(t, a)
				if reason != EventReasonMemoryCheckFailed || !strings.Contains(msg, fmt.Sprint(status)) {
					t.Errorf("status = %q %q, want %s naming %d", reason, msg, EventReasonMemoryCheckFailed, status)
				}
				if got := memoryCheckEvents(rec); len(got) != 1 || !strings.HasPrefix(got[0], "Warning ") {
					t.Errorf("events = %q, want one Warning %s", got, EventReasonMemoryCheckFailed)
				}
				if ex.starts != 0 {
					t.Errorf("engine started %d times, want 0", ex.starts)
				}
				if a.watcher.takeRequeue("default/svc") {
					t.Error("a definitive refusal was queued for an immediate retry, want only the refusal re-check")
				}
			})
		}
	}
}

// A probe that keeps failing transiently is retried quietly only for
// sizeProbeRetryWindow; after that it is refused with MemoryCheckFailed and
// its Warning Event, so an origin that stays unreachable is visible.
func TestEnsureProcess_SizeProbeSustainedTransientFailureRefused(t *testing.T) {
	for _, kind := range []string{"https", "s3"} {
		t.Run(kind, func(t *testing.T) {
			origin := newProbeOrigin(t)
			a, ex, rec, clk := sizeProbeFixture(t, kind, origin.url())
			start := clk.t

			for _, at := range []time.Duration{0, time.Minute, 2 * time.Minute, sizeProbeRetryWindow - time.Second} {
				clk.t = start.Add(at)
				err := a.ensureProcess(context.Background(), freshISVC(t, a))
				if err == nil || strings.Contains(err.Error(), "memory admission check failed") {
					t.Fatalf("at +%s: ensureProcess error = %v, want a quiet retry inside the window", at, err)
				}
			}
			if got := memoryCheckEvents(rec); len(got) != 0 {
				t.Fatalf("events inside the window = %q, want none", got)
			}

			clk.t = start.Add(sizeProbeRetryWindow)
			err := a.ensureProcess(context.Background(), freshISVC(t, a))
			if err == nil || !strings.Contains(err.Error(), "memory admission check failed") {
				t.Fatalf("after the window: ensureProcess error = %v, want a MemoryCheckFailed refusal", err)
			}
			reason, msg := isvcStatus(t, a)
			if reason != EventReasonMemoryCheckFailed || !strings.Contains(msg, "connection refused") ||
				!strings.Contains(msg, "has kept failing for 3m0s") {
				t.Errorf("status = %q %q, want %s naming the cause and how long it failed",
					reason, msg, EventReasonMemoryCheckFailed)
			}
			if got := memoryCheckEvents(rec); len(got) != 1 || !strings.HasPrefix(got[0], "Warning ") {
				t.Errorf("events = %q, want one Warning %s", got, EventReasonMemoryCheckFailed)
			}
			if ex.starts != 0 {
				t.Errorf("engine started %d times, want 0", ex.starts)
			}
		})
	}
}

// A streak of transient failures ends when the InferenceService is deleted
// and when the probe succeeds: a later failure starts a new window rather
// than being refused at once for an outage that has ended.
func TestSizeProbeStreakEndsOnDeleteAndSuccess(t *testing.T) {
	late := sizeProbeRetryWindow - 10*time.Second
	after := sizeProbeRetryWindow + 5*time.Second

	t.Run("delete", func(t *testing.T) {
		origin := newProbeOrigin(t)
		a, _, rec, clk := sizeProbeFixture(t, "https", origin.url())
		start := clk.t
		for _, at := range []time.Duration{0, late} {
			clk.t = start.Add(at)
			_ = a.ensureProcess(context.Background(), freshISVC(t, a))
		}
		if err := a.handleEvent(context.Background(), InferenceServiceEvent{
			Type: EventTypeDeleted, InferenceService: refusalISVC("svc"),
		}); err != nil {
			t.Fatalf("delete: %v", err)
		}
		clk.t = start.Add(after)
		err := a.ensureProcess(context.Background(), freshISVC(t, a))
		if err == nil || strings.Contains(err.Error(), "memory admission check failed") {
			t.Errorf("first failure after a delete: ensureProcess error = %v, want a quiet retry", err)
		}
		if got := memoryCheckEvents(rec); len(got) != 0 {
			t.Errorf("events = %q, want none", got)
		}
	})

	t.Run("success", func(t *testing.T) {
		origin := newProbeOrigin(t)
		a, _, rec, clk := sizeProbeFixture(t, "https", origin.url())
		start := clk.t
		isvc := freshISVC(t, a)
		model := refusalModel()
		model.Spec.Source = origin.url() + "/m.gguf"
		admit := func(at time.Duration) error {
			clk.t = start.Add(at)
			return a.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", "")
		}
		for _, at := range []time.Duration{0, late} {
			if err := admit(at); err == nil {
				t.Fatalf("admission at +%s with the origin down = nil, want a deferral", at)
			}
		}
		origin.up()
		if err := admit(late + 5*time.Second); err != nil {
			t.Fatalf("admission with the origin up: %v", err)
		}
		origin.srv.Close()
		err := admit(after)
		if err == nil || strings.Contains(err.Error(), "memory admission check failed") {
			t.Errorf("first failure after a success: error = %v, want a quiet retry", err)
		}
		if got := memoryCheckEvents(rec); len(got) != 0 {
			t.Errorf("events = %q, want none", got)
		}
	})
}

// In warn mode an incomplete check proceeds, transient or not: the legacy
// behavior that mode exists for is unchanged.
func TestCheckMemoryAdmission_WarnModeProceedsOnTransientProbeFailure(t *testing.T) {
	origin := newProbeOrigin(t)
	a, _, _, _ := sizeProbeFixture(t, "https", origin.url())
	a.memoryCheckWarnOnly = true
	model := refusalModel()
	model.Spec.Source = origin.url() + "/m.gguf"
	if err := a.checkMemoryAdmission(context.Background(), freshISVC(t, a), model, 2048, "", ""); err != nil {
		t.Fatalf("warn mode with a transient probe failure: %v, want it to proceed", err)
	}
}

// isTransientError against errors the real producers return wherever a
// test can provoke them: remoteModelSize through the guarded client, and the
// API server error a sourceSecretRef read sees.
func TestIsTransientProbeError(t *testing.T) {
	probe := func(t *testing.T, ctx context.Context, client *http.Client, url string) error {
		t.Helper()
		_, err := remoteModelSize(ctx, client, url)
		if err == nil {
			t.Fatalf("remoteModelSize(%s) = nil error, want a failure", url)
		}
		return err
	}
	statusServer := func(t *testing.T, status int) string {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		return srv.URL + "/m.gguf"
	}

	cases := []struct {
		name string
		err  func(t *testing.T) error
		want bool
	}{
		{"connection refused", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), newProbeOrigin(t).url()+"/m.gguf")
		}, true},
		{"probe deadline", func(t *testing.T) error {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(release) })
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			return probe(t, ctx, testProbeClient(), srv.URL+"/m.gguf")
		}, true},
		{"connection closed before a response", func(t *testing.T) error {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}))
			t.Cleanup(srv.Close)
			return probe(t, context.Background(), testProbeClient(), srv.URL+"/m.gguf")
		}, true},
		{"500", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusInternalServerError))
		}, true},
		{"502", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusBadGateway))
		}, true},
		{"503", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusServiceUnavailable))
		}, true},
		{"429", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusTooManyRequests))
		}, true},
		{"400", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusBadRequest))
		}, false},
		{"401", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusUnauthorized))
		}, false},
		{"403", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusForbidden))
		}, false},
		{"404", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusNotFound))
		}, false},
		{"no Content-Length", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), statusServer(t, http.StatusOK))
		}, false},
		{"host not in --allowed-download-hosts", func(t *testing.T) error {
			blocked := safehttp.NewClient(safehttp.ParseAllowlist(nil), 0, downloadHostsFlag)
			return probe(t, context.Background(), blocked, statusServer(t, http.StatusOK))
		}, false},
		{"invalid URL", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), "http://[::1")
		}, false},
		{"unsupported scheme", func(t *testing.T) error {
			return probe(t, context.Background(), testProbeClient(), "ftp://127.0.0.1/m.gguf")
		}, false},
		{"untrusted TLS certificate", func(t *testing.T) error {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)
			return probe(t, context.Background(), testProbeClient(), srv.URL+"/m.gguf")
		}, false},
		{"DNS temporary failure", func(*testing.T) error {
			return fmt.Errorf("dial: %w", &net.DNSError{Err: "server misbehaving", Name: "minio.lan", IsTemporary: true})
		}, true},
		{"DNS name not found", func(*testing.T) error {
			return fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "minio.lan", IsNotFound: true})
		}, false},
		{"API server unavailable reading the Secret", func(*testing.T) error {
			return fmt.Errorf("read sourceSecretRef: %w", apierrors.NewServiceUnavailable("etcd leader changed"))
		}, true},
		{"API server unreachable reading the Secret", func(*testing.T) error {
			return &sourceSecretReadError{Namespace: "default", Name: "creds", Err: errors.New("http2: client connection lost")}
		}, true},
		{"Secret not found", func(*testing.T) error {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "creds")
		}, false},
		{"Secret not marked", func(*testing.T) error {
			return &SourceSecretNotAllowedError{Namespace: "default", Name: "creds", Why: "not marked"}
		}, false},
		{"plain error", func(*testing.T) error { return errors.New("AWS_ENDPOINT_URL not set in secret") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err(t)
			if got := isTransientError(err); got != tc.want {
				t.Errorf("isTransientError(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}

// The probe errors the transient classification keys on keep their text:
// "bad status: 404 Not Found" is what operators read in the refusal.
func TestRemoteModelSize_StatusErrorText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := remoteModelSize(context.Background(), testProbeClient(), srv.URL+"/m.gguf")
	if err == nil || err.Error() != "bad status: 404 Not Found" {
		t.Fatalf("remoteModelSize error = %v, want %q", err, "bad status: 404 Not Found")
	}
}

// Requeue makes the next poll deliver an unchanged service once, as Updated,
// so a start the agent deferred without writing to it is retried; a requeue
// for a service the poll no longer lists is dropped.
func TestPoll_RequeueRedeliversUnchangedServiceOnce(t *testing.T) {
	f := newModelLookupFixture(t)
	key := "default/" + lookupISVCName
	seen := map[string]string{}

	if got := eventSummary(f.pollOnce(t, seen)); len(got) != 1 || got[0] != "CREATED "+key {
		t.Fatalf("first poll events = %v, want CREATED %s", got, key)
	}
	if got := eventSummary(f.pollOnce(t, seen)); len(got) != 0 {
		t.Fatalf("poll of an unchanged service = %v, want no events", got)
	}

	f.watcher.Requeue(key)
	if got := eventSummary(f.pollOnce(t, seen)); len(got) != 1 || got[0] != "UPDATED "+key {
		t.Fatalf("poll after Requeue = %v, want UPDATED %s", got, key)
	}
	if got := eventSummary(f.pollOnce(t, seen)); len(got) != 0 {
		t.Fatalf("second poll after Requeue = %v, want the requeue delivered once", got)
	}

	f.watcher.Requeue("default/gone")
	_ = f.pollOnce(t, seen)
	if f.watcher.takeRequeue("default/gone") {
		t.Error("a requeue for a service the poll did not list was kept")
	}
}
