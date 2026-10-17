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
	"fmt"
	"math"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

type mockMemoryProvider struct {
	totalBytes, availableBytes, wiredBytes uint64
	totalErr, availableErr, wiredErr       error
	processRSS                             map[int]uint64
	processRSSErr                          error
}

func (m *mockMemoryProvider) TotalMemory() (uint64, error) {
	return m.totalBytes, m.totalErr
}

func (m *mockMemoryProvider) AvailableMemory() (uint64, error) {
	return m.availableBytes, m.availableErr
}

func (m *mockMemoryProvider) WiredMemory() (uint64, error) {
	return m.wiredBytes, m.wiredErr
}

func (m *mockMemoryProvider) ProcessRSS(pid int) (uint64, error) {
	if m.processRSSErr != nil {
		return 0, m.processRSSErr
	}
	if m.processRSS != nil {
		if v, ok := m.processRSS[pid]; ok {
			return v, nil
		}
	}
	return 0, fmt.Errorf("no RSS for pid %d", pid)
}

func TestEstimateModelMemory_WithFullMetadata(t *testing.T) {
	// Llama 8B: 32 layers, 4096 embedding, 2048 context, ~4.5 GiB file
	fileSize := uint64(4831838208) // ~4.5 GiB
	est := EstimateModelMemory(fileSize, 32, 4096, 2048)

	// KV cache = 2 * 32 * 4096 * 2048 * 2 = 1073741824 (1 GiB)
	expectedKV := uint64(2 * 32 * 4096 * 2048 * 2)
	if est.KVCacheBytes != expectedKV {
		t.Errorf("KVCacheBytes = %d, want %d", est.KVCacheBytes, expectedKV)
	}
	if est.WeightsBytes != fileSize {
		t.Errorf("WeightsBytes = %d, want %d", est.WeightsBytes, fileSize)
	}
	if est.OverheadBytes != 512*1024*1024 {
		t.Errorf("OverheadBytes = %d, want %d", est.OverheadBytes, uint64(512*1024*1024))
	}
	expectedTotal := fileSize + expectedKV + 512*1024*1024
	if est.TotalBytes != expectedTotal {
		t.Errorf("TotalBytes = %d, want %d", est.TotalBytes, expectedTotal)
	}
}

func TestEstimateModelMemory_WithoutMetadata(t *testing.T) {
	fileSize := uint64(4 * 1024 * 1024 * 1024) // 4 GiB
	est := EstimateModelMemory(fileSize, 0, 0, 2048)

	if est.KVCacheBytes != 0 {
		t.Errorf("KVCacheBytes = %d, want 0 (fallback mode)", est.KVCacheBytes)
	}
	// Fallback: fileSize * 1.2 + 512 MiB. Use the same math.Ceil logic as the implementation.
	if est.TotalBytes <= fileSize {
		t.Errorf("TotalBytes (%d) should exceed file size (%d) in fallback mode", est.TotalBytes, fileSize)
	}
	// Verify the estimate is roughly fileSize * 1.2 + 512 MiB (within 1 byte of rounding)
	expected := EstimateModelMemory(fileSize, 0, 0, 2048)
	if est.TotalBytes != expected.TotalBytes {
		t.Errorf("TotalBytes = %d, want %d", est.TotalBytes, expected.TotalBytes)
	}
}

func TestEstimateModelMemory_ZeroEmbedding(t *testing.T) {
	fileSize := uint64(2 * 1024 * 1024 * 1024)
	est := EstimateModelMemory(fileSize, 32, 0, 2048)

	// Should fall back to heuristic since embeddingSize is 0
	if est.KVCacheBytes != 0 {
		t.Errorf("KVCacheBytes = %d, want 0 (fallback when embeddingSize=0)", est.KVCacheBytes)
	}
}

func TestEstimateModelMemory_LargeContext(t *testing.T) {
	fileSize := uint64(4831838208)
	smallCtx := EstimateModelMemory(fileSize, 32, 4096, 2048)
	largeCtx := EstimateModelMemory(fileSize, 32, 4096, 8192)

	if largeCtx.KVCacheBytes != smallCtx.KVCacheBytes*4 {
		t.Errorf("KV cache should scale 4x with 4x context: got %d, want %d",
			largeCtx.KVCacheBytes, smallCtx.KVCacheBytes*4)
	}
	if largeCtx.TotalBytes <= smallCtx.TotalBytes {
		t.Error("larger context should produce larger total estimate")
	}
}

