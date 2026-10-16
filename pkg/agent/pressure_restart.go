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
	"maps"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Restarts of services evicted under memory pressure (#2078). A blocked
// service stays down until pressure is Normal and the available memory has
// room for it (pickRestartable); its restart then goes through the event
// loop's restart queue (queueRestart in health_restart.go), which re-checks
// the service before starting it and is cancelled by a delete, scale-to-zero,
// eviction or shutdown.
//
// Every service that fits is queued on the same Normal sample: the headroom
// is summed across them, and a queued service keeps its RSS reserved until
// its engine runs, so later samples cannot pick against memory it is about
// to take. The loop may start them back to back, later than they were
// queued, so each start re-checks the pressure level (holdQueuedEvictedLocked)
// and puts the service back in pressureBlocked if pressure is no longer
// Normal. Queueing one service per sample would re-measure headroom between
// starts, but the reservation already covers what queued services will take,
// and it would hold every further service back a sample interval for no
// added safety.
//
// A queued start that fails for another reason (a refusal, an engine that
// does not come up) is retried on the restart queue's own backoff, and the
// service's RSS stays reserved meanwhile.

// Eviction restart backoff: a service evicted again within
// evictedRestartBackoffReset of being restarted after an eviction waits
// evictedRestartInitialWait before its next restart, doubling on each further
// quick re-eviction up to evictedRestartMaxWait. A service that stays up for
// evictedRestartBackoffReset starts from no wait again.
const (
	evictedRestartInitialWait  = time.Minute
	evictedRestartMaxWait      = 30 * time.Minute
	evictedRestartBackoffReset = 10 * time.Minute
)

// evictedService is what the agent remembers about a service it evicted
// under memory pressure, while the service is blocked or its restart queued.
type evictedService struct {
	// rss is the process's resident memory when it was evicted: about what
	// restarting it will take back. 0 when the watchdog had no RSS sample
	// for it, in which case it reserves no headroom.
	rss uint64
	// notBefore is the earliest time a restart may be picked (the eviction
	// restart backoff). Zero means at once.
	notBefore time.Time
}

// evictedRestart is one blocked service picked for a restart.
type evictedRestart struct {
	key string
	svc evictedService
}

// evictionBackoff is the eviction restart backoff of one service. It outlives
// the block so a quick re-eviction can be told apart. Guarded by
// MetalAgent.mu.
type evictionBackoff struct {
	// restartedAt is when the service's engine last came back after an
	// eviction.
	restartedAt time.Time
	// wait is the delay applied after the last quick re-eviction.
	wait time.Duration
}

// pickRestartable returns, in key order, the blocked services that may restart
// now: past their notBefore, and with each one's eviction-time RSS, added to
// reserved and to that of the services picked before it, leaving at least
// floor bytes of the available memory. One that does not fit stays blocked,
// and a smaller one after it may still fit. Admission alone cannot make this
// call: it compares one model's estimate with a fraction of total memory and
// never looks at what is available, so a service evicted for pressure that
// has not gone away would pass it and push the host straight back into
// pressure.
func pickRestartable(
	blocked map[string]evictedService, now time.Time, available, floor, reserved uint64,
) []string {
	var picked []string
	for _, key := range slices.Sorted(maps.Keys(blocked)) {
		svc := blocked[key]
		if now.Before(svc.notBefore) {
			continue
		}
		need := reserved + svc.rss
		if need > available || available-need < floor {
			continue
		}
		reserved = need
		picked = append(picked, key)
	}
	return picked
}

// restartFloor is the available memory a restart must leave free: the
// watchdog's Warning threshold of total memory, so taking back the evicted
// RSS is not expected to put the host back at Warning. Without a watchdog
// (unit tests) there is no threshold, and the floor is 0.
func (a *MetalAgent) restartFloor(total uint64) uint64 {
	if a.config.WatchdogConfig == nil {
		return 0
	}
	return uint64(a.config.WatchdogConfig.WarningThreshold * float64(total))
}

// restartCandidatesLocked returns the blocked services that may restart now
// (pickRestartable). Services whose restart is already queued keep their RSS
// reserved: available memory does not show them until their engine is up.
// It also forgets queued restarts that were cancelled and expired backoff
// records. The services stay blocked until queueEvictedRestarts queues them.
// The caller must hold a.mu.
func (a *MetalAgent) restartCandidatesLocked(stats MemoryStats, transition bool) []evictedRestart {
	now := a.now()
	var reserved uint64
	for key, svc := range a.pressureRestarting {
		if a.restarts[key] == nil && a.processes[key] == nil && !a.starting[key] {
			// Cancelled (deleted, stopped, shut down) or dropped as gone.
			delete(a.pressureRestarting, key)
			continue
		}
		reserved += svc.rss
	}
	for key, rec := range a.evictionBackoff {
		_, blocked := a.pressureBlocked[key]
		_, queued := a.pressureRestarting[key]
		if !blocked && !queued && now.Sub(rec.restartedAt) >= evictedRestartBackoffReset {
			delete(a.evictionBackoff, key)
		}
	}
	if len(a.pressureBlocked) == 0 {
		return nil
	}

	floor := a.restartFloor(stats.TotalMemory)
	keys := pickRestartable(a.pressureBlocked, now, stats.AvailableMemory, floor, reserved)
	log := a.logger.Debugw
	if transition {
		log = a.logger.Infow
	}
	log("memory pressure normal; restarting evicted services that fit the headroom",
		"restarting", len(keys), "stillBlocked", len(a.pressureBlocked)-len(keys),
		"available", formatMemory(stats.AvailableMemory), "reserved", formatMemory(reserved),
		"floor", formatMemory(floor))
	items := make([]evictedRestart, 0, len(keys))
	for _, key := range keys {
		items = append(items, evictedRestart{key: key, svc: a.pressureBlocked[key]})
	}
	return items
}

// evictedVerdict is what queueEvictedRestarts does with a picked service.
type evictedVerdict int

const (
	evictedQueue evictedVerdict = iota // queue its restart
	evictedKeep                        // leave it blocked; a later Normal sample retries it
	evictedDrop                        // forget it; it must not be restarted
)

// queueEvictedRestarts lifts the block of each picked service this agent
// still claims and queues its restart on the event loop. It runs on the
// watchdog goroutine without a.mu held; the starts happen later, one at a
// time, in runRestart.
func (a *MetalAgent) queueEvictedRestarts(ctx context.Context, items []evictedRestart) {
	for _, item := range items {
		switch a.evictedRestartVerdict(ctx, item.key) {
		case evictedKeep:
			continue
		case evictedDrop:
			a.forgetEvicted(item.key)
			continue
		}
		a.mu.Lock()
		queued := a.queueEvictedLocked(item)
		a.mu.Unlock()
		if queued {
			a.logger.Infow("restart queued", "key", item.key, "reason", "memory pressure cleared")
		}
	}
}

// queueEvictedLocked moves item from pressureBlocked to pressureRestarting and
// queues its restart, all under one lock, and reports whether it did. A key
// no longer blocked is skipped: a DELETED event (the service was deleted or
// is no longer claimed by this agent) handled since it was picked has
// forgotten it, and a restart queued now would start a service this agent
// does not own. A key that is starting, or already owed a restart, is left
// blocked for a later sample: queueing it now would mark a restart pending
// under a start in progress. The caller must hold a.mu.
func (a *MetalAgent) queueEvictedLocked(item evictedRestart) bool {
	if _, blocked := a.pressureBlocked[item.key]; !blocked {
		return false
	}
	if st := a.restarts[item.key]; a.starting[item.key] || (st != nil && st.pending) {
		return false
	}
	delete(a.pressureBlocked, item.key)
	a.pressureRestarting[item.key] = item.svc
	a.queueRestartLocked(item.key, time.Time{})
	return true
}

// evictedRestartVerdict reads the InferenceService of a picked key and decides
// what queueEvictedRestarts does with it. A service deleted or being deleted,
// or no longer claimed by this agent, is dropped; one that cannot be read, or
// whose claim cannot be checked, is kept blocked for a later retry. A service
// scaled to zero or suspended is queued too: runRestart applies its stop.
func (a *MetalAgent) evictedRestartVerdict(ctx context.Context, key string) evictedVerdict {
	if a.config.K8sClient == nil {
		return evictedDrop
	}
	namespace, name := parseKey(key)
	isvc := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, isvc); err != nil {
		if apierrors.IsNotFound(err) {
			a.logger.Infow("evicted service was deleted while blocked; not restarting it", "key", key)
			return evictedDrop
		}
		a.logger.Warnw("failed to fetch evicted service for restart; keeping it blocked", "key", key, "error", err)
		return evictedKeep
	}
	if isvc.DeletionTimestamp != nil {
		a.logger.Infow("evicted service is being deleted; not restarting it", "key", key)
		return evictedDrop
	}
	claimed, err := a.claimsForRestart(ctx, isvc)
	if err != nil {
		a.logger.Warnw("could not tell whether this agent still claims an evicted service; keeping it blocked",
			"key", key, "error", err)
		return evictedKeep
	}
	if !claimed {
		a.logger.Infow("evicted service is no longer claimed by this agent; not restarting it", "key", key)
		return evictedDrop
	}
	return evictedQueue
}

