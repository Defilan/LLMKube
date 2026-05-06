/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tui

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// hostMemoryGB returns the host's total memory in GB, rounded down. Returns
// 0 when the value can't be probed (unsupported platform or exec failure).
//
// The TUI uses this to tailor sizing recommendations on the deploy form.
// All call sites must tolerate the 0 return; never log "could not detect" —
// the form fallback already handles that case with a generic message.
func hostMemoryGB() int {
	switch runtime.GOOS {
	case "darwin":
		return darwinMemoryGB()
	case "linux":
		return linuxMemoryGB()
	}
	return 0
}

func darwinMemoryGB() int {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return int(bytes / (1024 * 1024 * 1024))
}

func linuxMemoryGB() int {
	out, err := exec.Command("grep", "MemTotal", "/proc/meminfo").Output()
	if err != nil {
		return 0
	}
	// Format: "MemTotal:       131072000 kB"
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0
	}
	kb, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return int(kb / (1024 * 1024))
}
