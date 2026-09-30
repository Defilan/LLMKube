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

package safehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// testHint is the refusal-message hint used by tests that do not care what
// operator-facing setting it names.
const testHint = "modelSource.allowedRemoteHosts"

func TestIPBlocked(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		// Blocked: loopback, link-local, RFC-1918, ULA, unspecified.
		{"127.0.0.1", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true}, // IPv4-in-IPv6 loopback must not slip through
		{"169.254.169.254", true},  // cloud metadata
		{"fe80::1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"fc00::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"::ffff:10.0.0.1", true}, // IPv4-in-IPv6 RFC-1918
		// Blocked: ranges IsPrivate() does not cover (GHSA-jw3m-8q7m-f35r).
		{"100.64.0.1", true},         // CGNAT (RFC 6598)
		{"100.93.59.11", true},       // CGNAT: Tailscale address in this env
		{"192.0.0.1", true},          // IETF protocol assignments (RFC 6890)
		{"198.18.0.1", true},         // benchmarking (RFC 2544)
		{"198.19.255.1", true},       // benchmarking upper half of the /15
		{"192.88.99.1", true},        // 6to4 relay anycast (RFC 3068)
		{"64:ff9b::7f00:1", true},    // NAT64 (RFC 6052) embedding 127.0.0.1
		{"64:ff9b::a00:1", true},     // NAT64 embedding 10.0.0.1
		{"64:ff9b::a9fe:a9fe", true}, // NAT64 embedding 169.254.169.254
		{"64:ff9b::6440:1", true},    // NAT64 embedding CGNAT 100.64.0.1
		{"64:ff9b:1::a", true},       // NAT64 local-use (RFC 8215)
		{"::169.254.169.254", true},  // IPv4-compatible IPv6 hiding metadata IP
		{"::127.0.0.1", true},        // IPv4-compatible IPv6 loopback
		{"::ffff:100.64.0.1", true},  // IPv4-mapped CGNAT
		// Not blocked: public addresses (incl. boundary neighbors of the new ranges).
		{"1.1.1.1", false},
		{"8.8.8.8", false},
		{"2606:4700:4700::1111", false},
		{"100.128.0.1", false}, // just above 100.64.0.0/10
		{"198.20.0.1", false},  // just above 198.18.0.0/15
		// DNS64 synthesizes 64:ff9b::/96 for IPv4-only public hosts; those
		// must stay reachable (classified by the embedded IPv4 address).
		{"64:ff9b::808:808", false}, // NAT64 embedding 8.8.8.8
		{"64:ff9b::101:101", false}, // NAT64 embedding 1.1.1.1
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := netip.MustParseAddr(tc.ip)
			if got := IPBlocked(ip); got != tc.blocked {
				t.Errorf("IPBlocked(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

func TestParseAllowlist(t *testing.T) {
	al := ParseAllowlist([]string{
		"10.20.0.0/16",
		"10.9.9.9",
		"Artifact.Corp",
		"  ",
		"",
	})

	ipCases := []struct {
		ip      string
		allowed bool
	}{
		{"10.20.5.5", true},        // inside CIDR
		{"10.30.5.5", false},       // outside CIDR
		{"10.9.9.9", true},         // bare IP became a /32
		{"10.9.9.10", false},       // adjacent IP not covered by the /32
		{"::ffff:10.20.5.5", true}, // 4-in-6 form of an allowlisted IPv4
	}
	for _, tc := range ipCases {
		if got := al.ipAllowed(netip.MustParseAddr(tc.ip)); got != tc.allowed {
			t.Errorf("ipAllowed(%s) = %v, want %v", tc.ip, got, tc.allowed)
		}
	}

	hostCases := []struct {
		host    string
		allowed bool
	}{
		{"artifact.corp", true}, // case-insensitive match
		{"ARTIFACT.CORP", true},
		{"other.corp", false},
		{"", false}, // blank entries were dropped, not registered
	}
	for _, tc := range hostCases {
		if got := al.hostAllowed(tc.host); got != tc.allowed {
			t.Errorf("hostAllowed(%q) = %v, want %v", tc.host, got, tc.allowed)
		}
	}
}

// TestGuardedClientBlocksLoopback is the end-to-end proof the guard fires: a
// guarded client with an empty allowlist must refuse to connect to a loopback
// httptest server (the SSRF-relevant target class), and the same client with
// 127.0.0.1 allowlisted must succeed.
func TestGuardedClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	t.Run("empty allowlist refuses loopback", func(t *testing.T) {
		client := NewClient(ParseAllowlist(nil), 5*time.Second, testHint)
		resp, err := client.Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("expected the SSRF guard to block %s, but the request succeeded", srv.URL)
		}
		if !strings.Contains(err.Error(), "SSRF guard") {
			t.Errorf("expected an SSRF-guard error, got: %v", err)
		}
	})

	t.Run("allowlisted loopback is permitted", func(t *testing.T) {
		client := NewClient(ParseAllowlist([]string{"127.0.0.1"}), 5*time.Second, testHint)
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("expected allowlisted request to succeed, got: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		if string(body) != "ok" {
			t.Errorf("unexpected body %q", body)
		}
	})

	t.Run("CIDR allowlist covering loopback is permitted", func(t *testing.T) {
		client := NewClient(ParseAllowlist([]string{"127.0.0.0/8"}), 5*time.Second, testHint)
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("expected CIDR-allowlisted request to succeed, got: %v", err)
		}
		_ = resp.Body.Close()
	})
}

