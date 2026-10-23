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
	"strings"
	"testing"
)

func balancedConfig(b *Balancing, m *MetricsProfile) *Config {
	return &Config{Backends: []Backend{{
		Name: "qwen", Tier: "local", Address: "http://qwen.ns.svc.cluster.local:8000",
		EndpointSource: &EndpointSource{Namespace: "ns", Service: "qwen", Port: 8000},
		Balancing:      b, Metrics: m,
	}}}
}

func TestBalancingLoadPercentDefault(t *testing.T) {
	if got := (&Balancing{}).LoadPercent(); got != DefaultMaxLoadPercent {
		t.Errorf("unset LoadPercent = %d, want %d", got, DefaultMaxLoadPercent)
	}
	if got := (&Balancing{MaxLoadPercent: 150}).LoadPercent(); got != 150 {
		t.Errorf("LoadPercent = %d, want 150", got)
	}
}

func TestValidateBalancing(t *testing.T) {
	vllm := &MetricsProfile{Path: "/metrics", Running: "vllm:num_requests_running", Waiting: "vllm:num_requests_waiting", KVUsage: "vllm:kv_cache_usage_perc"}
	for _, ok := range []*Config{
		balancedConfig(&Balancing{Mode: BalancingLoadAware, SessionHeaders: []string{"x-session-affinity"}, MaxLoadPercent: 125}, vllm),
		balancedConfig(&Balancing{Mode: BalancingRoundRobin}, nil),
		balancedConfig(&Balancing{}, nil),
		balancedConfig(&Balancing{SessionHeaders: []string{"X-Session_ID.v2", "!#$%&'*+-.^_`|~"}}, nil),
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("valid config rejected: %v", err)
		}
	}
	for name, bad := range map[string]*Config{
		"unknown mode":            balancedConfig(&Balancing{Mode: "Fastest"}, nil),
		"load percent 100":        balancedConfig(&Balancing{MaxLoadPercent: 100}, nil),
		"load percent 1001":       balancedConfig(&Balancing{MaxLoadPercent: 1001}, nil),
		"empty header name":       balancedConfig(&Balancing{SessionHeaders: []string{""}}, nil),
		"padded header name":      balancedConfig(&Balancing{SessionHeaders: []string{" x-foo "}}, nil),
		"header name with space":  balancedConfig(&Balancing{SessionHeaders: []string{"x session"}}, nil),
		"header name with colon":  balancedConfig(&Balancing{SessionHeaders: []string{"x-session:"}}, nil),
		"header name non-ASCII":   balancedConfig(&Balancing{SessionHeaders: []string{"x-sessión"}}, nil),
		"header name too long":    balancedConfig(&Balancing{SessionHeaders: []string{strings.Repeat("x", 257)}}, nil),
		"profile without path":    balancedConfig(&Balancing{Mode: BalancingLoadAware}, &MetricsProfile{Running: "a", Waiting: "b"}),
		"profile without waiting": balancedConfig(&Balancing{Mode: BalancingLoadAware}, &MetricsProfile{Path: "/metrics", Running: "a"}),
	} {
		err := bad.Validate()
		if err == nil || !(strings.Contains(err.Error(), "balancing") || strings.Contains(err.Error(), "metrics")) {
			t.Errorf("%s: err = %v, want a balancing or metrics error", name, err)
		}
	}
}
