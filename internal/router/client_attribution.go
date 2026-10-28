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
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// credentialHeaders carry a secret. As the audit caller header, one of them
// would write that secret into every audit line.
var credentialHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"x-api-key":           true,
	"api-key":             true,
	"x-goog-api-key":      true,
}

// IsCredentialHeader reports whether the header name, in any case, carries a
// secret and so must not be logged as the caller.
func IsCredentialHeader(name string) bool {
	return credentialHeaders[strings.ToLower(strings.TrimSpace(name))]
}

// ParseTrustedProxies parses policy.auditLog.trustedProxyCIDRs. A bare
// address is a single-host prefix.
func ParseTrustedProxies(cidrs []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for i, s := range cidrs {
		s = strings.TrimSpace(s)
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("trustedProxyCIDRs[%d]: %q is not a CIDR or an IP address", i, s)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// clientAddress is the address the audit line records for r: the TCP peer,
// unless the peer is a trusted proxy. Then X-Forwarded-For is read from the
// right, trusted hops are skipped, and the first untrusted hop is the client.
// A caller can send any X-Forwarded-For it likes, so only hops appended by a
// trusted proxy are believed, and the walk stops at an entry that is not an
// address. Empty when the peer address does not parse.
func clientAddress(r *http.Request, trusted []netip.Prefix) string {
	peer, ok := parseHop(r.RemoteAddr)
	if !ok {
		return ""
	}
	client := peer
	if !isTrustedProxy(peer, trusted) {
		return client.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := parseHop(hops[i])
		if !ok {
			break
		}
		client = hop
		if !isTrustedProxy(hop, trusted) {
			break
		}
	}
	return client.String()
}

// parseHop parses an address with or without a port, unmapping an
// IPv4-mapped IPv6 address so it compares against IPv4 prefixes.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

func isTrustedProxy(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// maxCallerLen caps the logged caller value. The client sets it, so it is a
// label rather than an identity, and it must not let a client bloat every
// audit line.
const maxCallerLen = 128

// callerValue is the header's value as the audit line records it: cut to
// maxCallerLen bytes, with every character outside printable ASCII replaced
// by '?'.
func callerValue(r *http.Request, header string) string {
	v := r.Header.Get(header)
	if len(v) > maxCallerLen {
		v = v[:maxCallerLen]
	}
	return strings.Map(func(c rune) rune {
		if c < 0x20 || c > 0x7e {
			return '?'
		}
		return c
	}, v)
}
