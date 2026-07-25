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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSynthesizePrompt(t *testing.T) {
	tests := []struct {
		name          string
		targetTokens  int
		rep           int
		wantEmpty     bool
		wantWordCount int
	}{
		{"zero tokens", 0, 0, true, 0},
		{"negative tokens", -5, 0, true, 0},
		{"small prompt", 10, 0, false, 10},
		{"medium prompt", 100, 0, false, 100},
		{"large prompt", 1000, 0, false, 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := synthesizePrompt(tt.targetTokens, tt.rep)
			if tt.wantEmpty {
				if result != "" {
					t.Errorf("synthesizePrompt(%d, %d) = %q, want empty", tt.targetTokens, tt.rep, result)
				}
				return
			}
			words := strings.Fields(result)
			if len(words) != tt.wantWordCount {
				t.Errorf("synthesizePrompt(%d, %d) produced %d words, want %d",
					tt.targetTokens, tt.rep, len(words), tt.wantWordCount)
			}
		})
	}
}

func TestSynthesizePromptVariesPerRep(t *testing.T) {
	// Each repetition must produce a distinct prompt body so that the
	// sweep measures the model, not the prompt cache.
	prompt0 := synthesizePrompt(100, 0)
	prompt1 := synthesizePrompt(100, 1)
	prompt2 := synthesizePrompt(100, 2)

	if prompt0 == prompt1 {
		t.Error("rep 0 and rep 1 produced identical prompts; cache contamination risk")
	}
	if prompt0 == prompt2 {
		t.Error("rep 0 and rep 2 produced identical prompts; cache contamination risk")
	}
	if prompt1 == prompt2 {
		t.Error("rep 1 and rep 2 produced identical prompts; cache contamination risk")
	}
}

func TestSynthesizePromptAllWordsFromVocab(t *testing.T) {
	result := synthesizePrompt(500, 0)
	words := strings.Fields(result)

	vocabSet := make(map[string]bool, len(depthSweepVocab))
	for _, w := range depthSweepVocab {
		vocabSet[w] = true
	}

	for _, w := range words {
		if !vocabSet[w] {
			t.Errorf("word %q not in vocabulary", w)
		}
	}
}

func TestComputeSpread(t *testing.T) {
	tests := []struct {
		name    string
		values  []float64
		wantMin float64
		wantMax float64
		wantMed float64
	}{
		{"empty", []float64{}, 0, 0, 0},
		{"single", []float64{42.0}, 42.0, 42.0, 42.0},
		{"two values", []float64{10.0, 20.0}, 10.0, 20.0, 15.0},
		{"three values", []float64{10.0, 20.0, 30.0}, 10.0, 30.0, 20.0},
		{"unsorted", []float64{30.0, 10.0, 20.0}, 10.0, 30.0, 20.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spread := computeSpread(tt.values)
			if spread.Min != tt.wantMin {
				t.Errorf("Min = %v, want %v", spread.Min, tt.wantMin)
			}
			if spread.Max != tt.wantMax {
				t.Errorf("Max = %v, want %v", spread.Max, tt.wantMax)
			}
			if spread.Median != tt.wantMed {
				t.Errorf("Median = %v, want %v", spread.Median, tt.wantMed)
			}
		})
	}
}

func TestComputeSpreadStdDev(t *testing.T) {
	// All identical values => stdev should be 0
	spread := computeSpread([]float64{50.0, 50.0, 50.0})
	if spread.StdDev != 0 {
		t.Errorf("StdDev for identical values = %v, want 0", spread.StdDev)
	}

	// Values with spread => stdev should be positive
	spread = computeSpread([]float64{10.0, 20.0, 30.0})
	if spread.StdDev <= 0 {
		t.Errorf("StdDev for varied values = %v, want > 0", spread.StdDev)
	}
}

func TestComputeSpreadDoesNotMutateInput(t *testing.T) {
	values := []float64{30.0, 10.0, 20.0}
	original := make([]float64, len(values))
	copy(original, values)

	_ = computeSpread(values)

	for i, v := range values {
		if v != original[i] {
			t.Errorf("computeSpread mutated input at index %d: got %v, want %v", i, v, original[i])
		}
	}
}

