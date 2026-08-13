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

package main

import (
	"fmt"
	"net"
	"strings"
)

// metricsDisabled reports whether a --metrics-bind-address value means "do not
// serve metrics".
//
// "" is this binary's documented sentinel, but "0" is what the manager uses
// (cmd/main.go defaults --metrics-bind-address to "0" and documents "leave as 0
// to disable"). Carrying the manager's value here used to produce a listener
// that failed with `address 0: missing port in address` AFTER a log line
// claiming it was listening, so the operator's last signal said the opposite of
// what happened. Accepting both makes the two binaries agree.
func metricsDisabled(addr string) bool {
	switch strings.ToLower(strings.TrimSpace(addr)) {
	case "", "0", "none":
		return true
	default:
		return false
	}
}

// listenConflict returns a non-nil error when two listen addresses would race
// for the same port.
//
// Configuring both listeners onto one address is not a stable failure: whichever
// binds first wins, and the loser's treatment differs by which one it is. The
// data plane's error is fatal, so the process crash-loops; the metrics server's
// is logged and execution continues without metrics. Measured over 15 runs that
// came out 14 crash-loops to 1 silent metrics loss, so the same configuration
// produced two different outcomes.
//
// Detecting it before either listener binds turns a coin flip into one
// deterministic startup error that names the conflict.
//
// Host comparison is deliberately conservative. Equal ports conflict when the
// hosts are equal or when either side is a wildcard, because a wildcard bind
// covers every specific address on that port. Two different specific hosts on
// the same port do not conflict and are left alone: that is a legitimate
// configuration on a multi-homed node, and refusing it would be worse than the
// bug being fixed.
func listenConflict(dataPlane, metrics string) error {
	dHost, dPort, err := splitListen(dataPlane)
	if err != nil {
		// A malformed data-plane address is the data plane's problem to report
		// when it binds; this check declines to speak for it.
		return nil
	}
	mHost, mPort, err := splitListen(metrics)
	if err != nil {
		return nil
	}

	if dPort != mPort {
		return nil
	}
	if !hostsOverlap(dHost, mHost) {
		return nil
	}

	return fmt.Errorf(
		"--listen (%s) and --metrics-bind-address (%s) resolve to the same address; "+
			"the two listeners would race for port %s and the loser's failure differs by "+
			"which one loses (the data plane exits, the metrics server does not). "+
			"Give them different ports, or disable metrics with --metrics-bind-address=0",
		dataPlane, metrics, dPort)
}

// splitListen accepts the forms a Go listener accepts, including the bare
// ":9090" that omits the host.
func splitListen(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", "", err
	}
	if port == "" {
		return "", "", fmt.Errorf("no port in %q", addr)
	}
	return host, port, nil
}

// hostsOverlap reports whether two listener hosts can contend for one port.
// The empty host, 0.0.0.0 and [::] all mean "every interface" and therefore
// overlap with anything.
func hostsOverlap(a, b string) bool {
	if isWildcardHost(a) || isWildcardHost(b) {
		return true
	}
	return strings.EqualFold(a, b)
}

func isWildcardHost(h string) bool {
	switch strings.TrimSpace(h) {
	case "", "0.0.0.0", "::", "[::]":
		return true
	default:
		return false
	}
}
