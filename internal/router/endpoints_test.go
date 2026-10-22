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
	"reflect"
	"strings"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
)

func slice(ready *bool, addrs ...string) *discoveryv1.EndpointSlice {
	eps := make([]discoveryv1.Endpoint, 0, len(addrs))
	for _, a := range addrs {
		eps = append(eps, discoveryv1.Endpoint{Addresses: []string{a}, Conditions: discoveryv1.EndpointConditions{Ready: ready}})
	}
	return &discoveryv1.EndpointSlice{Endpoints: eps}
}

func TestReadyEndpointURLs(t *testing.T) {
	yes, no := true, false
	got := ReadyEndpointURLs([]*discoveryv1.EndpointSlice{
		slice(&yes, "10.0.0.1"),
		slice(&no, "10.0.0.2"),
		slice(nil, "10.0.0.3"), // nil readiness counts as ready, per the EndpointSlice API
	}, 8080)
	want := []string{"http://10.0.0.1:8080", "http://10.0.0.3:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadyEndpointURLs = %v, want %v", got, want)
	}
	if got := ReadyEndpointURLs(nil, 8080); len(got) != 0 {
		t.Errorf("no slices: %v, want none", got)
	}
}

func TestValidateEndpointSource(t *testing.T) {
	base := func(src *EndpointSource) *Config {
		return &Config{Backends: []Backend{{
			Name: "b", Tier: "local", Address: "http://svc.ns.svc.cluster.local:8080", EndpointSource: src,
		}}}
	}
	if err := base(&EndpointSource{Namespace: "ns", Service: "svc", Port: 8080}).Validate(); err != nil {
		t.Fatalf("complete source rejected: %v", err)
	}
	for _, src := range []*EndpointSource{
		{Service: "svc", Port: 8080},
		{Namespace: "ns", Port: 8080},
		{Namespace: "ns", Service: "svc"},
	} {
		err := base(src).Validate()
		if err == nil || !strings.Contains(err.Error(), "endpointSource") {
			t.Errorf("incomplete source %+v: err = %v, want an endpointSource error", src, err)
		}
	}
}