// claimsForRestart reports whether this agent still claims isvc (the watcher's
// shouldWatch: allowlist, a Metal Model). An evicted service it no longer
// claims must not be started here, whether or not the watcher has reported
// it gone yet. The error is for a claim check that can fail; the caller keeps
// the service blocked on one.
//
//nolint:unparam // the error result is for a shouldWatch that returns (bool, error)
func (a *MetalAgent) claimsForRestart(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (bool, error) {
	if a.watcher == nil {
		return true, nil
	}
	return a.watcher.shouldWatch(ctx, isvc), nil
}

// holdQueuedEvictedLocked runs when key is about to start. If key is the
// queued restart of an evicted service and pressure is no longer Normal, it
// puts the service back in pressureBlocked and cancels the restart, so the
// start is refused as blocked and the restart queue neither retries it nor
// reports it failed. It is retried once pressure is Normal and there is room
// again. The caller must hold a.mu.
func (a *MetalAgent) holdQueuedEvictedLocked(key string) {
	svc, queued := a.pressureRestarting[key]
	if !queued || a.lastPressureLevel == MemoryPressureNormal {
		return
	}
	delete(a.pressureRestarting, key)
	a.pressureBlocked[key] = svc
	a.cancelRestartLocked(key)
	a.logger.Infow("memory pressure rose before a queued restart of an evicted service; blocked again",
		"key", key, "level", a.lastPressureLevel.String())
}

// nextEvictedRestartLocked returns when key, being evicted now, may be
// restarted: at once, unless it was restarted after an earlier eviction less
// than evictedRestartBackoffReset ago, in which case the wait doubles from
// evictedRestartInitialWait up to evictedRestartMaxWait. The caller must hold
// a.mu.
func (a *MetalAgent) nextEvictedRestartLocked(key string) time.Time {
	now := a.now()
	rec := a.evictionBackoff[key]
	if rec == nil || now.Sub(rec.restartedAt) >= evictedRestartBackoffReset {
		delete(a.evictionBackoff, key)
		return time.Time{}
	}
	if rec.wait == 0 {
		rec.wait = evictedRestartInitialWait
	} else {
		rec.wait = min(2*rec.wait, evictedRestartMaxWait)
	}
	return now.Add(rec.wait)
}

// noteEvictedRestarted runs after key's engine has started. If that was the
// queued restart of an evicted service, it releases the service's reserved
// RSS, records the restart for the backoff, and sets its MemoryPressure
// condition to the current level, no longer Evicted.
func (a *MetalAgent) noteEvictedRestarted(ctx context.Context, key string) {
	a.mu.Lock()
	_, restarting := a.pressureRestarting[key]
	if restarting {
		delete(a.pressureRestarting, key)
		rec := a.evictionBackoff[key]
		if rec == nil {
			rec = &evictionBackoff{}
			a.evictionBackoff[key] = rec
		}
		rec.restartedAt = a.now()
	}
	a.mu.Unlock()
	if restarting {
		a.markUnblockedCondition(ctx, key)
	}
}

// forgetEvicted drops everything the agent remembers about key's eviction:
// its block, its queued restart's reservation and its backoff.
func (a *MetalAgent) forgetEvicted(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pressureBlocked, key)
	delete(a.pressureRestarting, key)
	delete(a.evictionBackoff, key)
}

// hasEvictedWaiting reports whether evicted services are still blocked, so
// the watchdog keeps delivering steady Normal samples to retry them.
func (a *MetalAgent) hasEvictedWaiting() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.pressureBlocked) > 0
}

// markUnblockedCondition sets key's MemoryPressure condition to the current
// level, no longer Evicted, and records the key as observed at that level.
func (a *MetalAgent) markUnblockedCondition(ctx context.Context, key string) {
	a.mu.RLock()
	level := a.lastPressureLevel
	a.mu.RUnlock()
	namespace, name := parseKey(key)
	if err := a.updateMemoryPressureCondition(ctx, &ManagedProcess{Namespace: namespace, Name: name}, level,
		"watchdog reported "+level.String()+" memory pressure"); err != nil {
		a.logger.Warnw("failed to update MemoryPressure condition", "key", key, "level", level.String(), "error", err)
		return
	}
	a.mu.Lock()
	a.pressureObserved[key] = level
	a.mu.Unlock()
}