// TestGuardedClientRedirects verifies redirect hops dial through the same
// guard: with loopback allowlisted a 302 chain works end-to-end, and the hop
// count is capped.
func TestGuardedClientRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("target"))
	}))
	defer target.Close()

	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer hop.Close()

	t.Run("allowlisted redirect chain succeeds", func(t *testing.T) {
		client := NewClient(ParseAllowlist([]string{"127.0.0.1"}), 5*time.Second, testHint)
		resp, err := client.Get(hop.URL)
		if err != nil {
			t.Fatalf("expected allowlisted redirect to succeed, got: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "target" {
			t.Errorf("expected to land on target, got body %q", body)
		}
	})

	t.Run("blocked redirect target is refused at dial", func(t *testing.T) {
		// Allowlist ONLY the first hop's exact host:port would not be expressible
		// (allowlist is host-level), so instead prove the guard fires on the hop:
		// with an empty allowlist even the first loopback hop is refused.
		client := NewClient(ParseAllowlist(nil), 5*time.Second, testHint)
		resp, err := client.Get(hop.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected the SSRF guard to block the redirecting server")
		}
	})

	t.Run("redirect hops are capped", func(t *testing.T) {
		var loop *httptest.Server
		loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, loop.URL, http.StatusFound)
		}))
		defer loop.Close()

		client := NewClient(ParseAllowlist([]string{"127.0.0.1"}), 5*time.Second, testHint)
		resp, err := client.Get(loop.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected the redirect loop to be capped")
		}
		if !strings.Contains(err.Error(), "redirect") {
			t.Errorf("expected a redirect-cap error, got: %v", err)
		}
	})
}

// TestGuardedClientHostnameAllowlist proves a hostname entry re-permits a name
// that resolves to a blocked IP. "localhost" resolves to loopback, so with
// "localhost" allowlisted the request must succeed even though every resolved
// IP is blocked.
func TestGuardedClientHostnameAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// Rewrite 127.0.0.1 -> localhost so the dialer takes the DNS path.
	target := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	t.Run("hostname not allowlisted is refused", func(t *testing.T) {
		client := NewClient(ParseAllowlist(nil), 5*time.Second, testHint)
		resp, err := client.Get(target)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected the SSRF guard to block localhost")
		}
	})

	t.Run("hostname allowlisted is permitted", func(t *testing.T) {
		client := NewClient(ParseAllowlist([]string{"LocalHost"}), 5*time.Second, testHint)
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("expected hostname-allowlisted request to succeed, got: %v", err)
		}
		_ = resp.Body.Close()
	})
}

