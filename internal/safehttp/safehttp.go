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

// Package safehttp provides an SSRF-guarded HTTP client shared by any code
// path that fetches a source-derived URL: the controller's Model source
// reads today, the metal-agent's downloader next. See GHSA-jw3m-8q7m-f35r.
package safehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/idna"
)

// Allowlist permits specific hostnames and CIDRs back through the
// private-range SSRF guard. Hostnames match the request URL host
// (case-insensitive); CIDRs match resolved IPs.
type Allowlist struct {
	hosts    map[string]struct{}
	prefixes []netip.Prefix
}

// ParseAllowlist builds an allowlist from operator-supplied entries (for
// example --allowed-remote-hosts or --allowed-download-hosts). Each entry is
// a CIDR (10.20.0.0/16), a bare IP (treated as a single-address prefix), or a
// hostname. Blank entries are ignored; an unparseable CIDR falls back to
// hostname matching.
func ParseAllowlist(entries []string) Allowlist {
	al := Allowlist{hosts: map[string]struct{}{}}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if p, err := netip.ParsePrefix(e); err == nil {
				al.prefixes = append(al.prefixes, p)
				continue
			}
			// Not a valid CIDR; fall through and treat it as a host entry.
		}
		if ip, err := netip.ParseAddr(e); err == nil {
			al.prefixes = append(al.prefixes, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		al.hosts[strings.ToLower(e)] = struct{}{}
	}
	return al
}

func (al Allowlist) hostAllowed(host string) bool {
	_, ok := al.hosts[strings.ToLower(host)]
	return ok
}

func (al Allowlist) ipAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range al.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// blockedPrefixes lists non-public ranges that Go's stdlib classifiers
// (IsPrivate, IsLoopback, IsLinkLocal*) do NOT cover but that must never be
// reachable from source-derived URLs unless allowlisted. Parsed once at
// package init; IPBlocked runs on every dial, so no per-call parsing.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT / shared address space (RFC 6598), incl. Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking (RFC 2544)
	netip.MustParsePrefix("192.88.99.0/24"), // 6to4 relay anycast (RFC 3068)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use (RFC 8215)
}

// v4CompatPrefix matches deprecated IPv4-compatible IPv6 addresses
// (::a.b.c.d, RFC 4291 §2.5.5.1). Some stacks route these to the embedded
// IPv4 target, so they classify by the embedded address, not as IPv6.
var v4CompatPrefix = netip.MustParsePrefix("::/96")