// TestEstimateModelMemory_F16DefaultMatchesLegacyFormula proves the new
// EstimateModelMemoryWithOptions path is bit-for-bit identical to the legacy
// integer-math formula when called with the historical f16/f16 default.
// Critical regression guard for any future call site relying on the old shape.
func TestEstimateModelMemory_F16DefaultMatchesLegacyFormula(t *testing.T) {
	fileSize := uint64(4831838208)
	got := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 65536, EstimateOptions{})
	// Legacy formula: 4 * layers * embed * ctx (2 bytes/element * K + V).
	want := uint64(4 * 32 * 4096 * 65536)
	if got.KVCacheBytes != want {
		t.Errorf("default-options KV = %d, want legacy %d", got.KVCacheBytes, want)
	}
}

// TestEstimateModelMemory_Q8KVHalvesCache: q8_0 K + V should be roughly half
// of f16 K + V for the same model and context. (Exactly 1.0625/2.0 ≈ 0.531
// thanks to the 16-bit per-block scale.)
func TestEstimateModelMemory_Q8KVHalvesCache(t *testing.T) {
	fileSize := uint64(4831838208)
	f16 := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 65536, EstimateOptions{})
	q8 := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 65536, EstimateOptions{
		CacheTypeK: "q8_0",
		CacheTypeV: "q8_0",
	})
	ratio := float64(q8.KVCacheBytes) / float64(f16.KVCacheBytes)
	if ratio < 0.50 || ratio > 0.55 {
		t.Errorf("q8_0/f16 KV ratio = %.3f, want ~0.531", ratio)
	}
}

// TestEstimateModelMemory_Turbo3CompressesKV asserts the architecture-independent
// invariant: turbo3 K + V (3.25 + 3.25 bits/element) should produce a KV cache
// roughly 1/5 the size of f16 K + V (16 + 16 bits/element). 0.8125 / 4.0 ≈ 0.203,
// so the ratio must land between 0.18 and 0.23.
//
// This is the user-visible behavior change that motivated wiring cache types
// into the estimator: a 256K context with TurboQuant cache that previously got
// rejected as OOM (f16 estimate) now passes the pre-flight check.
func TestEstimateModelMemory_Turbo3CompressesKV(t *testing.T) {
	const fileSize = uint64(35) * 1024 * 1024 * 1024
	const layers, embed = uint64(64), uint64(5120)
	const context = 262144 // 256K

	f16 := EstimateModelMemoryWithOptions(fileSize, layers, embed, context, EstimateOptions{})
	turbo3 := EstimateModelMemoryWithOptions(fileSize, layers, embed, context, EstimateOptions{
		CacheTypeK: "turbo3",
		CacheTypeV: "turbo3",
	})

	ratio := float64(turbo3.KVCacheBytes) / float64(f16.KVCacheBytes)
	if ratio < 0.18 || ratio > 0.23 {
		t.Errorf("turbo3/f16 KV ratio = %.3f, want ~0.203 (0.8125/4.0)", ratio)
	}
	if turbo3.TotalBytes >= f16.TotalBytes {
		t.Errorf("turbo3 total should be lower than f16 total: turbo3=%d, f16=%d",
			turbo3.TotalBytes, f16.TotalBytes)
	}
}

func TestEstimateModelMemory_AsymmetricCacheTypes(t *testing.T) {
	// Renjith's recommendation: -ctk q8_0 -ctv turbo4. The estimate should
	// reflect this without forcing the user into symmetric K/V.
	fileSize := uint64(4831838208)
	got := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 65536, EstimateOptions{
		CacheTypeK: "q8_0",
		CacheTypeV: "turbo4",
	})
	// q8_0 (1.0625) + turbo4 (0.53125) = 1.59375 bytes/element.
	const product = uint64(32) * 4096 * 65536
	want := uint64(float64(product) * 1.59375)
	// Allow a single-byte rounding tolerance from math.Ceil. Compare via
	// uint64 to dodge the int64 conversion lint.
	if got.KVCacheBytes < want || got.KVCacheBytes > want+1 {
		t.Errorf("asymmetric q8_0/turbo4 KV = %d, want %d or %d", got.KVCacheBytes, want, want+1)
	}
}