func TestRunPromptDepthSweepIterationSuccess(t *testing.T) {
	// Create a mock server that returns a successful chat completion
	// response with a prompt_n matching the request.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		// Count words in the prompt to simulate achieved token count
		promptText := ""
		if len(req.Messages) > 0 {
			promptText = req.Messages[0].Content
		}
		achievedTokens := len(strings.Fields(promptText))

		response := ChatCompletionResponse{
			ID:      "test-id",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   "test-model",
			Choices: []struct {
				Index   int `json:"index"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}{
				{
					Index: 0,
					Message: struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					}{
						Role:    "assistant",
						Content: "Test response",
					},
					FinishReason: "stop",
				},
			},
			Usage: struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}{
				PromptTokens:     achievedTokens,
				CompletionTokens: 20,
				TotalTokens:      achievedTokens + 20,
			},
			Timings: struct {
				PromptN             int     `json:"prompt_n"`
				PromptMs            float64 `json:"prompt_ms"`
				PromptPerTokenMs    float64 `json:"prompt_per_token_ms"`
				PromptPerSecond     float64 `json:"prompt_per_second"`
				PredictedN          int     `json:"predicted_n"`
				PredictedMs         float64 `json:"predicted_ms"`
				PredictedPerTokenMs float64 `json:"predicted_per_token_ms"`
				PredictedPerSecond  float64 `json:"predicted_per_second"`
			}{
				PromptN:             achievedTokens,
				PromptMs:            float64(achievedTokens) * 2.0,
				PromptPerTokenMs:    2.0,
				PromptPerSecond:     500.0,
				PredictedN:          20,
				PredictedMs:         400.0,
				PredictedPerTokenMs: 20.0,
				PredictedPerSecond:  50.0,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	opts := &benchmarkOptions{
		name:       "test-service",
		iterations: 3,
		maxTokens:  50,
		timeout:    10 * time.Second,
	}

	result := runPromptDepthSweepIteration(t.Context(), server.URL, 100, opts)

	if result.Parameter != "prompt_depth" {
		t.Errorf("Parameter = %q, want %q", result.Parameter, "prompt_depth")
	}
	if result.Value != "100" {
		t.Errorf("Value = %q, want %q", result.Value, "100")
	}
	if result.Error != "" {
		t.Errorf("Error = %q, want empty", result.Error)
	}
	if result.Depth == nil {
		t.Fatal("Depth is nil, expected non-nil")
	}

	depth := result.Depth
	if depth.RequestedDepth != 100 {
		t.Errorf("RequestedDepth = %d, want 100", depth.RequestedDepth)
	}
	if depth.Iterations != 3 {
		t.Errorf("Iterations = %d, want 3", depth.Iterations)
	}
	if depth.Successful != 3 {
		t.Errorf("Successful = %d, want 3", depth.Successful)
	}
	if depth.Failed != 0 {
		t.Errorf("Failed = %d, want 0", depth.Failed)
	}
	// Achieved depth should be the median of the achieved token counts
	if depth.AchievedDepth <= 0 {
		t.Errorf("AchievedDepth = %d, want > 0", depth.AchievedDepth)
	}
	// Spread should have non-zero median
	if depth.PromptToksPerSec.Median != 500.0 {
		t.Errorf("PromptToksPerSec.Median = %v, want 500.0", depth.PromptToksPerSec.Median)
	}
	if depth.GenToksPerSec.Median != 50.0 {
		t.Errorf("GenToksPerSec.Median = %v, want 50.0", depth.GenToksPerSec.Median)
	}
}

func TestRunPromptDepthSweepIterationFailedDepth(t *testing.T) {
	// Create a mock server that returns an error (HTTP 400) to simulate
	// a depth that exceeds the served context.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("context length exceeded"))
	}))
	defer server.Close()

	opts := &benchmarkOptions{
		name:       "test-service",
		iterations: 3,
		maxTokens:  50,
		timeout:    10 * time.Second,
	}

	result := runPromptDepthSweepIteration(t.Context(), server.URL, 100000, opts)

	if result.Error == "" {
		t.Error("Expected error for failed depth, got empty")
	}
	if result.Depth == nil {
		t.Fatal("Depth is nil, expected non-nil")
	}
	if result.Depth.Successful != 0 {
		t.Errorf("Successful = %d, want 0", result.Depth.Successful)
	}
	if result.Depth.Failed != 3 {
		t.Errorf("Failed = %d, want 3", result.Depth.Failed)
	}
}

func TestRunPromptDepthSweepIterationPartialFailure(t *testing.T) {
	// Create a mock server that fails the first request but succeeds
	// afterwards, to verify the sweep continues past a failed iteration.
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("context length exceeded"))
			return
		}

		var req ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		achievedTokens := 100
		if len(req.Messages) > 0 {
			achievedTokens = len(strings.Fields(req.Messages[0].Content))
		}

		response := ChatCompletionResponse{
			ID:      "test-id",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   "test-model",
			Choices: []struct {
				Index   int `json:"index"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}{
				{
					Index: 0,
					Message: struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					}{
						Role:    "assistant",
						Content: "Test response",
					},
					FinishReason: "stop",
				},
			},
			Usage: struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}{
				PromptTokens:     achievedTokens,
				CompletionTokens: 20,
				TotalTokens:      achievedTokens + 20,
			},
			Timings: struct {
				PromptN             int     `json:"prompt_n"`
				PromptMs            float64 `json:"prompt_ms"`
				PromptPerTokenMs    float64 `json:"prompt_per_token_ms"`
				PromptPerSecond     float64 `json:"prompt_per_second"`
				PredictedN          int     `json:"predicted_n"`
				PredictedMs         float64 `json:"predicted_ms"`
				PredictedPerTokenMs float64 `json:"predicted_per_token_ms"`
				PredictedPerSecond  float64 `json:"predicted_per_second"`
			}{
				PromptN:             achievedTokens,
				PromptMs:            200.0,
				PromptPerTokenMs:    2.0,
				PromptPerSecond:     500.0,
				PredictedN:          20,
				PredictedMs:         400.0,
				PredictedPerTokenMs: 20.0,
				PredictedPerSecond:  50.0,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	opts := &benchmarkOptions{
		name:       "test-service",
		iterations: 3,
		maxTokens:  50,
		timeout:    10 * time.Second,
	}

	result := runPromptDepthSweepIteration(t.Context(), server.URL, 100, opts)

	if result.Depth == nil {
		t.Fatal("Depth is nil, expected non-nil")
	}
	if result.Depth.Successful != 2 {
		t.Errorf("Successful = %d, want 2", result.Depth.Successful)
	}
	if result.Depth.Failed != 1 {
		t.Errorf("Failed = %d, want 1", result.Depth.Failed)
	}
	// The sweep should continue past the failed iteration
	if result.Depth.AchievedDepth <= 0 {
		t.Errorf("AchievedDepth = %d, want > 0", result.Depth.AchievedDepth)
	}
}

