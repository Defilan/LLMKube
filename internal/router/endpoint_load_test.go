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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/defilantech/llmkube/internal/router/picker"
)

func singleEndpointBackend(url string) (*Config, *Backend) {
	cfg := &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: url,
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
	}}}
	return cfg, &cfg.Backends[0]
}

func inflight(d *Dispatcher, ep string) int64 { return d.loadFor("qwen", ep).inflight.Load() }

func TestDispatchReleasesInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	cfg, b := singleEndpointBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})

	resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/v1/chat/completions", http.Header{}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := inflight(d, srv.URL); got != 1 {
		t.Errorf("in-flight while the body is open = %d, want 1", got)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = resp.Body.Close() // a second Close must not release twice
	if got := inflight(d, srv.URL); got != 0 {
		t.Errorf("in-flight after Close = %d, want 0", got)
	}
}

func TestDispatchReleasesInFlightOnFailureAndCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a stream the client abandons
	}))
	defer srv.Close()
	cfg, b := singleEndpointBackend(srv.URL)
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{srv.URL})

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := d.Dispatch(ctx, b, http.MethodPost, "/v1/chat/completions", http.Header{}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close() // closed without reading to the end
	if got := inflight(d, srv.URL); got != 0 {
		t.Errorf("in-flight after an abandoned stream = %d, want 0", got)
	}

	dead := "http://127.0.0.1:1"
	d.SetEndpoints("qwen", []string{dead})
	if resp, err := d.Dispatch(context.Background(), b, http.MethodPost, "/x", http.Header{}, []byte(`{}`)); err == nil {
		_ = resp.Body.Close()
		t.Fatal("dispatch to a closed port succeeded")
	}
	if got := inflight(d, dead); got != 0 {
		t.Errorf("in-flight after a failed request = %d, want 0", got)
	}
}

func TestRecordSampleStampsInFlightAndClearsOnLeave(t *testing.T) {
	cfg, _ := singleEndpointBackend("http://unused")
	d := NewDispatcher(cfg)
	d.SetEndpoints("qwen", []string{"http://10.0.0.1:8000", "http://10.0.0.2:8000"})
	d.loadFor("qwen", "http://10.0.0.1:8000").inflight.Store(3)
	d.RecordSample("qwen", "http://10.0.0.1:8000", picker.Sample{Running: 2, At: time.Now()})
	s := d.loadFor("qwen", "http://10.0.0.1:8000").sample.Load()
	if s == nil || s.InFlightAt != 3 || s.Running != 2 {
		t.Fatalf("stored sample = %+v, want Running 2 and InFlightAt 3", s)
	}
	if got := d.EndpointsOf("qwen"); len(got) != 2 {
		t.Fatalf("EndpointsOf = %v, want 2 endpoints", got)
	}
	d.SetEndpoints("qwen", []string{"http://10.0.0.2:8000"})
	if s := d.loadFor("qwen", "http://10.0.0.1:8000").sample.Load(); s != nil {
		t.Errorf("a departed endpoint kept its sample: %+v", s)
	}
}
