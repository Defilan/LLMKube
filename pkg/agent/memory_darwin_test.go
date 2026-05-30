//go:build darwin

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

import "testing"

// Representative vm_stat output from an Apple Silicon host with a large
// Metal-wired model loaded: most RAM sits in "wired down", and the
// reclaimable budget is spread across free, inactive, speculative, and
// purgeable. macOS counts all four as "available". Regression fixture
// for defilantech/LLMKube#578.
const sampleVMStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                          58000.
Pages active:                       450000.
Pages inactive:                     440000.
Pages speculative:                   20000.
Pages throttled:                         0.
Pages wired down:                  1250000.
Pages purgeable:                     30000.
Pages stored in compressor:         100000.
Pages occupied by compressor:        40000.
File-backed pages:                  300000.
Anonymous pages:                    590000.
`

func TestAvailableBytesFromVMStat_IncludesPurgeableAndSpeculative(t *testing.T) {
	const pageSize uint64 = 16384
	// free + inactive + speculative + purgeable
	want := uint64(58000+440000+20000+30000) * pageSize

	got := availableBytesFromVMStat(sampleVMStat)

	if got != want {
		t.Fatalf("availableBytesFromVMStat = %d, want %d (free+inactive+speculative+purgeable)", got, want)
	}
}

// A node that omits the optional speculative/purgeable lines must still
// compute free+inactive without error (older macOS / unusual output).
func TestAvailableBytesFromVMStat_ToleratesMissingOptionalLines(t *testing.T) {
	const out = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                          58000.
Pages inactive:                     440000.
Pages wired down:                  1250000.
`
	const pageSize uint64 = 16384
	want := uint64(58000+440000) * pageSize

	got := availableBytesFromVMStat(out)

	if got != want {
		t.Fatalf("availableBytesFromVMStat = %d, want %d (free+inactive only)", got, want)
	}
}
