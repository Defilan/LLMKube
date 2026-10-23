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

package router

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// streamingUpstream reads the whole request, sends headers and one SSE chunk,
// then holds the stream open until release is closed or the client leaves.
func streamingUpstream(release <-chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
}

// growth is how far the heap grew from before to after, 0 if it shrank.
func growth(before, after uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// While a response streams, the request body it was sent with must not stay
// in memory: a long-context agent session holds its stream for minutes, and
// hundreds of them at once OOM-killed the proxy at its default limit (#2194).
func TestDispatchDoesNotRetainRequestBodiesWhileStreaming(t *testing.T) {
	for _, tc := range []struct {
		name, servedModel string
	}{
		{"model passed through", ""},
		{"model rewritten", "served-name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const streams, size = 32, 1 << 20
			release := make(chan struct{})
			srv := streamingUpstream(release)
			defer srv.Close()
			defer close(release)
			cfg, b := singleEndpointBackend(srv.URL)
			b.Model = tc.servedModel
			d := NewDispatcher(cfg)
			d.SetEndpoints("qwen", []string{srv.URL})

			before := heapInUse()
			for i := 0; i < streams; i++ {
				body := []byte(`{"model":"alias","messages":[{"role":"user","content":"` + strings.Repeat("a", size) + `"}]}`)
				resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, body)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				if _, err := resp.Body.Read(make([]byte, 64)); err != nil {
					t.Fatal(err)
				}
			}
			grew := growth(before, heapInUse())
			if limit := uint64(streams * size / 4); grew > limit {
				t.Errorf("heap grew %d MiB with %d open streams of %d MiB bodies, want under %d MiB: request bodies are retained while responses stream",
					grew>>20, streams, size>>20, limit>>20)
			}
		})
	}
}

// The same through the whole proxy: the handler's buffered copy, the model
// rewrite and the outbound request must all be gone while the reply streams.
func TestProxyDoesNotRetainRequestBodiesWhileStreaming(t *testing.T) {
	const streams, size = 32, 1 << 20
	release := make(chan struct{})
	upstream := streamingUpstream(release)
	defer upstream.Close()
	defer close(release)

	cfg := &Config{
		Backends:     []Backend{{Name: "local", Tier: "local", Address: upstream.URL, Model: "served-name"}},
		DefaultRoute: "local",
		Policy:       Policy{Classification: ClassificationPolicy{Mode: "header-only"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Mount(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	before := heapInUse()
	for i := 0; i < streams; i++ {
		body := []byte(`{"model":"alias","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("a", size) + `"}]}`)
		// The client's own copy must not count, so it goes out through a
		// body that drops its bytes once sent, like the proxy's.
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", newSentOnceBody(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := resp.Body.Read(make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
	}
	grew := growth(before, heapInUse())
	if limit := uint64(streams * size / 4); grew > limit {
		t.Errorf("heap grew %d MiB with %d open proxied streams of %d MiB bodies, want under %d MiB",
			grew>>20, streams, size>>20, limit>>20)
	}
}