// TestNewClientRefusalIncludesHint proves the refusal message names the
// caller's own operator-facing setting rather than a hardcoded one, so each
// caller (controller flag, agent flag) gets accurate guidance.
func TestNewClientRefusalIncludesHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	const hint = "--allowed-download-hosts"
	client := NewClient(ParseAllowlist(nil), 5*time.Second, hint)
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the SSRF guard to block loopback")
	}
	if !strings.Contains(err.Error(), hint) {
		t.Errorf("expected the refusal error to mention hint %q, got: %v", hint, err)
	}
}

// TestWithResolver proves the resolver seam replaces DNS for hostname dials
// and that the guard still judges the addresses it returns: a hostname that
// resolves to loopback is refused unless the hostname is allowlisted, and a
// nil resolver keeps the default.
func TestWithResolver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	loopback := func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host != "mirror.test" {
			return nil, errors.New("unexpected lookup of " + host)
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	target := strings.Replace(srv.URL, "127.0.0.1", "mirror.test", 1)

	t.Run("resolved loopback refused when not allowlisted", func(t *testing.T) {
		client := NewClient(ParseAllowlist(nil), 5*time.Second, testHint, WithResolver(loopback))
		resp, err := client.Get(target)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected the SSRF guard to block mirror.test resolved to loopback")
		}
		if !strings.Contains(err.Error(), "mirror.test") {
			t.Errorf("refusal should name the host, got: %v", err)
		}
	})

	t.Run("allowlisted hostname dials the resolved address", func(t *testing.T) {
		client := NewClient(ParseAllowlist([]string{"mirror.test"}), 5*time.Second, testHint,
			WithResolver(loopback))
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("expected allowlisted mirror.test to succeed, got: %v", err)
		}
		_ = resp.Body.Close()
	})

	t.Run("nil resolver keeps the default", func(t *testing.T) {
		client := NewClient(ParseAllowlist([]string{"localhost"}), 5*time.Second, testHint, WithResolver(nil))
		resp, err := client.Get(strings.Replace(srv.URL, "127.0.0.1", "localhost", 1))
		if err != nil {
			t.Fatalf("expected default resolution of localhost to succeed, got: %v", err)
		}
		_ = resp.Body.Close()
	})
}

// proxyEnv points HTTP_PROXY at proxyURL for the test and clears every other
// proxy variable, so the host environment cannot leak in. NewClient reads the
// proxy settings from the environment when it is called (httpproxy
// FromEnvironment, not http.ProxyFromEnvironment, which caches the
// environment for the life of the process and so could not be changed per
// test), so each test builds its client after setting the environment.
func proxyEnv(t *testing.T, proxyURL, noProxy string) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
		t.Setenv(k, "")
	}
	t.Setenv("HTTP_PROXY", proxyURL)
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("NO_PROXY", noProxy)
}