// nat64Prefix is the NAT64 well-known prefix (RFC 6052). On a DNS64 network
// every IPv4-only host resolves to 64:ff9b::a.b.c.d, so blocking the prefix
// outright would refuse every public IPv4-only host there. It classifies by
// the embedded IPv4 address instead: public stays reachable, 64:ff9b::7f00:1
// (127.0.0.1) and friends stay blocked.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// IPBlocked reports whether an IP is in a range we refuse to connect to
// unless explicitly allowlisted: loopback, link-local (incl. 169.254.169.254
// and fe80::/10), RFC-1918 private, ULA (fc00::/7), unspecified, or any of
// blockedPrefixes (CGNAT, benchmarking, NAT64, ...). Unmap() first so
// IPv4-in-IPv6 forms (::ffff:127.0.0.1) classify as their IPv4 self;
// IPv4-compatible forms (::127.0.0.1) recurse on the embedded IPv4 address.
func IPBlocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	// IPv4-compatible IPv6 (::/96, but not :: or ::1, both already returned
	// true above) and NAT64 (64:ff9b::/96): extract the embedded IPv4 address
	// and classify by it, so ::169.254.169.254 and 64:ff9b::7f00:1 are blocked
	// like their IPv4 selves while 64:ff9b::808:808 (8.8.8.8) is not.
	// Recursion terminates: the embedded address is Is4 and cannot re-enter
	// this branch.
	if ip.Is6() && (v4CompatPrefix.Contains(ip) || nat64Prefix.Contains(ip)) {
		b := ip.As16()
		return IPBlocked(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	return false
}

// LookupFunc resolves host to its IP addresses, with the signature of
// (*net.Resolver).LookupNetIP.
type LookupFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

// Option configures NewClient.
type Option func(*clientOptions)

type clientOptions struct {
	lookup LookupFunc
}

// WithResolver replaces the DNS lookup the guarded dialer uses for hostname
// targets. The guard still judges every address the lookup returns, so this
// changes only where names resolve, never what is permitted. A nil lookup
// keeps net.DefaultResolver, which is what production callers pass; tests
// pass a fake so a hostname can be pinned to a local server without real DNS.
func WithResolver(lookup LookupFunc) Option {
	return func(o *clientOptions) {
		if lookup != nil {
			o.lookup = lookup
		}
	}
}

// NewClient returns an *http.Client whose dialer refuses to connect to
// blocked IP ranges unless the target host/IP is allowlisted. The check runs
// on the RESOLVED IPs and dials only those pinned IPs, so DNS rebinding
// cannot slip a blocked address in after the check. Every resolved IP must be
// permitted (a multi-record answer mixing one public and one loopback address
// is rejected outright). Redirect targets dial through the same guard; hops
// are capped.
//
// Proxy settings (HTTP_PROXY, HTTPS_PROXY, NO_PROXY and their lowercase
// forms) are read from the environment when NewClient is called. A request
// that goes through a proxy is judged by its target before it is handed to
// the proxy: the target host is resolved and put through the same strict
// check the dial applies, on every request including each redirect hop. The
// proxy's own address is operator-configured and trusted, so a proxy on a
// private or loopback address is dialed. Limit: the proxy resolves the target
// name again itself, so DNS rebinding between our check and the proxy's
// lookup is not closed; the operator controls the proxy and its resolver.
// Requests the environment sends direct (NO_PROXY, loopback) use the guarded
// dial unchanged.
//
// hint is the operator-facing setting named in the refusal message, for
// example "modelSource.allowedRemoteHosts" for the controller or
// "--allowed-download-hosts" for the metal-agent. opts are optional; see
// WithResolver.
func NewClient(allow Allowlist, timeout time.Duration, hint string, opts ...Option) *http.Client {
	o := clientOptions{lookup: net.DefaultResolver.LookupNetIP}
	for _, opt := range opts {
		opt(&o)
	}
	// checkHost resolves host (unless it is an IP literal) and applies the
	// strict guard: every resolved IP must be permitted, so a multi-record
	// rebind (one public + one 127.0.0.1) cannot pass.
	checkHost := func(ctx context.Context, rawHost string) ([]netip.Addr, error) {
		// Judge the host net/http will actually dial: IDNA-mapped to ASCII.
		host, err := canonicalHost(rawHost)
		if err != nil {
			return nil, err
		}
		hostAllowed := allow.hostAllowed(host)
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			addrs, err := o.lookup(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			ips = addrs
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for host %q", host)
		}
		for _, ip := range ips {
			permitted := hostAllowed || allow.ipAllowed(ip) || !IPBlocked(ip)
			if !permitted {
				return nil, fmt.Errorf(
					"connection to %s (%s) blocked by SSRF guard (GHSA-jw3m-8q7m-f35r); "+
						"allowlist via %s", host, ip, hint)
			}
		}
		return ips, nil
	}

	// httpproxy.FromEnvironment is what http.ProxyFromEnvironment wraps, but
	// read per client instead of cached once per process.
	proxyCfg := httpproxy.FromEnvironment()
	envProxy := proxyCfg.ProxyFunc()
	trust := newProxyTrust(proxyCfg, o.lookup)

	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if trust.isProxyDialAddr(addr) {
			// The operator-configured proxy: trusted, dialed as configured.
			return base.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := checkHost(ctx, host)
		if err != nil {
			return nil, err
		}
		// Dial the pinned, already-checked IPs in resolver order, falling back
		// across them (dual-stack hosts may not listen on every address).
		var dialErr error
		for _, ip := range ips {
			conn, err := base.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}
		return nil, dialErr
	}
	proxy := func(req *http.Request) (*url.URL, error) {
		proxyURL, err := envProxy(req.URL)
		if err != nil {
			return nil, err
		}
		if proxyURL == nil {
			// Direct (no proxy, NO_PROXY, loopback): the dial guard applies,
			// except that a target that is the proxy's own address, under any
			// spelling or name, would ride the trusted proxy dial. Judge it
			// here instead.
			hit, err := trust.targets(req.Context(), req.URL)
			if err != nil {
				return nil, err
			}
			if hit {
				if _, err := checkHost(req.Context(), req.URL.Hostname()); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}
		if _, err := checkHost(req.Context(), req.URL.Hostname()); err != nil {
			return nil, err
		}
		return proxyURL, nil
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 proxy,
			DialContext:           dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          10,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return nil
		},
	}
}

// canonicalHost returns host the way net/http and httpproxy see it before
// deciding "direct" and before dialing: ASCII hosts unchanged, non-ASCII
// hosts mapped through IDNA (so fullwidth 127.0.0.1 is 127.0.0.1), then
// lowercased. A host IDNA cannot map is refused rather than passed through.
func canonicalHost(host string) (string, error) {
	if isASCII(host) {
		return strings.ToLower(host), nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("host %q is not a valid hostname; refused by SSRF guard: %w", host, err)
	}
	return strings.ToLower(ascii), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// proxyTrust records the operator-configured proxies (HTTP_PROXY and
// HTTPS_PROXY), whose own addresses the dial trusts.
type proxyTrust struct {
	lookup LookupFunc
	// dialAddrs are the canonical host:port dial addresses of the proxies.
	dialAddrs map[string]struct{}
	proxies   []proxyEndpoint
}

type proxyEndpoint struct {
	host, port string // canonical
}

func newProxyTrust(cfg *httpproxy.Config, lookup LookupFunc) *proxyTrust {
	t := &proxyTrust{lookup: lookup, dialAddrs: map[string]struct{}{}}
	for _, raw := range []string{cfg.HTTPProxy, cfg.HTTPSProxy} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			// Bare "host:port", which httpproxy also accepts as http://.
			if u, err = url.Parse("http://" + raw); err != nil {
				continue
			}
		}
		host, err := canonicalHost(u.Hostname())
		if err != nil {
			continue // net/http cannot dial it either
		}
		port := defaultPort(u)
		t.dialAddrs[net.JoinHostPort(host, port)] = struct{}{}
		t.proxies = append(t.proxies, proxyEndpoint{host: host, port: port})
	}
	return t
}