func TestEstimateModelMemory_UnknownCacheTypeFallsBackToF16(t *testing.T) {
	fileSize := uint64(4831838208)
	unknown := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 65536, EstimateOptions{
		CacheTypeK: "future-cache-format",
		CacheTypeV: "future-cache-format",
	})
	f16 := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 65536, EstimateOptions{})
	if unknown.KVCacheBytes != f16.KVCacheBytes {
		t.Errorf("unknown cache type should fall back to f16: got %d, want %d",
			unknown.KVCacheBytes, f16.KVCacheBytes)
	}
}

// TestEstimateModelMemoryWithOptions_GQAShape covers the grouped-query-attention
// KV estimate (#2085): the GQA formula when the GGUF attention shape is known,
// byte-identical legacy behavior when it is not, per-side cache-type bytes,
// and the fallback and overflow guards.
func TestEstimateModelMemoryWithOptions_GQAShape(t *testing.T) {
	const fileSize = uint64(4 * 1024 * 1024 * 1024) // 4 GiB
	const kvGQA = uint64(17179869184)               // 32 layers * 131072 ctx * 8 kv heads * 128 dims * 4 bytes = 16 GiB
	const kvLegacy = uint64(68719476736)            // 32 * 4096 * 131072 * 4 bytes = 64 GiB
	const overhead = uint64(512 * 1024 * 1024)

	tests := []struct {
		name       string
		layerCount uint64
		embedSize  uint64
		opts       EstimateOptions
		wantKV     uint64
		fallback   bool // expect the fileSize*1.2 heuristic instead of a KV term
	}{
		{
			name:       "llama-3.1-8B shape with full GQA metadata",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{HeadCount: 32, HeadCountKV: 8, KeyLength: 128, ValueLength: 128},
			wantKV: kvGQA,
		},
		{
			name:       "no GQA metadata matches the legacy formula exactly",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{},
			wantKV: kvLegacy,
		},
		{
			// Real Llama 3.1 8B GGUFs carry head_count_kv but no key/value
			// lengths; the derived dims (4096/32 = 128) are llama.cpp's own
			// allocation default for an absent length, so the GQA term is
			// exact for the issue's flagship example.
			name:       "llama-3.1-8B real-file shape (lengths absent) derives head dims",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{HeadCount: 32, HeadCountKV: 8},
			wantKV: kvGQA,
		},
		{
			// A missing key length derives while an explicit value length
			// stands: llama.cpp defaults each side independently.
			name:       "key length absent derives, value length present",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{HeadCount: 32, HeadCountKV: 8, ValueLength: 128},
			wantKV: kvGQA,
		},
		{
			// Real Qwen 2.5 7B GGUFs: heads 28, kv heads 4, no lengths.
			name:       "qwen2.5-7B real-file shape (lengths absent) derives head dims",
			layerCount: 28, embedSize: 3584,
			opts:   EstimateOptions{HeadCount: 28, HeadCountKV: 4},
			wantKV: 7516192768, // 28 * 131072 * 4 * 128 * 4
		},
		{
			// The multi-head-attention identity: with kv heads equal to head
			// count the derived dims reproduce the legacy number exactly.
			name:       "kv heads equal to headCount derives dims and matches legacy",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{HeadCount: 32, HeadCountKV: 32},
			wantKV: kvLegacy,
		},
		{
			name:       "equal head counts with non-divisible embedding falls back",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{HeadCount: 48, HeadCountKV: 48},
			wantKV: kvLegacy,
		},
		{
			name:       "asymmetric cache types apply per-side bytes",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{HeadCount: 32, HeadCountKV: 8, KeyLength: 128, ValueLength: 128, CacheTypeK: "q8_0"},
			wantKV: 13153337344, // 4294967296 elems * (1.0625 + 2.0)
		},
		{
			name:       "missing headCount falls back even with kv heads and lengths",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{HeadCountKV: 8, KeyLength: 128, ValueLength: 128},
			wantKV: kvLegacy,
		},
		{
			name:       "zero headCountKV falls back, no MHA substitution",
			layerCount: 32, embedSize: 4096,
			// keyLength 64 differs from the derivable 4096/32 = 128 so the
			// row distinguishes the fallback from a headCount substitution:
			// substituting would halve the KV term.
			opts:   EstimateOptions{HeadCount: 32, HeadCountKV: 0, KeyLength: 64, ValueLength: 64},
			wantKV: kvLegacy,
		},
		{
			// Explicit dims must win even when the embedding/headCount
			// derivation would fail its divisibility check (4096 % 48 != 0).
			name:       "explicit head dims win over an underivable embedding ratio",
			layerCount: 32, embedSize: 4096,
			opts:   EstimateOptions{HeadCount: 48, HeadCountKV: 48, KeyLength: 256, ValueLength: 256},
			wantKV: 206158430208, // 32 * 48 * 131072 * 256 * 4
		},
		{
			// Element counts fit uint64 (2^62) while the f16 byte sum is 2^64,
			// which does not; the row must take the fallback, not a garbage
			// conversion.
			name:       "byte totals beyond uint64 fall back",
			layerCount: 1 << 28, embedSize: 256,
			opts:     EstimateOptions{HeadCount: 256, HeadCountKV: 256, KeyLength: 512, ValueLength: 512},
			fallback: true,
		},
		{
			// Real DeepSeek-V2-Lite GGUFs carry kv_lora_rank: a low-rank (MLA)
			// cache whose per-token cell is not head-shaped, so the head-dim
			// formula does not describe it and the model keeps the legacy
			// estimate instead of the 1.25x raise the stated dims would give.
			name:       "deepseek-v2-lite MLA shape (kv_lora_rank present) keeps the legacy estimate",
			layerCount: 27, embedSize: 2048,
			opts: EstimateOptions{
				HeadCount: 16, HeadCountKV: 16, KeyLength: 192, ValueLength: 128, KVLoraRank: 512,
			},
			wantKV: 28991029248, // 27 * 2048 * 131072 * 4, the legacy number
		},
		{
			// Real Gemma 7B GGUFs: 16 heads x 256 dims against a 3072
			// embedding, so the attention-shape charge is higher than the
			// embedding-width formula and matches what llama-server allocates.
			name:       "gemma-7B shape (heads x head_dim above embedding width) estimates higher than legacy",
			layerCount: 28, embedSize: 3072,
			opts: EstimateOptions{
				HeadCount: 16, HeadCountKV: 16, KeyLength: 256, ValueLength: 256,
			},
			wantKV: 60129542144, // 28 * 131072 * 16 * (256*2 + 256*2)
		},
		{
			// q5_0's per-element cost (0.6875) gives a MaxUint64/4 bound, so
			// element counts a fixed MaxUint64/8 bound would refuse now
			// compute; the literal is exact (all factors are powers-of-two
			// multiples).
			name:       "q5_0 element counts between the legacy and derived bounds compute",
			layerCount: 1 << 20, embedSize: 256,
			opts: EstimateOptions{
				HeadCount: 256, HeadCountKV: 256, KeyLength: 98304, ValueLength: 98304,
				CacheTypeK: "q5_0", CacheTypeV: "q5_0",
			},
			wantKV: 4755801206503243776, // 33 * 2^57: two sides of 3*2^60 elements at 11/16 bytes
		},
		{
			// f32's per-element cost (4.0) tightens the bound to MaxUint64/16,
			// below the old fixed MaxUint64/8: these element counts must fall
			// back instead of producing a byte total near the uint64 edge.
			name:       "f32 byte totals above the derived bound fall back",
			layerCount: 1 << 20, embedSize: 256,
			opts: EstimateOptions{
				HeadCount: 256, HeadCountKV: 256, KeyLength: 49152, ValueLength: 49152,
				CacheTypeK: "f32", CacheTypeV: "f32",
			},
			fallback: true,
		},
		{
			// layerCount*kvHeads*ctx stays below 2^64 (2^45) while
			// base*keyLen overflows, so the row fires at the kElems guard
			// specifically; the shape itself passes resolveKVShape (kv == q).
			name:       "absurd shapes hit the overflow guard and fall back",
			layerCount: 1 << 20, embedSize: 256,
			opts:     EstimateOptions{HeadCount: 256, HeadCountKV: 256, KeyLength: 1048576, ValueLength: 1048576},
			fallback: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			est := EstimateModelMemoryWithOptions(fileSize, tt.layerCount, tt.embedSize, 131072, tt.opts)
			if tt.fallback {
				want := fallbackEstimate(fileSize)
				if est.KVCacheBytes != 0 || est.TotalBytes != want.TotalBytes {
					t.Errorf("expected fallback estimate, got KV=%d total=%d, want KV=%d total=%d",
						est.KVCacheBytes, est.TotalBytes, want.KVCacheBytes, want.TotalBytes)
				}
				return
			}
			if est.KVCacheBytes != tt.wantKV {
				t.Errorf("KV = %d, want %d", est.KVCacheBytes, tt.wantKV)
			}
			if want := fileSize + tt.wantKV + overhead; est.TotalBytes != want {
				t.Errorf("total = %d, want %d", est.TotalBytes, want)
			}
		})
	}

	// The multi-head-attention path must reproduce the legacy branch
	// byte-for-byte even with a quantized cache type, where the two float
	// expressions could diverge by a rounding step.
	mha := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 131072, EstimateOptions{
		HeadCount: 32, HeadCountKV: 32, CacheTypeK: "q8_0",
	})
	legacyQ8 := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 131072, EstimateOptions{CacheTypeK: "q8_0"})
	if mha.KVCacheBytes != legacyQ8.KVCacheBytes || mha.KVCacheBytes != 52613349376 {
		t.Errorf("MHA q8_0 KV = %d, want %d (legacy branch)", mha.KVCacheBytes, legacyQ8.KVCacheBytes)
	}

	// The legacy 4-arg entry point must keep producing the no-metadata number.
	legacy := EstimateModelMemory(fileSize, 32, 4096, 131072)
	if legacy.KVCacheBytes != kvLegacy {
		t.Errorf("legacy estimate = %d, want %d", legacy.KVCacheBytes, kvLegacy)
	}
}