// fakeProxy is a forward proxy on 127.0.0.1 that never forwards: it records
// each absolute-URI request and answers it itself, so no real host is
// contacted. redirects maps a target host to a Location to answer with.
func fakeProxy(t *testing.T, redirects map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if loc, ok := redirects[r.URL.Hostname()]; ok {
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "via-proxy:"+r.URL.Hostname()) //nolint:gosec // test proxy, plain text
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// proxyTestLookup resolves the proxy test hostnames without real DNS.
func proxyTestLookup(_ context.Context, _, host string) ([]netip.Addr, error) {
	records := map[string]string{
		"public.test":   "8.8.8.8",
		"internal.test": "10.0.0.5",
	}
	ip, ok := records[host]
	if !ok {
		return nil, errors.New("test resolver: no record for " + host)
	}
	return []netip.Addr{netip.MustParseAddr(ip)}, nil
}

// With a proxy configured, the guard judges the request's TARGET before the
// request is handed to the proxy (the proxy, not us, dials the target). The
// proxy itself is operator-configured and trusted, so a proxy on loopback or
// RFC 1918 is dialed even though those ranges are blocked for targets.
func TestGuardedClientProxy(t *testing.T) {
	const hint = "--allowed-download-hosts"

	t.Run("public host goes via the proxy, proxy on loopback is dialed", func(t *testing.T) {
		proxy, hits := fakeProxy(t, nil)
		proxyEnv(t, proxy.URL, "")
		client := NewClient(ParseAllowlist(nil), 5*time.Second, hint, WithResolver(proxyTestLookup))
		resp, err := client.Get("http://public.test/model.gguf")
		if err != nil {
			t.Fatalf("public host via proxy: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "via-proxy:public.test" || hits.Load() != 1 {
			t.Errorf("body = %q, proxy hits = %d; want the proxy to serve it once", body, hits.Load())
		}
	})

	for _, target := range []string{"http://internal.test/model.gguf", "http://10.0.0.5/model.gguf"} {
		t.Run("blocked target refused before reaching the proxy: "+target, func(t *testing.T) {
			proxy, hits := fakeProxy(t, nil)
			proxyEnv(t, proxy.URL, "")
			client := NewClient(ParseAllowlist(nil), 5*time.Second, hint, WithResolver(proxyTestLookup))
			resp, err := client.Get(target)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("blocked target via proxy was not refused")
			}
			if !strings.Contains(err.Error(), "SSRF guard") || !strings.Contains(err.Error(), hint) {
				t.Errorf("refusal %q should be the guard error naming %s", err, hint)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("proxy received %d requests for a blocked target, want 0", n)
			}
		})
	}

	t.Run("allowlisted private host goes via the proxy", func(t *testing.T) {
		proxy, hits := fakeProxy(t, nil)
		proxyEnv(t, proxy.URL, "")
		client := NewClient(ParseAllowlist([]string{"internal.test"}), 5*time.Second, hint,
			WithResolver(proxyTestLookup))
		resp, err := client.Get("http://internal.test/model.gguf")
		if err != nil {
			t.Fatalf("allowlisted private host via proxy: %v", err)
		}
		_ = resp.Body.Close()
		if hits.Load() != 1 {
			t.Errorf("proxy hits = %d, want 1", hits.Load())
		}
	})

	t.Run("proxied redirect to a blocked host is refused", func(t *testing.T) {
		proxy, hits := fakeProxy(t, map[string]string{"public.test": "http://internal.test/secret"})
		proxyEnv(t, proxy.URL, "")
		client := NewClient(ParseAllowlist(nil), 5*time.Second, hint, WithResolver(proxyTestLookup))
		resp, err := client.Get("http://public.test/model.gguf")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("redirect to a blocked host via proxy was not refused")
		}
		if !strings.Contains(err.Error(), "internal.test") || !strings.Contains(err.Error(), hint) {
			t.Errorf("refusal %q should name internal.test and %s", err, hint)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("proxy hits = %d, want exactly 1 (the redirect hop must not reach it)", n)
		}
	})

	t.Run("proxy address as a direct target gets no trust", func(t *testing.T) {
		// A loopback target is sent direct by the proxy settings, so a source
		// URL naming the proxy's own address must not ride the trusted proxy
		// dial: it is judged like any other target.
		proxy, hits := fakeProxy(t, nil)
		proxyEnv(t, proxy.URL, "")
		client := NewClient(ParseAllowlist(nil), 5*time.Second, hint, WithResolver(proxyTestLookup))
		resp, err := client.Get(proxy.URL + "/admin")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("direct request to the proxy's loopback address was not refused")
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("proxy received %d direct requests, want 0", n)
		}
	})

	t.Run("NO_PROXY host uses the direct guarded dial", func(t *testing.T) {
		proxy, proxyHits := fakeProxy(t, nil)
		var directHits atomic.Int32
		direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			directHits.Add(1)
			_, _ = w.Write([]byte("direct"))
		}))
		defer direct.Close()
		proxyEnv(t, proxy.URL, "direct.test")
		lookup := func(_ context.Context, _, host string) ([]netip.Addr, error) {
			if host != "direct.test" {
				return nil, errors.New("unexpected lookup of " + host)
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		target := strings.Replace(direct.URL, "127.0.0.1", "direct.test", 1)

		refused := NewClient(ParseAllowlist(nil), 5*time.Second, hint, WithResolver(lookup))
		if resp, err := refused.Get(target); err == nil {
			_ = resp.Body.Close()
			t.Fatal("NO_PROXY host resolving to loopback was not refused by the direct dial guard")
		}
		allowed := NewClient(ParseAllowlist([]string{"direct.test"}), 5*time.Second, hint, WithResolver(lookup))
		resp, err := allowed.Get(target)
		if err != nil {
			t.Fatalf("allowlisted NO_PROXY host: %v", err)
		}
		_ = resp.Body.Close()
		if directHits.Load() != 1 || proxyHits.Load() != 0 {
			t.Errorf("direct hits = %d, proxy hits = %d; want 1 and 0", directHits.Load(), proxyHits.Load())
		}
	})
}

