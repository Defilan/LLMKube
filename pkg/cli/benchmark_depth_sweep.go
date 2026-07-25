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

package cli

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// depthSweepVocab is a small word pool used to synthesize prompts of a
// target token count. Words are drawn from a fixed vocabulary so the
// generated text is deterministic for a given seed, but the per-repetition
// variation (see synthesizePrompt) ensures each request performs a real
// prefill rather than reusing a cached prefix.
var depthSweepVocab = []string{
	"the", "model", "processes", "tokens", "through", "attention", "layers",
	"and", "generates", "output", "based", "on", "context", "window", "size",
	"which", "affects", "memory", "bandwidth", "compute", "requirements",
	"for", "long", "sequences", "versus", "short", "prompts", "in", "real",
	"time", "inference", "workloads", "across", "different", "architectures",
	"including", "dense", "sparse", "hybrid", "and", "sliding", "window",
	"attention", "patterns", "that", "trade", "off", "latency", "throughput",
	"and", "quality", "for", "each", "specific", "use", "case", "scenario",
}

// synthesizePrompt builds a prompt of approximately targetTokens tokens by
// repeating words from depthSweepVocab. The rep index varies the starting
// offset so that each repetition produces a distinct prompt body, which is
// defence in depth against prompt-cache contamination: even with
// cache_prompt disabled, a unique body guarantees the server cannot reuse a
// cached prefix.
//
// The returned string is plain whitespace-separated words. The achieved
// token count is reported by the server (prompt_n), not assumed here,
// because synthetic text does not tokenize at a predictable ratio.
func synthesizePrompt(targetTokens, rep int) string {
	if targetTokens <= 0 {
		return ""
	}
	vocab := depthSweepVocab
	if len(vocab) == 0 {
		return ""
	}
	// Vary the starting offset per repetition so each prompt body differs.
	offset := rep % len(vocab)
	words := make([]string, 0, targetTokens)
	for i := 0; i < targetTokens; i++ {
		words = append(words, vocab[(offset+i)%len(vocab)])
	}
	return strings.Join(words, " ")
}

// computeSpread returns the median, min, max, and stdev of the given values.
// It is used to report the spread of throughput across repetitions for each
// depth, rather than a single central value.
func computeSpread(values []float64) TokenSpread {
	if len(values) == 0 {
		return TokenSpread{}
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	spread := TokenSpread{
		Min:    sorted[0],
		Max:    sorted[len(sorted)-1],
		Median: percentile(sorted, 50),
	}

	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))

	var sumSquares float64
	for _, v := range values {
		diff := v - mean
		sumSquares += diff * diff
	}
	spread.StdDev = math.Sqrt(sumSquares / float64(len(values)))

	return spread
}

// runPromptDepthSweep runs a prompt-depth sweep against a single already-
// deployed service. No redeploy and no --catalog requirement: it measures
// how throughput degrades as the prompt gets deeper against one running
// deployment.
func runPromptDepthSweep(opts *benchmarkOptions) error {
	ctx := context.Background()
	startTime := time.Now()

	values, err := parseSweepValues(opts.promptDepthSweep)
	if err != nil {
		return fmt.Errorf("invalid prompt-depth-sweep values: %w", err)
	}

	endpoint, cleanup, err := getEndpoint(ctx, opts)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return err
	}

	var gpuMon *gpuMonitor
	if opts.monitorGPU {
		gpuMon = newGPUMonitor()
		gpuMon.start()
	}

	fmt.Printf("\n🔄 Prompt Depth Sweep\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Service:     %s\n", opts.name)
	fmt.Printf("Values:      %v\n", values)
	fmt.Printf("Iterations:  %d per depth\n", opts.iterations)
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

	sweepReport := SweepReport{
		SweepType:  "Prompt Depth",
		Values:     make([]string, len(values)),
		Results:    make([]SweepResult, 0, len(values)),
		Timestamp:  startTime,
		GPUEnabled: opts.gpu,
	}
	for i, v := range values {
		sweepReport.Values[i] = strconv.Itoa(v)
	}

	for _, depth := range values {
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("📊 Testing prompt depth: %d tokens\n", depth)
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

		result := runPromptDepthSweepIteration(ctx, endpoint, depth, opts)
		sweepReport.Results = append(sweepReport.Results, result)
		fmt.Println()
	}

	if gpuMon != nil {
		sweepReport.GPUMetrics = gpuMon.stop()
	}

	sweepReport.Duration = time.Since(startTime)
	outputDepthSweepTable(sweepReport)

	if reportWriter != nil {
		if err := reportWriter.writeDepthSweepResults(&sweepReport); err != nil {
			return fmt.Errorf("failed to write sweep results: %w", err)
		}
		if len(sweepReport.GPUMetrics) > 0 {
			if err := reportWriter.writeGPUMetrics(sweepReport.GPUMetrics); err != nil {
				return fmt.Errorf("failed to write GPU metrics: %w", err)
			}
		}
		if err := reportWriter.close(); err != nil {
			return fmt.Errorf("failed to close report: %w", err)
		}
	}

	return nil
}