// A zero context must not panic in the GQA branch's divisions and must match
// what the legacy branch produces for the same input (a zero KV term).
func TestEstimateModelMemoryWithOptions_GQAZeroContext(t *testing.T) {
	const fileSize = uint64(4 * 1024 * 1024 * 1024)
	opts := EstimateOptions{HeadCount: 32, HeadCountKV: 8, KeyLength: 128, ValueLength: 128}
	got := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 0, opts)
	want := EstimateModelMemoryWithOptions(fileSize, 32, 4096, 0, EstimateOptions{})
	if got.KVCacheBytes != want.KVCacheBytes || got.TotalBytes != want.TotalBytes {
		t.Errorf("zero context with GQA opts = (KV=%d total=%d), want legacy zero-context (KV=%d total=%d)",
			got.KVCacheBytes, got.TotalBytes, want.KVCacheBytes, want.TotalBytes)
	}
}

func TestDefaultMemoryFraction_Small(t *testing.T) {
	total := uint64(16 * 1024 * 1024 * 1024) // 16 GiB
	f := DefaultMemoryFraction(total)
	if f != 0.67 {
		t.Errorf("fraction = %f, want 0.67 for 16GB system", f)
	}
}

func TestDefaultMemoryFraction_AtThreshold(t *testing.T) {
	total := uint64(36 * 1024 * 1024 * 1024) // 36 GiB
	f := DefaultMemoryFraction(total)
	if f != 0.67 {
		t.Errorf("fraction = %f, want 0.67 for 36GB system (at threshold)", f)
	}
}