// loopbackLookup resolves localhost and alias.test to 127.0.0.1, so a test
// can name the proxy's address with a hostname the direct path resolves.
func loopbackLookup(_ context.Context, _, host string) ([]netip.Addr, error) {
	switch host {
	case "localhost", "alias.test":
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	return nil, errors.New("test resolver: no record for " + host)
}

// No spelling of the proxy's own address may reach the trusted proxy dial on
// the direct path. net/http and httpproxy map a non-ASCII host through IDNA
// before deciding "direct" and before dialing, so fullwidth digits and dots
// (U+FF10.., U+FF0E) become 127.0.0.1 and fullwidth letters become
// localhost; a hostname that resolves to the proxy's address, sent direct by
// NO_PROXY, is the same address under another name.
func TestGuardedClientProxyAddressSpellings(t *testing.T) {
	const hint = "--allowed-download-hosts"
	const fullwidthIP = "\uff11\uff12\uff17\uff0e\uff10\uff0e\uff10\uff0e\uff11"        // 127.0.0.1
	const fullwidthLocalhost = "\uff4c\uff4f\uff43\uff41\uff4c\uff48\uff4f\uff53\uff54" // localhost

	proxy, hits := fakeProxy(t, nil)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatalf("split proxy URL: %v", err)
	}
	for _, proxyHost := range []string{"127.0.0.1", "localhost"} {
		for _, target := range []string{fullwidthIP, fullwidthLocalhost, "alias.test"} {
			t.Run(proxyHost+"/"+target, func(t *testing.T) {
				hits.Store(0)
				proxyEnv(t, "http://"+net.JoinHostPort(proxyHost, port), "alias.test")
				client := NewClient(ParseAllowlist(nil), 5*time.Second, hint, WithResolver(loopbackLookup))
				resp, err := client.Get("http://" + net.JoinHostPort(target, port) + "/admin")
				if err == nil {
					_ = resp.Body.Close()
					t.Fatal("a spelling of the proxy's address reached the proxy on the direct path")
				}
				if n := hits.Load(); n != 0 {
					t.Errorf("proxy received %d requests, want 0 (err %v)", n, err)
				}
			})
		}
	}
}