func TestDepthSweepResultJSONSerialization(t *testing.T) {
	result := DepthSweepResult{
		RequestedDepth: 2048,
		AchievedDepth:  1980,
		PromptToksPerSec: TokenSpread{
			Median: 450.0,
			Min:    400.0,
			Max:    500.0,
			StdDev: 25.0,
		},
		GenToksPerSec: TokenSpread{
			Median: 50.0,
			Min:    45.0,
			Max:    55.0,
			StdDev: 2.0,
		},
		Iterations: 3,
		Successful: 3,
		Failed:     0,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Failed to marshal DepthSweepResult: %v", err)
	}

	var decoded DepthSweepResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal DepthSweepResult: %v", err)
	}

	if decoded.RequestedDepth != 2048 {
		t.Errorf("RequestedDepth = %d, want 2048", decoded.RequestedDepth)
	}
	if decoded.AchievedDepth != 1980 {
		t.Errorf("AchievedDepth = %d, want 1980", decoded.AchievedDepth)
	}
	if decoded.PromptToksPerSec.Median != 450.0 {
		t.Errorf("PromptToksPerSec.Median = %v, want 450.0", decoded.PromptToksPerSec.Median)
	}
	if decoded.GenToksPerSec.StdDev != 2.0 {
		t.Errorf("GenToksPerSec.StdDev = %v, want 2.0", decoded.GenToksPerSec.StdDev)
	}
}

