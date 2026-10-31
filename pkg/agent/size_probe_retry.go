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

// A remote size probe that fails for a reason that may clear on its own is
// retried quietly instead of refused (#2326). On macOS the first LAN
// connection a freshly started agent makes can fail with "no route to host"
// while Local Network Privacy evaluates the process, so without this the
// first remote Model after every agent restart got a MemoryCheckFailed
// Warning Event that read as a real failure, although the next attempt a few
// seconds later started it. Nothing starts while the size is unknown either
// way: a deferred start is a start that has not happened yet.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sizeProbeRetryWindow bounds the quiet retries: once the remote size probe
// for one InferenceService has failed transiently for this long, the failure
// is refused with MemoryCheckFailed and its Warning Event like any other
// incomplete memory check, so an origin that stays unreachable is visible
// instead of retried in silence forever. The causes it exists for clear well
// inside it: the Local Network Privacy evaluation takes seconds, and an
// object store or Wi-Fi link that restarts is back within a minute or two.
// It matches the 3 minutes after which the controller treats a silent agent
// as gone (DefaultAgentHeartbeatTimeout), the existing line between a blip
// and an outage. It also caps what the retries cost: each attempt runs on
// the agent's single event loop and can hold it for the probe's 10 second
// timeout when the origin drops packets, which at the 5 second poll interval
// is at most about a dozen attempts.
const sizeProbeRetryWindow = 3 * time.Minute

// transientSizeProbeError wraps the "cannot determine model size" error of an
// estimate whose only missing input is a remote size probe that failed for a
// reason that may clear on its own (isTransientError).
type transientSizeProbeError struct {
	err error
}

func (e *transientSizeProbeError) Error() string { return e.err.Error() }

func (e *transientSizeProbeError) Unwrap() error { return e.err }

// transientErrnos are the socket errors that say the origin could not be
// reached right now, not that the request was wrong.
var transientErrnos = []syscall.Errno{
	syscall.ECONNREFUSED,
	syscall.ECONNRESET,
	syscall.ECONNABORTED,
	syscall.EHOSTUNREACH,
	syscall.EHOSTDOWN,
	syscall.ENETUNREACH,
	syscall.ENETDOWN,
	syscall.ETIMEDOUT,
	syscall.EPIPE,
}

// isTransientError reports whether an error from a remote size probe or from
// an API call in the start path may clear on its own. It is an allowlist:
// only the causes named here are retried, and every other error (a 401, 403
// or 404, any other 4xx, a response without a usable size, an invalid URL or
// endpoint, a host the SSRF guard or --allowed-download-hosts refuses, a TLS
// verification failure, a host that does not exist, a refused or incomplete
// sourceSecretRef Secret) stays a refusal, because retrying it would only
// hide a configuration the operator has to fix.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	var status *sizeProbeStatusError
	if errors.As(err, &status) {
		return status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= http.StatusInternalServerError
	}
	// The probe's own timeout, or the agent shutting down mid-probe.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// A DNS answer is decided here: a temporary resolver failure is retried,
	// a name that does not exist is not.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTemporary || dnsErr.IsTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	for _, errno := range transientErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	// The connection closed before a response arrived.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// A sourceSecretRef read that was not refused (the s3 probe reads the
	// Secret first) is retried, as checkSourceSecret retries it.
	var readErr *sourceSecretReadError
	if errors.As(err, &readErr) {
		return true
	}
	// An API server that is briefly unavailable.
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) || apierrors.IsUnexpectedServerError(err)
}

// sizeProbeStreak is one InferenceService's run of consecutive transient size
// probe failures against one source.
type sizeProbeStreak struct {
	source   string
	first    time.Time
	last     time.Time
	failures int
}

// sizeProbeRetries tracks each InferenceService's streak of transient size
// probe failures, so the quiet retries are bounded by sizeProbeRetryWindow.
// The zero value is ready to use. A streak ends when the probe succeeds, when
// it fails definitively, when the InferenceService is deleted or its process
// torn down, when the Model's source changes, and when no attempt has failed
// for a whole window (nothing retried in between, so a later failure is a new
// incident rather than the same one still going).
type sizeProbeRetries struct {
	mu      sync.Mutex
	streaks map[string]sizeProbeStreak
}

// note records a transient failure for key's probe of source at now and
// returns how long the streak has lasted and how many failures it holds.
func (r *sizeProbeRetries) note(key, source string, now time.Time) (failingFor time.Duration, failures int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streaks == nil {
		r.streaks = make(map[string]sizeProbeStreak)
	}
	s, ok := r.streaks[key]
	if !ok || s.source != source || now.Sub(s.last) > sizeProbeRetryWindow {
		s = sizeProbeStreak{source: source, first: now}
	}
	s.last = now
	s.failures++
	r.streaks[key] = s
	return now.Sub(s.first), s.failures
}

// forget ends key's streak, if it has one.
func (r *sizeProbeRetries) forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.streaks, key)
}

// failModelEstimate handles an estimateModelMemory error. A transient probe
// failure (enforce mode only) inside sizeProbeRetryWindow is logged, the
// InferenceService is queued for the next poll, and a plain error is
// returned: no status write and no Event, the same rule checkSourceSecret
// applies to a transient Secret read. Every other estimate error, and a
// transient one that outlasted the window, fails the memory check as before.
func (a *MetalAgent) failModelEstimate(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	estimateErr error,
) error {
	key := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}.String()
	reason := fmt.Sprintf("memory estimation failed: %v", estimateErr)
	var transient *transientSizeProbeError
	if !errors.As(estimateErr, &transient) || a.memoryCheckWarnOnly {
		a.sizeProbes.forget(key)
		return a.failMemoryCheck(ctx, isvc, reason)
	}
	failingFor, failures := a.sizeProbes.note(key, model.Spec.Source, a.clock())
	if failingFor >= sizeProbeRetryWindow {
		return a.failMemoryCheck(ctx, isvc, fmt.Sprintf("%s (the remote size probe has failed for %s, %d attempts)",
			reason, failingFor.Round(time.Second), failures))
	}
	a.logger.Warnw("remote model size probe failed transiently, retrying on the next poll",
		"namespace", isvc.Namespace, "name", isvc.Name, "reason", reason,
		"failures", failures, "failingFor", failingFor.Round(time.Second).String(),
		"refuseAfter", sizeProbeRetryWindow.String())
	a.requeue(key)
	return fmt.Errorf("memory admission deferred until the model size is known: %s", reason)
}

// retryIfTransient queues isvc for the next poll when err may clear on its own
// (isTransientError) and returns err unchanged. It is for a start that fails
// before anything is written to the InferenceService or started, so a retry
// costs only the API reads that failed.
func (a *MetalAgent) retryIfTransient(isvc *inferencev1alpha1.InferenceService, err error) error {
	if isTransientError(err) {
		a.requeue(types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}.String())
	}
	return err
}

// requeue asks the watcher to deliver key again on its next poll even though
// nothing about the InferenceService changed. A start that failed without a
// status write would otherwise not be retried: the watcher only delivers a
// service whose resourceVersion moved.
func (a *MetalAgent) requeue(key string) {
	if a.watcher != nil {
		a.watcher.Requeue(key)
	}
}

// clock returns a.now(), or the wall clock for an agent built as a struct
// literal.
func (a *MetalAgent) clock() time.Time {
	if a.now == nil {
		return time.Now()
	}
	return a.now()
}