// isProxyDialAddr reports whether addr, as the transport hands it to the
// dialer (already IDNA-mapped), is one of the configured proxies.
func (t *proxyTrust) isProxyDialAddr(addr string) bool {
	_, ok := t.dialAddrs[strings.ToLower(addr)]
	return ok
}

// targets reports whether a direct request to u would dial one of the
// proxies: by canonical spelling, or by resolved address (a hostname that
// resolves to the proxy's IP on the proxy's port). A host IDNA cannot map is
// an error. A lookup failure is not a match; the dial's own guarded lookup
// then decides.
func (t *proxyTrust) targets(ctx context.Context, u *url.URL) (bool, error) {
	if len(t.proxies) == 0 {
		return false, nil
	}
	host, err := canonicalHost(u.Hostname())
	if err != nil {
		return false, err
	}
	port := defaultPort(u)
	if _, ok := t.dialAddrs[net.JoinHostPort(host, port)]; ok {
		return true, nil
	}
	var targetIPs []netip.Addr
	for _, p := range t.proxies {
		if p.port != port {
			continue
		}
		if targetIPs == nil {
			if targetIPs = t.resolve(ctx, host); len(targetIPs) == 0 {
				return false, nil
			}
		}
		for _, pip := range t.resolve(ctx, p.host) {
			for _, tip := range targetIPs {
				if pip.Unmap() == tip.Unmap() {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (t *proxyTrust) resolve(ctx context.Context, host string) []netip.Addr {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}
	}
	ips, err := t.lookup(ctx, "ip", host)
	if err != nil {
		return nil
	}
	return ips
}

// defaultPort returns u's port, or the scheme's default the way net/http
// fills it in when dialing.
func defaultPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	return map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
}

// SameOrigin reports whether a and b name the same scheme, host and port:
// the scheme and host compare case-insensitively (a non-ASCII host through
// IDNA, as canonicalHost maps it), and a missing port is the scheme's
// default, so https://Store:443 and https://store are the same origin. A host
// IDNA cannot map never matches.
//
// Request signers use it to sign only requests to the endpoint they were
// configured for, so a redirect to another host does not carry their
// credentials (#1955).
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	ah, err := canonicalHost(a.Hostname())
	if err != nil || ah == "" {
		return false
	}
	bh, err := canonicalHost(b.Hostname())
	if err != nil || ah != bh {
		return false
	}
	return defaultPort(a) == defaultPort(b)
}