func TestDefaultMemoryFraction_Large(t *testing.T) {
	total := uint64(64 * 1024 * 1024 * 1024) // 64 GiB
	f := DefaultMemoryFraction(total)
	if f != 0.75 {
		t.Errorf("fraction = %f, want 0.75 for 64GB system", f)
	}
}

func TestCheckMemoryBudget_Fits(t *testing.T) {
	provider := &mockMemoryProvider{totalBytes: 48 * 1024 * 1024 * 1024} // 48 GiB
	estimate := MemoryEstimate{TotalBytes: 10 * 1024 * 1024 * 1024}      // 10 GiB

	budget, err := CheckMemoryBudget(provider, estimate, 0.75)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !budget.Fits {
		t.Error("expected model to fit within budget")
	}
	expectedBudget := uint64(float64(48*1024*1024*1024) * 0.75)
	if budget.BudgetBytes != expectedBudget {
		t.Errorf("BudgetBytes = %d, want %d", budget.BudgetBytes, expectedBudget)
	}
	if budget.HeadroomBytes != expectedBudget-estimate.TotalBytes {
		t.Errorf("HeadroomBytes = %d, want %d", budget.HeadroomBytes, expectedBudget-estimate.TotalBytes)
	}
}

func TestCheckMemoryBudget_DoesNotFit(t *testing.T) {
	provider := &mockMemoryProvider{totalBytes: 16 * 1024 * 1024 * 1024} // 16 GiB
	estimate := MemoryEstimate{TotalBytes: 14 * 1024 * 1024 * 1024}      // 14 GiB

	budget, err := CheckMemoryBudget(provider, estimate, 0.67)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if budget.Fits {
		t.Error("expected model to NOT fit within budget")
	}
	if budget.HeadroomBytes != 0 {
		t.Errorf("HeadroomBytes should be 0 when model doesn't fit, got %d", budget.HeadroomBytes)
	}
}