func TestSweepResultDepthField(t *testing.T) {
	// Verify that SweepResult can carry a Depth field and serialize it
	depthResult := &DepthSweepResult{
		RequestedDepth: 8192,
		AchievedDepth:  7900,
		Iterations:     3,
		Successful:     3,
		Failed:         0,
	}

	result := SweepResult{
		Parameter: "prompt_depth",
		Value:     "8192",
		Depth:     depthResult,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Failed to marshal SweepResult: %v", err)
	}

	var decoded SweepResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal SweepResult: %v", err)
	}

	if decoded.Depth == nil {
		t.Fatal("Depth is nil after deserialization")
	}
	if decoded.Depth.RequestedDepth != 8192 {
		t.Errorf("RequestedDepth = %d, want 8192", decoded.Depth.RequestedDepth)
	}
}

func TestPromptDepthSweepFlagRegistered(t *testing.T) {
	cmd := NewBenchmarkCommand()
	flag := cmd.Flags().Lookup("prompt-depth-sweep")
	if flag == nil {
		t.Fatal("Expected --prompt-depth-sweep flag to be registered")
	}
	if flag.DefValue != "" {
		t.Errorf("Expected default value '', got '%s'", flag.DefValue)
	}
}

func TestOutputDepthSweepTable(t *testing.T) {
	report := SweepReport{
		SweepType: "Prompt Depth",
		Values:    []string{"512", "2048"},
		Results: []SweepResult{
			{
				Value: "512",
				Depth: &DepthSweepResult{
					RequestedDepth: 512,
					AchievedDepth:  500,
					PromptToksPerSec: TokenSpread{
						Median: 450.0, Min: 400.0, Max: 500.0, StdDev: 25.0,
					},
					GenToksPerSec: TokenSpread{
						Median: 50.0, Min: 45.0, Max: 55.0, StdDev: 2.0,
					},
					Iterations: 3,
					Successful: 3,
					Failed:     0,
				},
			},
			{
				Value: "2048",
				Error: "context length exceeded",
			},
		},
		Duration: 30 * time.Second,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputDepthSweepTable(report)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "Prompt Depth Sweep Results") {
		t.Error("outputDepthSweepTable should contain sweep type in header")
	}
	if !strings.Contains(output, "512") {
		t.Error("outputDepthSweepTable should show depth value 512")
	}
	if !strings.Contains(output, "500") {
		t.Error("outputDepthSweepTable should show achieved depth 500")
	}
	if !strings.Contains(output, "❌") {
		t.Error("outputDepthSweepTable should show failed status for error row")
	}
}