// runPromptDepthSweepIteration runs --iterations repetitions at a single
// target depth against the already-deployed endpoint. Each repetition
// synthesises a distinct prompt body and disables the prompt cache so every
// request performs a real prefill. The achieved token count is taken from
// the server's prompt_n, not the requested target.
func runPromptDepthSweepIteration(ctx context.Context, endpoint string, depth int, opts *benchmarkOptions) SweepResult {
	result := SweepResult{
		Parameter: "prompt_depth",
		Value:     strconv.Itoa(depth),
	}

	depthResult := DepthSweepResult{
		RequestedDepth: depth,
		Iterations:     opts.iterations,
	}

	var promptToksPerSec []float64
	var genToksPerSec []float64
	var achievedDepths []int

	for i := 0; i < opts.iterations; i++ {
		prompt := synthesizePrompt(depth, i)

		// sendBenchmarkRequestWithPrompt already sets CachePrompt: boolPtr(false),
		// so every iteration performs a genuine prefill. Combined with a unique
		// prompt body per repetition, this ensures the sweep measures the model,
		// not the cache. See issue #1268.
		benchResult, err := sendBenchmarkRequestWithPrompt(ctx, endpoint, opts, i+1, prompt)
		if err != nil {
			depthResult.Failed++
			depthResult.Error = err.Error()
			fmt.Printf("   [%d/%d] ❌ %v\n", i+1, opts.iterations, err)
			continue
		}

		depthResult.Successful++
		achievedDepths = append(achievedDepths, benchResult.PromptTokens)
		if benchResult.PromptToksPerSec > 0 {
			promptToksPerSec = append(promptToksPerSec, benchResult.PromptToksPerSec)
		}
		if benchResult.GenerationToksPerSec > 0 {
			genToksPerSec = append(genToksPerSec, benchResult.GenerationToksPerSec)
		}

		fmt.Printf("   [%d/%d] ✅ achieved %d prompt tokens | %.1f tok/s prefill | %.1f tok/s decode\n",
			i+1, opts.iterations, benchResult.PromptTokens,
			benchResult.PromptToksPerSec, benchResult.GenerationToksPerSec)
	}

	// Report the achieved token count from the server, not the requested
	// target. Synthetic text does not tokenize at a predictable ratio and
	// different models tokenize differently, so labelling rows by the
	// requested depth is wrong.
	if len(achievedDepths) > 0 {
		sort.Ints(achievedDepths)
		depthResult.AchievedDepth = achievedDepths[len(achievedDepths)/2]
	}

	depthResult.PromptToksPerSec = computeSpread(promptToksPerSec)
	depthResult.GenToksPerSec = computeSpread(genToksPerSec)

	// When every iteration fails (e.g. the depth exceeds the served
	// context), surface the error on the SweepResult so the sweep table
	// marks the row as failed and continues to the next depth.
	if depthResult.Successful == 0 && depthResult.Failed > 0 {
		result.Error = depthResult.Error
	}

	result.Depth = &depthResult
	return result
}