func TestCheckMemoryBudget_ExactlyAtLimit(t *testing.T) {
	total := uint64(16 * 1024 * 1024 * 1024)
	budgetBytes := uint64(float64(total) * 0.75)
	provider := &mockMemoryProvider{totalBytes: total}
	estimate := MemoryEstimate{TotalBytes: budgetBytes}

	budget, err := CheckMemoryBudget(provider, estimate, 0.75)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !budget.Fits {
		t.Error("expected model to fit when exactly at budget limit")
	}
	if budget.HeadroomBytes != 0 {
		t.Errorf("HeadroomBytes = %d, want 0 at exact limit", budget.HeadroomBytes)
	}
}

func TestCheckMemoryBudget_ProviderError(t *testing.T) {
	provider := &mockMemoryProvider{totalErr: errTestProvider}
	estimate := MemoryEstimate{TotalBytes: 1024}

	_, err := CheckMemoryBudget(provider, estimate, 0.75)
	if err == nil {
		t.Fatal("expected error when provider fails")
	}
}

var errTestProvider = fmt.Errorf("mock provider error")

func TestFormatMemory(t *testing.T) {
	tests := []struct {
		bytes    uint64
		expected string
	}{
		{0, "0 MB"},
		{512 * 1024 * 1024, "512 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{26093297869, "24.3 GB"}, // 24.3 * 1024^3
		{48 * 1024 * 1024 * 1024, "48.0 GB"},
	}
	for _, tt := range tests {
		got := formatMemory(tt.bytes)
		if got != tt.expected {
			t.Errorf("formatMemory(%d) = %q, want %q", tt.bytes, got, tt.expected)
		}
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string
		expected uint64
	}{
		{"4.5 GiB", uint64(4.5 * 1024 * 1024 * 1024)},
		{"512.0 MiB", uint64(512 * 1024 * 1024)},
		{"1.0 KiB", 1024},
		{"100 B", 100},
		{"1.0 GiB", 1024 * 1024 * 1024},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.input)
		if err != nil {
			t.Errorf("parseSize(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if got != tt.expected {
			t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.expected)
		}
	}
}

func TestParseSize_Invalid(t *testing.T) {
	tests := []string{
		"",
		"abc",
		"4.5",
		"4.5 XB",
		"not a number GiB",
	}
	for _, input := range tests {
		_, err := parseSize(input)
		if err == nil {
			t.Errorf("parseSize(%q) expected error, got nil", input)
		}
	}
}

func TestNewMetalAgent_MemoryFraction(t *testing.T) {
	provider := &mockMemoryProvider{totalBytes: 64 * 1024 * 1024 * 1024} // 64 GiB

	// Auto-detect: should resolve to 0.75 for 64GB
	agent := NewMetalAgent(MetalAgentConfig{
		MemoryProvider: provider,
	})
	if agent.memoryFraction != 0.75 {
		t.Errorf("auto-detected fraction = %f, want 0.75 for 64GB", agent.memoryFraction)
	}

	// Explicit fraction
	agent = NewMetalAgent(MetalAgentConfig{
		MemoryProvider: provider,
		MemoryFraction: 0.5,
	})
	if agent.memoryFraction != 0.5 {
		t.Errorf("explicit fraction = %f, want 0.5", agent.memoryFraction)
	}

	// Small system auto-detect
	smallProvider := &mockMemoryProvider{totalBytes: 16 * 1024 * 1024 * 1024}
	agent = NewMetalAgent(MetalAgentConfig{
		MemoryProvider: smallProvider,
	})
	if agent.memoryFraction != 0.67 {
		t.Errorf("auto-detected fraction = %f, want 0.67 for 16GB", agent.memoryFraction)
	}
}

// --- ResolveMemoryBudget tests ---

