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
	"time"

	"go.uber.org/zap"
)

// MemoryWatchdogConfig holds configuration for the MemoryWatchdog.
type MemoryWatchdogConfig struct {
	Interval          time.Duration
	WarningThreshold  float64 // available fraction below which we warn (e.g. 0.20)
	CriticalThreshold float64 // available fraction below which we go critical (e.g. 0.10)
}

// MemoryStats holds a point-in-time snapshot of system and process memory.
type MemoryStats struct {
	TotalMemory     uint64
	AvailableMemory uint64
	WiredMemory     uint64
	ProcessRSS      map[string]uint64 // keyed by process name
	TotalRSS        uint64
	PressureLevel   MemoryPressureLevel
}

// normalSamplesBeforeRecovery is how many consecutive Normal samples the
// watchdog needs before it reports a return to Normal (30s at the default
// 10s interval). A level hovering around the Warning threshold would
// otherwise flap, and every reported return to Normal can restart evicted
// services.
const normalSamplesBeforeRecovery = 3

// MemoryWatchdog periodically samples system memory and per-process RSS,
// computes a pressure level, and updates Prometheus metrics. It calls
// onPressure on every sample at Warning or above, and with Normal once
// normalSamplesBeforeRecovery consecutive samples have been Normal after a
// higher level. After that, steady Normal samples call it only while
// wantNormal reports true. A sample whose available memory cannot be read has
// no level and is skipped.
type MemoryWatchdog struct {
	provider   MemoryProvider
	processes  func() []processMemInfo
	onPressure func(level MemoryPressureLevel, stats MemoryStats)
	config     MemoryWatchdogConfig
	logger     *zap.SugaredLogger
	// wantNormal, when set, asks for steady Normal samples too: the agent
	// sets it while evicted services wait for headroom to restart. Nil
	// means steady Normal samples stay silent.
	wantNormal func() bool
	// lastLevel is the level last delivered to onPressure, and normalStreak
	// counts the consecutive Normal samples since the last higher one. Only
	// the goroutine running sample touches them.
	lastLevel    MemoryPressureLevel
	normalStreak int
}

// processMemInfo is the minimal info needed to sample a process's RSS.
type processMemInfo struct {
	Name string
	PID  int
}

// NewMemoryWatchdog creates a new watchdog.
func NewMemoryWatchdog(
	provider MemoryProvider,
	processes func() []processMemInfo,
	onPressure func(level MemoryPressureLevel, stats MemoryStats),
	config MemoryWatchdogConfig,
	logger *zap.SugaredLogger,
) *MemoryWatchdog {
	return &MemoryWatchdog{
		provider:   provider,
		processes:  processes,
		onPressure: onPressure,
		config:     config,
		logger:     logger,
	}
}

// Run starts the watchdog loop. It blocks until ctx is cancelled.
func (w *MemoryWatchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(w.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sample()
		}
	}
}

// sample performs one memory check cycle.
func (w *MemoryWatchdog) sample() {
	stats := MemoryStats{
		ProcessRSS: make(map[string]uint64),
	}

	total, err := w.provider.TotalMemory()
	if err != nil {
		w.logger.Warnw("watchdog: failed to get total memory", "error", err)
		return
	}
	stats.TotalMemory = total

	avail, err := w.provider.AvailableMemory()
	availKnown := err == nil
	if err != nil {
		w.logger.Warnw("watchdog: failed to get available memory", "error", err)
	} else {
		stats.AvailableMemory = avail
		systemMemoryAvailableBytes.Set(float64(avail))
	}

	wired, err := w.provider.WiredMemory()
	if err != nil {
		w.logger.Debugw("watchdog: failed to get wired memory", "error", err)
	} else {
		stats.WiredMemory = wired
		systemMemoryWiredBytes.Set(float64(wired))
	}

	// Sample per-process RSS
	for _, p := range w.processes() {
		rss, rssErr := w.provider.ProcessRSS(p.PID)
		if rssErr != nil {
			w.logger.Debugw("watchdog: failed to get process RSS",
				"name", p.Name, "pid", p.PID, "error", rssErr)
			continue
		}
		stats.ProcessRSS[p.Name] = rss
		stats.TotalRSS += rss
		processRSSBytes.WithLabelValues(p.Name).Set(float64(rss))
	}

	// Without available memory the level is unknown: computing it from a
	// zero reads as Critical, which could evict a process on a sensor
	// error. Skip pressure evaluation for this sample, leaving the level
	// gauge, lastLevel and the callback untouched, so the next good sample
	// still compares against the last real level. It neither counts toward
	// nor breaks a run of Normal samples.
	if !availKnown {
		return
	}

	// Compute pressure level
	stats.PressureLevel = w.computePressure(stats)
	memoryPressureLevelGauge.Set(float64(stats.PressureLevel))

	if stats.PressureLevel >= MemoryPressureWarning {
		w.normalStreak = 0
		w.lastLevel = stats.PressureLevel
		w.logger.Warnw("memory pressure detected",
			"level", stats.PressureLevel.String(),
			"available", formatMemory(stats.AvailableMemory),
			"total", formatMemory(stats.TotalMemory),
			"wired", formatMemory(stats.WiredMemory),
			"totalRSS", formatMemory(stats.TotalRSS),
		)
		if w.onPressure != nil {
			w.onPressure(stats.PressureLevel, stats)
		}
		return
	}

	if w.normalStreak < normalSamplesBeforeRecovery {
		w.normalStreak++
	}
	switch {
	case w.lastLevel >= MemoryPressureWarning && w.normalStreak >= normalSamplesBeforeRecovery:
		// The callback needs the return to Normal to undo what it did
		// under pressure: clearing eviction blocks (otherwise held until
		// the agent restarts) and setting MemoryPressure conditions back
		// to Normal.
		w.logger.Infow("memory pressure returned to normal",
			"previous", w.lastLevel.String(),
			"available", formatMemory(stats.AvailableMemory),
			"total", formatMemory(stats.TotalMemory),
		)
		w.lastLevel = MemoryPressureNormal
	case w.lastLevel == MemoryPressureNormal && w.wantNormal != nil && w.wantNormal():
		// Steady Normal, delivered so evicted services waiting for
		// headroom can be retried against fresh numbers.
	default:
		return
	}
	if w.onPressure != nil {
		w.onPressure(stats.PressureLevel, stats)
	}
}

// computePressure determines the pressure level from available/total ratio.
func (w *MemoryWatchdog) computePressure(stats MemoryStats) MemoryPressureLevel {
	if stats.TotalMemory == 0 {
		return MemoryPressureNormal
	}
	availFraction := float64(stats.AvailableMemory) / float64(stats.TotalMemory)

	if availFraction < w.config.CriticalThreshold {
		return MemoryPressureCritical
	}
	if availFraction < w.config.WarningThreshold {
		return MemoryPressureWarning
	}
	return MemoryPressureNormal
}
