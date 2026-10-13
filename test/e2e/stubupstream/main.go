// Package main is a minimal HTTP server used by the ModelRouter cluster
// e2e tests as a fake "local InferenceService" or "cloud provider"
// upstream. The binary speaks just enough of the OpenAI chat-completions
// shape for the router-proxy to dispatch real requests at it, plus an
// /__introspect__ endpoint the tests use to assert which upstream
// received which request (and with what headers).
//
// This is intentionally not part of the operator's public surface; it
// lives under test/e2e/ and is only built into a container image during
// the e2e job.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type recordedRequest struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	Headers  map[string][]string `json:"headers"`
	Body     string              `json:"body"`
	At       time.Time           `json:"at"`
	Streamed bool                `json:"streamed"`
}

type recorder struct {
	mu          sync.Mutex
	records     []recordedRequest
	healthCalls []recordedRequest
}

func (r *recorder) record(req recordedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, req)
	if len(r.records) > 100 {
		r.records = r.records[len(r.records)-100:]
	}
}

func (r *recorder) recordHealth(req recordedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthCalls = append(r.healthCalls, req)
	if len(r.healthCalls) > 100 {
		r.healthCalls = r.healthCalls[len(r.healthCalls)-100:]
	}
}

func (r *recorder) snapshot() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.records))
	copy(out, r.records)
	return out
}

func (r *recorder) healthSnapshot() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.healthCalls))
	copy(out, r.healthCalls)
	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
	r.healthCalls = nil
}

// stubConfig holds the runtime knobs for a stub-upstream instance,
// assembled by main from CLI flags plus the VLLM_API_KEY environment
// variable (so newMux can be exercised directly from unit tests without
// touching flags or the environment).
type stubConfig struct {
	label         string
	responseDelay time.Duration
	chunkDelay    time.Duration
	apiKey        string
}

// keyEnforcedPrefixes are the path prefixes vLLM's own server guards
// with its API key middleware. /health, /healthz, and the test-only
// /__introspect__* endpoints are intentionally left out: kubelet probes
// and the e2e harness must be able to reach them regardless of
// cfg.apiKey.
var keyEnforcedPrefixes = []string{"/v1", "/v2", "/inference"}

func requiresKey(path string) bool {
	for _, prefix := range keyEnforcedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// newMux builds the stub's HTTP handler. Kept separate from main so unit
// tests can exercise the routing, key-enforcement, and streaming
// behavior directly with httptest instead of spawning a real listener.
func newMux(cfg stubConfig, rec *recorder) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		rec.recordHealth(recordedRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Headers: cloneHeader(r.Header),
			At:      time.Now().UTC(),
		})
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("GET /__introspect__", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"label":    cfg.label,
			"requests": rec.snapshot(),
		})
	})
	mux.HandleFunc("GET /__introspect__/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"label":    cfg.label,
			"requests": rec.healthSnapshot(),
		})
	})
	mux.HandleFunc("POST /__introspect__/reset", func(w http.ResponseWriter, _ *http.Request) {
		rec.reset()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		stream := false
		includeUsage := false
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err == nil {
			if v, ok := parsed["stream"].(bool); ok {
				stream = v
			}
			if so, ok := parsed["stream_options"].(map[string]any); ok {
				if v, ok := so["include_usage"].(bool); ok {
					includeUsage = v
				}
			}
		}

		rec.record(recordedRequest{
			Method:   r.Method,
			Path:     r.URL.Path,
			Headers:  cloneHeader(r.Header),
			Body:     string(body),
			At:       time.Now().UTC(),
			Streamed: stream,
		})

		if cfg.responseDelay > 0 {
			// Honor the client's deadline if it surrenders first: a
			// proxy with a tight context deadline will close the conn,
			// at which point r.Context() is canceled and we abandon
			// the sleep. This keeps the test deterministic instead of
			// waiting the full configured delay.
			select {
			case <-time.After(cfg.responseDelay):
			case <-r.Context().Done():
				return
			}
		}

		if stream {
			writeStreamed(w, r, cfg.label, cfg.chunkDelay, includeUsage)
			return
		}
		writeNonStreamed(w, cfg.label)
	})

	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]string{
				{"id": cfg.label, "object": "model"},
			},
		})
	})

	var handler http.Handler = mux
	if cfg.apiKey != "" {
		handler = enforceKey(cfg.apiKey, mux)
	}
	return handler
}

// enforceKey mirrors vLLM's own API-key middleware: any request whose
// path starts with /v1, /v2, or /inference must carry
// "Authorization: Bearer <key>", or it gets vLLM's 401 body. Everything
// else (health checks, the test-only introspection endpoints) passes
// through untouched.
func enforceKey(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requiresKey(r.URL.Path) && r.Header.Get("Authorization") != "Bearer "+apiKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	label := flag.String("label", "stub",
		"identifier returned in chat completion content so tests can tell upstreams apart")
	listen := flag.String("listen", ":8080", "address to listen on")
	responseDelay := flag.Duration("response-delay", 0,
		"artificial delay before writing the chat-completion response. "+
			"Lets the per-rule / per-backend timeout e2e tests force the "+
			"proxy's context deadline to fire by simulating a slow upstream.")
	chunkDelay := flag.Duration("chunk-delay", 0,
		"artificial delay between streamed chunks. Lets tests that "+
			"exercise mid-stream behavior (stall detection, client "+
			"disconnects) observe a response that trickles in over "+
			"time instead of arriving all at once.")
	flag.Parse()

	cfg := stubConfig{
		label:         *label,
		responseDelay: *responseDelay,
		chunkDelay:    *chunkDelay,
		apiKey:        os.Getenv("VLLM_API_KEY"),
	}

	rec := &recorder{}
	mux := newMux(cfg, rec)

	log.Printf("stub-upstream label=%s listen=%s", cfg.label, *listen)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func cloneHeader(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		dup := make([]string, len(vs))
		copy(dup, vs)
		out[k] = dup
	}
	return out
}

func writeNonStreamed(w http.ResponseWriter, label string) {
	resp := map[string]any{
		"id":      "stub-" + label + "-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   label,
		"choices": []map[string]any{
			{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": "stub-response-from-" + label,
				},
			},
		},
		"usage": usage(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// usage returns the stub's fixed token-usage figures. It is a function
// (rather than a package-level map literal reused across responses) so
// every caller gets its own map, safe to marshal concurrently.
//
// encoding/json always serializes map[string]T keys in sorted order,
// so this comes out as completion_tokens, prompt_tokens, total_tokens --
// alphabetical, and matching vLLM's wire shape byte-for-byte.
func usage() map[string]int {
	return map[string]int{
		"completion_tokens": 3,
		"prompt_tokens":     7,
		"total_tokens":      10,
	}
}

func writeStreamed(w http.ResponseWriter, r *http.Request, label string, chunkDelay time.Duration, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	chunks := []map[string]any{
		{
			"id":      "stub-stream-" + label,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   label,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"role": "assistant"}},
			},
		},
		{
			"id":      "stub-stream-" + label,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   label,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"content": "stub-response-from-" + label}},
			},
		},
		{
			"id":      "stub-stream-" + label,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   label,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			},
		},
	}
	if includeUsage {
		chunks = append(chunks, map[string]any{
			"choices": []map[string]any{},
			"usage":   usage(),
		})
	}

	for i, c := range chunks {
		if i > 0 && chunkDelay > 0 {
			select {
			case <-time.After(chunkDelay):
			case <-r.Context().Done():
				return
			}
		}
		data, _ := json.Marshal(c)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