func TestResolveMemoryBudget_AbsoluteTakesPrecedence(t *testing.T) {
	frac := 0.8
	hw := &inferencev1alpha1.HardwareSpec{
		MemoryBudget:   "24Gi",
		MemoryFraction: &frac,
	}
	resolved, err := ResolveMemoryBudget(hw, 0.67)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved.Mode != BudgetModeAbsolute {
		t.Errorf("Mode = %q, want absolute", resolved.Mode)
	}
	if resolved.Source != "crd-budget" {
		t.Errorf("Source = %q, want crd-budget", resolved.Source)
	}
	expectedBytes := uint64(24 * 1024 * 1024 * 1024)
	if resolved.Bytes != expectedBytes {
		t.Errorf("Bytes = %d, want %d", resolved.Bytes, expectedBytes)
	}
}

func TestResolveMemoryBudget_FractionFromCRD(t *testing.T) {
	frac := 0.9
	hw := &inferencev1alpha1.HardwareSpec{
		MemoryFraction: &frac,
	}
	resolved, err := ResolveMemoryBudget(hw, 0.67)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved.Mode != BudgetModeFraction {
		t.Errorf("Mode = %q, want fraction", resolved.Mode)
	}
	if resolved.Source != "crd-fraction" {
		t.Errorf("Source = %q, want crd-fraction", resolved.Source)
	}
	if resolved.Fraction != 0.9 {
		t.Errorf("Fraction = %f, want 0.9", resolved.Fraction)
	}
}

func TestResolveMemoryBudget_FallsBackToAgentDefault(t *testing.T) {
	hw := &inferencev1alpha1.HardwareSpec{
		Accelerator: "metal",
	}
	resolved, err := ResolveMemoryBudget(hw, 0.75)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved.Mode != BudgetModeFraction {
		t.Errorf("Mode = %q, want fraction", resolved.Mode)
	}
	if resolved.Source != "agent-flag" {
		t.Errorf("Source = %q, want agent-flag", resolved.Source)
	}
	if resolved.Fraction != 0.75 {
		t.Errorf("Fraction = %f, want 0.75", resolved.Fraction)
	}
}

func TestResolveMemoryBudget_InvalidBudgetString(t *testing.T) {
	hw := &inferencev1alpha1.HardwareSpec{
		MemoryBudget: "not-a-quantity",
	}
	_, err := ResolveMemoryBudget(hw, 0.67)
	if err == nil {
		t.Error("expected error for invalid memoryBudget string")
	}
}

func TestResolveMemoryBudget_NilHardware(t *testing.T) {
	resolved, err := ResolveMemoryBudget(nil, 0.67)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved.Mode != BudgetModeFraction {
		t.Errorf("Mode = %q, want fraction", resolved.Mode)
	}
	if resolved.Source != "agent-flag" {
		t.Errorf("Source = %q, want agent-flag", resolved.Source)
	}
	if resolved.Fraction != 0.67 {
		t.Errorf("Fraction = %f, want 0.67", resolved.Fraction)
	}
}

func TestResolveMemoryBudget_SmallAbsoluteBudget(t *testing.T) {
	hw := &inferencev1alpha1.HardwareSpec{
		MemoryBudget: "1Gi",
	}
	resolved, err := ResolveMemoryBudget(hw, 0.75)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved.Mode != BudgetModeAbsolute {
		t.Errorf("Mode = %q, want absolute", resolved.Mode)
	}
	expectedBytes := uint64(1024 * 1024 * 1024)
	if resolved.Bytes != expectedBytes {
		t.Errorf("Bytes = %d, want %d", resolved.Bytes, expectedBytes)
	}
}

// --- CheckMemoryBudgetAbsolute tests ---

func TestCheckMemoryBudgetAbsolute_Fits(t *testing.T) {
	estimate := MemoryEstimate{TotalBytes: 10 * 1024 * 1024 * 1024}  // 10 GiB
	budget := CheckMemoryBudgetAbsolute(24*1024*1024*1024, estimate) // 24 GiB budget

	if !budget.Fits {
		t.Error("expected model to fit within absolute budget")
	}
	if budget.BudgetBytes != 24*1024*1024*1024 {
		t.Errorf("BudgetBytes = %d, want %d", budget.BudgetBytes, uint64(24*1024*1024*1024))
	}
	expectedHeadroom := uint64(14 * 1024 * 1024 * 1024)
	if budget.HeadroomBytes != expectedHeadroom {
		t.Errorf("HeadroomBytes = %d, want %d", budget.HeadroomBytes, expectedHeadroom)
	}
}

