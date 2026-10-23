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
	"io"
	"sync"
	"sync/atomic"

	"github.com/defilantech/llmkube/internal/router/picker"
)

// endpointLoad is what placement reads for one endpoint of one backend.
type endpointLoad struct {
	inflight atomic.Int64
	sample   atomic.Pointer[picker.Sample]
}

// loadFor returns the load record for a backend's endpoint, creating it.
func (d *Dispatcher) loadFor(backend, endpoint string) *endpointLoad {
	key := healthKey(backend, endpoint)
	if v, ok := d.loads.Load(key); ok {
		return v.(*endpointLoad)
	}
	actual, _ := d.loads.LoadOrStore(key, &endpointLoad{})
	return actual.(*endpointLoad)
}

// RecordSample stores an engine reading for a backend's endpoint, stamped
// with this replica's in-flight count at the moment it is stored.
func (d *Dispatcher) RecordSample(backend, endpoint string, s picker.Sample) {
	l := d.loadFor(backend, endpoint)
	s.InFlightAt = l.inflight.Load()
	l.sample.Store(&s)
}

// EndpointsOf returns the live endpoint set of a backend (nil when none).
func (d *Dispatcher) EndpointsOf(backend string) []string {
	if v, ok := d.endpoints.Load(backend); ok {
		return v.([]string)
	}
	return nil
}

// track counts a request against an endpoint and returns the release
// function, which is safe to call more than once.
func (d *Dispatcher) track(backend, endpoint string) func() {
	if endpoint == "" {
		return func() {}
	}
	l := d.loadFor(backend, endpoint)
	l.inflight.Add(1)
	var once sync.Once
	return func() { once.Do(func() { l.inflight.Add(-1) }) }
}

// releaseOnCloseBody releases an in-flight count when the body is closed.
type releaseOnCloseBody struct {
	io.ReadCloser
	release func()
}

func (b *releaseOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}
