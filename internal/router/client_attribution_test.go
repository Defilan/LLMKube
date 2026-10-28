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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientAddress(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"10.0.0.0/8", "192.0.2.7"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		peer    string
		xff     []string
		trusted bool
		want    string
	}{
		{"untrusted peer ignores a spoofed header", "198.51.100.4:5000", []string{"203.0.113.9"}, true, "198.51.100.4"},
		{"no trusted proxies ignores the header", "10.1.1.1:5000", []string{"203.0.113.9"}, false, "10.1.1.1"},
		{"trusted peer, one hop", "10.1.1.1:5000", []string{"203.0.113.9"}, true, "203.0.113.9"},
		{"trusted hops are skipped from the right", "10.1.1.1:5000", []string{"203.0.113.9, 10.2.2.2, 192.0.2.7"}, true, "203.0.113.9"},
		{"a value the client prepended is not believed", "10.1.1.1:5000", []string{"6.6.6.6, 203.0.113.9"}, true, "203.0.113.9"},
		{"several header lines are one list", "10.1.1.1:5000", []string{"6.6.6.6", "203.0.113.9"}, true, "203.0.113.9"},
		{"every hop trusted gives the leftmost", "10.1.1.1:5000", []string{"10.3.3.3, 10.2.2.2"}, true, "10.3.3.3"},
		{"garbage stops the walk at the last trusted hop", "10.1.1.1:5000", []string{"203.0.113.9, not-an-ip"}, true, "10.1.1.1"},
		{"hop with a port", "10.1.1.1:5000", []string{"203.0.113.9:4711"}, true, "203.0.113.9"},
		{"no header from a trusted peer", "10.1.1.1:5000", nil, true, "10.1.1.1"},
		{"IPv4-mapped peer matches an IPv4 prefix", "[::ffff:10.1.1.1]:5000", []string{"203.0.113.9"}, true, "203.0.113.9"},
		{"IPv6 peer", "[2001:db8::1]:5000", nil, true, "2001:db8::1"},
		{"unparseable peer", "@", nil, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.RemoteAddr = tc.peer
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			tp := trusted
			if !tc.trusted {
				tp = nil
			}
			if got := clientAddress(r, tp); got != tc.want {
				t.Errorf("clientAddress = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCallerValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-LLMKube-Client", "opencode/1.2 (ci)")
	if got := callerValue(r, "x-llmkube-client"); got != "opencode/1.2 (ci)" {
		t.Errorf("callerValue = %q, want the value as sent", got)
	}

	r.Header.Set("X-LLMKube-Client", "café\tbot")
	if got := callerValue(r, "X-LLMKube-Client"); got != "caf??bot" {
		t.Errorf("callerValue = %q, want each non-printable character replaced", got)
	}

	r.Header.Set("X-LLMKube-Client", strings.Repeat("a", 500))
	if got := callerValue(r, "X-LLMKube-Client"); len(got) != maxCallerLen {
		t.Errorf("callerValue length = %d, want it cut to %d", len(got), maxCallerLen)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies([]string{" 10.0.0.0/8 ", "192.0.2.7", "2001:db8::/32", "10.1.2.3/8"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "192.0.2.7/32", "2001:db8::/32", "10.0.0.0/8"}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("prefix %d = %s, want %s", i, got[i], want[i])
		}
	}
	if _, err := ParseTrustedProxies([]string{"10.0.0.0/8", "ingress"}); err == nil ||
		!strings.Contains(err.Error(), "trustedProxyCIDRs[1]") {
		t.Errorf("err = %v, want it to name entry 1", err)
	}
}

func TestConfigValidateRejectsBadAuditAttribution(t *testing.T) {
	base := func() *Config {
		return &Config{
			Backends:     []Backend{{Name: "b", Tier: "local", Address: "http://b"}},
			DefaultRoute: "b",
		}
	}
	c := base()
	c.Policy.AuditLog.TrustedProxyCIDRs = []string{"10.0.0.0/33"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "trustedProxyCIDRs[0]") {
		t.Errorf("Validate = %v, want the bad CIDR refused", err)
	}
	for _, h := range []string{"Authorization", "cookie", "X-Api-Key"} {
		c = base()
		c.Policy.AuditLog.CallerHeader = h
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "credential") {
			t.Errorf("Validate with callerHeader %s = %v, want it refused", h, err)
		}
	}
	c = base()
	c.Policy.AuditLog = AuditLogPolicy{LogClientAddress: true, TrustedProxyCIDRs: []string{"10.0.0.0/8"}, CallerHeader: "X-LLMKube-Client"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate = %v, want a valid attribution config accepted", err)
	}
}

// The audit line names the caller only when policy.auditLog opts in, a
// spoofed X-Forwarded-For from an untrusted peer is ignored at the request
// path, and the attribution is on every audit line, failures included.
func TestAuditClientAttribution(t *testing.T) {
	be := newFakeBackend(t)
	post := func(t *testing.T, cfg *Config, headers map[string]string) string {
		t.Helper()
		srv, logs := newOutcomeProxy(t, cfg)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return logs.dispatchLine(t)
	}
	config := func(addr string, audit AuditLogPolicy) *Config {
		return &Config{
			Backends:     []Backend{{Name: "b", Tier: "local", Address: addr}},
			DefaultRoute: "b",
			Policy:       Policy{AuditLog: audit},
		}
	}
	spoof := map[string]string{"X-Forwarded-For": "6.6.6.6", "X-LLMKube-Client": "opencode"}

	t.Run("off by default", func(t *testing.T) {
		line := post(t, config(be.URL(), AuditLogPolicy{}), spoof)
		if strings.Contains(line, "client=") || strings.Contains(line, "caller=") {
			t.Errorf("audit line = %s\nwant no client or caller without the opt-in", line)
		}
	})
	t.Run("peer address, spoofed header ignored", func(t *testing.T) {
		line := post(t, config(be.URL(), AuditLogPolicy{LogClientAddress: true}), spoof)
		if !strings.Contains(line, "client=127.0.0.1") || strings.Contains(line, "6.6.6.6") {
			t.Errorf("audit line = %s\nwant client=127.0.0.1, the TCP peer, and not the spoofed address", line)
		}
	})
	t.Run("forwarded address behind a trusted proxy", func(t *testing.T) {
		audit := AuditLogPolicy{LogClientAddress: true, TrustedProxyCIDRs: []string{"127.0.0.0/8"}}
		line := post(t, config(be.URL(), audit), map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.9"})
		if !strings.Contains(line, "client=203.0.113.9") {
			t.Errorf("audit line = %s\nwant client=203.0.113.9, the hop the trusted proxy appended", line)
		}
	})
	t.Run("caller header", func(t *testing.T) {
		line := post(t, config(be.URL(), AuditLogPolicy{CallerHeader: "X-LLMKube-Client"}), spoof)
		if !strings.Contains(line, "caller=opencode") || strings.Contains(line, "client=") {
			t.Errorf("audit line = %s\nwant caller=opencode and no client", line)
		}
	})
	t.Run("on a failed dispatch too", func(t *testing.T) {
		audit := AuditLogPolicy{LogClientAddress: true, CallerHeader: "X-LLMKube-Client"}
		line := post(t, config("http://127.0.0.1:1", audit), spoof)
		if !strings.Contains(line, "outcome=all_backends_failed") ||
			!strings.Contains(line, "client=127.0.0.1") || !strings.Contains(line, "caller=opencode") {
			t.Errorf("audit line = %s\nwant the failure attributed", line)
		}
	})
}