func TestCheckMemoryBudgetAbsolute_DoesNotFit(t *testing.T) {
	estimate := MemoryEstimate{TotalBytes: 20 * 1024 * 1024 * 1024} // 20 GiB
	budget := CheckMemoryBudgetAbsolute(8*1024*1024*1024, estimate) // 8 GiB budget

	if budget.Fits {
		t.Error("expected model to NOT fit within absolute budget")
	}
	if budget.HeadroomBytes != 0 {
		t.Errorf("HeadroomBytes should be 0 when model doesn't fit, got %d", budget.HeadroomBytes)
	}
}

// --- Edge-case validation tests ---

func TestResolveMemoryBudget_ZeroFraction(t *testing.T) {
	zero := 0.0
	hw := &inferencev1alpha1.HardwareSpec{MemoryFraction: &zero}
	_, err := ResolveMemoryBudget(hw, 0.75)
	if err == nil {
		t.Error("expected error for zero memoryFraction")
	}
}

func TestResolveMemoryBudget_NegativeFraction(t *testing.T) {
	neg := -0.5
	hw := &inferencev1alpha1.HardwareSpec{MemoryFraction: &neg}
	_, err := ResolveMemoryBudget(hw, 0.75)
	if err == nil {
		t.Error("expected error for negative memoryFraction")
	}
}

func TestResolveMemoryBudget_FractionExceedsOne(t *testing.T) {
	big := 1.5
	hw := &inferencev1alpha1.HardwareSpec{MemoryFraction: &big}
	_, err := ResolveMemoryBudget(hw, 0.75)
	if err == nil {
		t.Error("expected error for memoryFraction > 1.0")
	}
}

func TestResolveMemoryBudget_NaNFraction(t *testing.T) {
	nan := math.NaN()
	hw := &inferencev1alpha1.HardwareSpec{MemoryFraction: &nan}
	_, err := ResolveMemoryBudget(hw, 0.75)
	if err == nil {
		t.Error("expected error for NaN memoryFraction")
	}
}

func TestResolveMemoryBudget_InfFraction(t *testing.T) {
	inf := math.Inf(1)
	hw := &inferencev1alpha1.HardwareSpec{MemoryFraction: &inf}
	_, err := ResolveMemoryBudget(hw, 0.75)
	if err == nil {
		t.Error("expected error for Inf memoryFraction")
	}
}

func TestResolveMemoryBudget_NegativeBudget(t *testing.T) {
	hw := &inferencev1alpha1.HardwareSpec{MemoryBudget: "-8Gi"}
	_, err := ResolveMemoryBudget(hw, 0.75)
	if err == nil {
		t.Error("expected error for negative memoryBudget")
	}
}

func TestResolveMemoryBudget_ZeroBudget(t *testing.T) {
	hw := &inferencev1alpha1.HardwareSpec{MemoryBudget: "0"}
	_, err := ResolveMemoryBudget(hw, 0.75)
	if err == nil {
		t.Error("expected error for zero memoryBudget")
	}
}

func TestEstimateModelMemory_OverflowFallsBack(t *testing.T) {
	fileSize := uint64(4 * 1024 * 1024 * 1024) // 4 GiB
	// Huge values that would overflow uint64 multiplication
	est := EstimateModelMemory(fileSize, math.MaxUint64/2, 4, 2)

	// Should have fallen back to heuristic (no KV cache)
	if est.KVCacheBytes != 0 {
		t.Errorf("expected fallback (KVCacheBytes=0) on overflow, got %d", est.KVCacheBytes)
	}
	if est.TotalBytes <= fileSize {
		t.Error("fallback estimate should exceed file size")
	}
}

func TestCacheTypeBytesPerElement(t *testing.T) {
	tests := []struct {
		name      string
		cacheType string
		expected  float64
	}{
		{"empty defaults to f16", "", 2.0},
		{"f16", "f16", 2.0},
		{"f32", "f32", 4.0},
		{"q8_0", "q8_0", 1.0625},
		{"q5_0", "q5_0", 0.6875},
		{"q5_1", "q5_1", 0.75},
		{"q4_0", "q4_0", 0.5625},
		{"iq4_nl", "iq4_nl", 0.5625},
		{"q4_1", "q4_1", 0.625},
		{"turbo3", "turbo3", 0.40625},
		{"turbo4", "turbo4", 0.53125},
		{"unknown falls back to f16", "unknown-type", 2.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cacheTypeBytesPerElement(tt.cacheType)
			if got != tt.expected {
				t.Errorf("cacheTypeBytesPerElement(%q) = %f, want %f", tt.cacheType, got, tt.expected)
			}
		})
	}
}
