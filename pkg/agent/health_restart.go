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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// DefaultHealthFailureThreshold is how many consecutive failed health probes
// make the agent stop an engine and start a new one when
// MetalAgentConfig.HealthFailureThreshold is zero. The first failure already
// withdraws the endpoint (#662); the restart waits for the rest, so one slow
// probe does not reload a large model.
const DefaultHealthFailureThreshold = 3

// Health restart backoff, after the kubelet's container restart backoff: the
// first restart runs at once, each later one waits twice as long as the one
// before (10s, 20s, 40s, ...) up to healthRestartMaxBackoff, and the delay
// resets once a restarted engine has been up for healthRestartBackoffReset.
const (
	healthRestartInitialBackoff = 10 * time.Second
	healthRestartMaxBackoff     = 5 * time.Minute
	healthRestartBackoffReset   = 10 * time.Minute
)

// Kubernetes event reasons for health restarts.
const (
	EventReasonHealthRestart = "HealthRestart"
	EventReasonRestartFailed = "RestartFailed"
)

// restartBackoff is the health-restart state of one InferenceService. It
// outlives the engines that replace each other, so the delay keeps growing
// while they keep failing. Guarded by MetalAgent.mu.
type restartBackoff struct {
	// delay is how long the next restart waits.
	delay time.Duration
	// pending is set while the service is owed a restart: its engine was
	// stopped for failing health checks, or the last restart attempt left
	// no engine running.
	pending bool
	// nextAt is the earliest time the pending restart may run.
	nextAt time.Time
	// timer wakes the event loop at nextAt.
	timer *time.Timer
}

// healthFailureThreshold resolves config.HealthFailureThreshold.
func (a *MetalAgent) healthFailureThreshold() int {
	if a.config.HealthFailureThreshold > 0 {
		return a.config.HealthFailureThreshold
	}
	return DefaultHealthFailureThreshold
}

// nextRestartDelay returns the delay that follows d: the initial backoff
// after none, then double the last one, capped at healthRestartMaxBackoff.
func nextRestartDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return healthRestartInitialBackoff
	}
	return min(2*d, healthRestartMaxBackoff)
}

// restartStateLocked returns key's restart state, creating it if needed.
// Must be called with a.mu held.
func (a *MetalAgent) restartStateLocked(key string) *restartBackoff {
	if a.restarts == nil {
		a.restarts = make(map[string]*restartBackoff)
	}
	st := a.restarts[key]
	if st == nil {
		st = &restartBackoff{}
		a.restarts[key] = st
	}
	return st
}

// scheduleRestartLocked marks key as owed a restart once the current delay
// has passed (and not before notBefore), grows the delay for the restart
// after it, and arms a wake-up for the event loop. It returns when the
// restart may run. Must be called with a.mu held.
func (a *MetalAgent) scheduleRestartLocked(key string, notBefore time.Time) time.Time {
	st := a.restartStateLocked(key)
	now := a.now()
	st.pending = true
	st.nextAt = now.Add(st.delay)
	if st.nextAt.Before(notBefore) {
		st.nextAt = notBefore
	}
	st.delay = nextRestartDelay(st.delay)
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	if wait := st.nextAt.Sub(now); wait > 0 {
		st.timer = time.AfterFunc(wait, a.wakeRestarts)
	} else {
		a.wakeRestarts()
	}
	return st.nextAt
}

