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
	mu      sync.Mutex
	records []recordedRequest
}

func (r *recorder) record(req recordedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, req)
	if len(r.records) > 100 {
		r.records = r.records[len(r.records)-100:]
	}
}

func (r *recorder) snapshot() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.records))
	copy(out, r.records)
	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
}

func main() {
	label := flag.String("label", "stub",
		"identifier returned in chat completion content so tests can tell upstreams apart")
	listen := flag.String("listen", ":8080", "address to listen on")
	flag.Parse()

	rec := &recorder{}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("GET /__introspect__", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"label":    *label,
			"requests": rec.snapshot(),
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
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err == nil {
			if v, ok := parsed["stream"].(bool); ok {
				stream = v
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

		if stream {
			writeStreamed(w, *label)
			return
		}
		writeNonStreamed(w, *label)
	})

	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]string{
				{"id": *label, "object": "model"},
			},
		})
	})

	log.Printf("stub-upstream label=%s listen=%s", *label, *listen)
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
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeStreamed(w http.ResponseWriter, label string) {
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
	for _, c := range chunks {
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