// targetsTrustedProxy matches by resolved address, not only by spelling: a
// NO_PROXY hostname that resolves to the proxy's IP and port targets it.
func TestTargetsTrustedProxyByResolvedAddress(t *testing.T) {
	tr := newProxyTrust(&httpproxy.Config{HTTPProxy: "http://127.0.0.1:3128"}, loopbackLookup)
	cases := []struct {
		target string
		want   bool
	}{
		{"http://127.0.0.1:3128/", true},
		{"http://alias.test:3128/", true},  // resolves to the proxy's IP
		{"http://alias.test:3129/", false}, // same IP, different port
		{"http://unknown.test:3128/", false},
	}
	for _, tc := range cases {
		u, _ := url.Parse(tc.target)
		got, err := tr.targets(t.Context(), u)
		if err != nil {
			t.Fatalf("targets(%s): %v", tc.target, err)
		}
		if got != tc.want {
			t.Errorf("targets(%s) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

// A host that IDNA cannot map to ASCII is refused, never passed through.
func TestCanonicalHostFailsClosed(t *testing.T) {
	if _, err := canonicalHost("\uff4c\uff4f\uff43\uff41\uff4c\uff48\uff4f\uff53\uff54"); err != nil {
		t.Fatalf("fullwidth localhost should map to ASCII: %v", err)
	}
	if h, _ := canonicalHost("\uff4c\uff4f\uff43\uff41\uff4c\uff48\uff4f\uff53\uff54"); h != "localhost" {
		t.Errorf("canonicalHost(fullwidth localhost) = %q, want localhost", h)
	}
	if h, _ := canonicalHost("Mirror.TEST"); h != "mirror.test" {
		t.Errorf("canonicalHost(Mirror.TEST) = %q, want mirror.test", h)
	}
	if _, err := canonicalHost("bad\u200d\u0000host"); err == nil {
		t.Error("an unmappable host should be refused")
	}
}

// fakeConnectProxy answers CONNECT with 502 after recording the authority it
// was asked to tunnel to, so a test can see that an HTTPS request reached the
// trusted proxy (and for which target) without any tunnel being built.
func fakeConnectProxy(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var authorities []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorities = append(authorities, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), authorities...)
	}
}

// HTTPS through a proxy is a CONNECT tunnel. The target is judged before the
// CONNECT is sent: a blocked host never reaches the proxy, an allowed one
// issues CONNECT for the right authority to the trusted (loopback) proxy.
func TestGuardedClientProxyConnect(t *testing.T) {
	const hint = "--allowed-download-hosts"

	t.Run("blocked https target refused before CONNECT", func(t *testing.T) {
		proxy, seen := fakeConnectProxy(t)
		proxyEnv(t, proxy.URL, "")
		client := NewClient(ParseAllowlist(nil), 5*time.Second, hint, WithResolver(proxyTestLookup))
		resp, err := client.Get("https://internal.test/model.gguf")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("blocked https target via proxy was not refused")
		}
		if !strings.Contains(err.Error(), "SSRF guard") {
			t.Errorf("want the guard refusal, got: %v", err)
		}
		if got := seen(); len(got) != 0 {
			t.Errorf("proxy saw %v, want nothing", got)
		}
	})

	t.Run("allowed https target issues CONNECT to the proxy", func(t *testing.T) {
		proxy, seen := fakeConnectProxy(t)
		proxyEnv(t, proxy.URL, "")
		client := NewClient(ParseAllowlist(nil), 5*time.Second, hint, WithResolver(proxyTestLookup))
		resp, err := client.Get("https://public.test/model.gguf")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("the fake proxy refuses every CONNECT; the request should fail")
		}
		if got := seen(); len(got) != 1 || got[0] != "CONNECT public.test:443" {
			t.Errorf("proxy saw %v, want exactly [CONNECT public.test:443]", got)
		}
	})
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://minio.lan:9000/bucket/key", "https://minio.lan:9000/other", true},
		{"https://MinIO.LAN:9000", "https://minio.lan:9000/x", true},
		{"https://store.example", "https://store.example:443/x", true},
		{"http://store.example:80", "http://store.example/x", true},
		{"https://store.example", "https://other.example/x", false},
		{"https://store.example", "https://store.example.evil.test/x", false},
		{"http://127.0.0.1:9000", "http://127.0.0.1:9001/x", false},
		{"https://store.example", "http://store.example/x", false},
		{"https://store.example:443", "http://store.example:443/x", false},
		{"https://store.example", "https://store.example:8443/x", false},
		{"https://ｓｔｏｒｅ.example", "https://store.example/x", true},
	}
	for _, tc := range cases {
		a, err := url.Parse(tc.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := url.Parse(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := SameOrigin(a, b); got != tc.want {
			t.Errorf("SameOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if SameOrigin(nil, &url.URL{}) || SameOrigin(&url.URL{}, &url.URL{}) {
		t.Error("nil or empty URLs must not match")
	}
}