// cancelRestartLocked forgets key's restart state, a pending restart
// included. Must be called with a.mu held.
func (a *MetalAgent) cancelRestartLocked(key string) {
	st := a.restarts[key]
	if st == nil {
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	delete(a.restarts, key)
}

func (a *MetalAgent) cancelRestart(key string) {
	a.mu.Lock()
	a.cancelRestartLocked(key)
	a.mu.Unlock()
}

// resetRestartBackoffLocked forgets key's restart backoff once its engine has
// been up for healthRestartBackoffReset and is answering health checks, so a
// failure long after a crash loop restarts at once again. Must be called with
// a.mu held.
func (a *MetalAgent) resetRestartBackoffLocked(key string, p *ManagedProcess) {
	st := a.restarts[key]
	if st == nil || st.pending || a.now().Sub(p.StartedAt) < healthRestartBackoffReset {
		return
	}
	a.cancelRestartLocked(key)
}

// wakeRestarts asks the event loop to run due restarts. It never blocks: one
// queued wake-up covers every restart that is due when the loop gets to it.
func (a *MetalAgent) wakeRestarts() {
	select {
	case a.restartWake <- struct{}{}:
	default:
	}
}

// restartUnhealthy stops proc, which has failed `failures` consecutive health
// probes, and schedules its replacement on the service's restart backoff. It
// runs on the health monitor's goroutine; the replacement starts later on the
// event loop (runDueRestarts), never before the stop has returned.
//
// proc is removed from a.processes and its key held in a.starting under one
// lock, so a watch event can neither start a second engine while this one is
// stopping nor stop it a second time. A proc that was replaced, removed or
// recovered since the probe, or whose key has a start in flight, is left
// alone; the next probe looks again.
func (a *MetalAgent) restartUnhealthy(ctx context.Context, key string, proc *ManagedProcess, failures int) {
	a.mu.Lock()
	if a.processes[key] != proc || proc.Healthy || a.starting[key] {
		a.mu.Unlock()
		return
	}
	delete(a.processes, key)
	managedProcesses.Set(float64(len(a.processes)))
	a.starting[key] = true
	// Held from before the stop: a delete handled while the engine stops
	// cancels it, and that is how the check below sees the delete.
	st := a.restartStateLocked(key)
	a.mu.Unlock()

	a.logger.Warnw("stopping unhealthy process to restart it",
		"namespace", proc.Namespace, "name", proc.Name, "pid", proc.PID, "consecutiveFailures", failures)
	var notBefore time.Time
	if err := a.stopManagedProcess(ctx, proc); err != nil {
		// A stop that failed, or that had to SIGKILL, may not have released
		// the engine's memory yet: give it the initial backoff before a new
		// engine wires another copy of the weights.
		a.logger.Warnw("unhealthy process did not stop cleanly",
			"namespace", proc.Namespace, "name", proc.Name, "pid", proc.PID, "error", err)
		notBefore = a.now().Add(healthRestartInitialBackoff)
	}

	a.mu.Lock()
	delete(a.starting, key)
	if a.restarts[key] != st {
		a.mu.Unlock()
		a.logger.Infow("service was torn down while its unhealthy process stopped; not restarting it",
			"namespace", proc.Namespace, "name", proc.Name)
		return
	}
	restartAt := a.scheduleRestartLocked(key, notBefore)
	restartIn := max(restartAt.Sub(a.now()), 0)
	a.mu.Unlock()

	a.logger.Warnw("restart scheduled for unhealthy process",
		"namespace", proc.Namespace, "name", proc.Name, "restartAt", restartAt, "restartIn", restartIn)
	a.emitInferenceEvent(ctx, proc, corev1.EventTypeWarning, EventReasonHealthRestart,
		"Engine failed %d consecutive health checks and was stopped; restarting in %s", failures, restartIn)
}

// runDueRestarts runs every pending health restart whose backoff has expired.
// It runs on the event loop, so a restart's start is serialized with watch
// events: a delete or scale-to-zero handled before it cancels the restart,
// and one that arrives during it is handled after it, against the new engine.
func (a *MetalAgent) runDueRestarts(ctx context.Context) {
	now := a.now()
	a.mu.Lock()
	var due []string
	for key, st := range a.restarts {
		if st.pending && !now.Before(st.nextAt) {
			due = append(due, key)
		}
	}
	a.mu.Unlock()
	slices.Sort(due)

	for _, key := range due {
		if ctx.Err() != nil {
			return
		}
		a.runRestart(ctx, key)
	}
}

// runRestart starts the engine of a service owed a health restart, unless
// the restart was cancelled or another start already brought an engine up.
// A service that is gone, being deleted or stopped drops the restart; an
// attempt that leaves no engine running is retried on the backoff schedule.
func (a *MetalAgent) runRestart(ctx context.Context, key string) {
	a.mu.Lock()
	st := a.restarts[key]
	switch {
	case st == nil || !st.pending || a.starting[key] || a.now().Before(st.nextAt):
		// Cancelled or rescheduled, or the health monitor is still stopping
		// the old engine and reschedules once it has.
		a.mu.Unlock()
		return
	case a.processes[key] != nil:
		// A watch event started the service in the meantime.
		st.pending = false
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	namespace, name := parseKey(key)
	isvc := &inferencev1alpha1.InferenceService{}
	err := a.config.K8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, isvc)
	switch {
	case apierrors.IsNotFound(err):
		a.cancelRestart(key)
		return
	case err != nil:
		err = fmt.Errorf("failed to fetch InferenceService: %w", err)
	case isvc.DeletionTimestamp != nil || isvcStopped(isvc):
		a.cancelRestart(key)
		return
	default:
		processRestarts.WithLabelValues(name, namespace).Inc()
		a.logger.Infow("restarting inference service after failed health checks",
			"namespace", namespace, "name", name)
		err = a.ensureProcess(ctx, isvc)
	}

	a.mu.Lock()
	st = a.restarts[key]
	if st == nil || a.processes[key] != nil {
		// Cancelled during the attempt, or the attempt brought an engine up.
		if st != nil {
			st.pending = false
		}
		a.mu.Unlock()
		return
	}
	retryAt := a.scheduleRestartLocked(key, time.Time{})
	retryIn := retryAt.Sub(a.now())
	a.mu.Unlock()

	if err == nil {
		err = errors.New("no engine is running after the restart")
	}
	a.logger.Warnw("health restart failed; retrying",
		"namespace", namespace, "name", name, "retryAt", retryAt, "retryIn", retryIn, "error", err)
	// Only the first line: a start error can carry the tail of the engine
	// log, which belongs in the agent log, not in an Event.
	reason, _, _ := strings.Cut(err.Error(), "\n")
	a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: namespace, Name: name}, corev1.EventTypeWarning,
		EventReasonRestartFailed, "Restart after failed health checks did not bring the engine back: %s; retrying in %s",
		reason, retryIn)
}