func TestWriteDepthSweepResults(t *testing.T) {
	tmpDir := t.TempDir()
	reportPath := filepath.Join(tmpDir, "depth-report.md")
	opts := &benchmarkOptions{report: reportPath, name: "test-svc", namespace: "test-ns"}

	rw, err := newReportWriter(opts)
	if err != nil {
		t.Fatalf("newReportWriter error: %v", err)
	}
	defer func() { _ = rw.close() }()

	sweepReport := &SweepReport{
		SweepType: "Prompt Depth",
		Values:    []string{"512", "2048"},
		Results: []SweepResult{
			{
				Value: "512",
				Depth: &DepthSweepResult{
					RequestedDepth: 512,
					AchievedDepth:  500,
					PromptToksPerSec: TokenSpread{
						Median: 450.0, Min: 400.0, Max: 500.0, StdDev: 25.0,
					},
					GenToksPerSec: TokenSpread{
						Median: 50.0, Min: 45.0, Max: 55.0, StdDev: 2.0,
					},
					Iterations: 3,
					Successful: 3,
					Failed:     0,
				},
			},
			{
				Value: "2048",
				Error: "context length exceeded",
			},
		},
		Duration: 30 * time.Second,
	}

	if err := rw.writeDepthSweepResults(sweepReport); err != nil {
		t.Fatalf("writeDepthSweepResults error: %v", err)
	}

	content, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("Failed to read report: %v", err)
	}

	reportStr := string(content)
	if !strings.Contains(reportStr, "Prompt Depth Sweep Results") {
		t.Error("Report should contain sweep results section")
	}
	if !strings.Contains(reportStr, "512") {
		t.Error("Report should contain depth value 512")
	}
	if !strings.Contains(reportStr, "500") {
		t.Error("Report should contain achieved depth 500")
	}
}

func TestRunPromptDepthSweepWithMockServer(t *testing.T) {
	// Create a mock server that returns successful responses
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		achievedTokens := 100
		if len(req.Messages) > 0 {
			achievedTokens = len(strings.Fields(req.Messages[0].Content))
		}

		response := ChatCompletionResponse{
			ID:      "test-id",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   "test-model",
			Choices: []struct {
				Index   int `json:"index"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}{
				{
					Index: 0,
					Message: struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					}{
						Role:    "assistant",
						Content: "Test response",
					},
					FinishReason: "stop",
				},
			},
			Usage: struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}{
				PromptTokens:     achievedTokens,
				CompletionTokens: 20,
				TotalTokens:      achievedTokens + 20,
			},
			Timings: struct {
				PromptN             int     `json:"prompt_n"`
				PromptMs            float64 `json:"prompt_ms"`
				PromptPerTokenMs    float64 `json:"prompt_per_token_ms"`
				PromptPerSecond     float64 `json:"prompt_per_second"`
				PredictedN          int     `json:"predicted_n"`
				PredictedMs         float64 `json:"predicted_ms"`
				PredictedPerTokenMs float64 `json:"predicted_per_token_ms"`
				PredictedPerSecond  float64 `json:"predicted_per_second"`
			}{
				PromptN:             achievedTokens,
				PromptMs:            200.0,
				PromptPerTokenMs:    2.0,
				PromptPerSecond:     500.0,
				PredictedN:          20,
				PredictedMs:         400.0,
				PredictedPerTokenMs: 20.0,
				PredictedPerSecond:  50.0,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	opts := &benchmarkOptions{
		name:             "test-service",
		iterations:       2,
		maxTokens:        50,
		timeout:          10 * time.Second,
		promptDepthSweep: "100,200",
		endpoint:         server.URL,
	}

	// Capture stdout to suppress output
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runPromptDepthSweep(opts)

	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	if err != nil {
		t.Fatalf("runPromptDepthSweep failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Prompt Depth Sweep") {
		t.Error("runPromptDepthSweep should print sweep header")
	}
	if !strings.Contains(output, "100") {
		t.Error("runPromptDepthSweep should test depth 100")
	}
	if !strings.Contains(output, "200") {
		t.Error("runPromptDepthSweep should test depth 200")
	}
}

func TestGPUMonitorStart(t *testing.T) {
	// Exercise gpuMonitor.start() to verify it begins sampling and
	// stop() returns collected metrics. This guards the refactored
	// start() signature (no interval parameter).
	gm := newGPUMonitor()
	gm.start()

	// Give the goroutine a moment to collect at least one sample.
	// The mock sample() returns nil (no nvidia-smi), so we just
	// verify start/stop don't panic and stop returns cleanly.
	time.Sleep(100 * time.Millisecond)

	metrics := gm.stop()
	// With no nvidia-smi available, metrics may be empty, but stop()
	// must return without error and the goroutine must terminate.
	_ = metrics
}
