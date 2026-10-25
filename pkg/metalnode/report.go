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

// Package metalnode is the wire contract between the metal-agent and the
// MetalNode controller. The agent never writes its MetalNode: it renews a
// coordination.k8s.io Lease and carries what it knows in one JSON annotation
// on that Lease. The controller reads the annotation, treats every field as a
// claim, copies the facts into MetalNode status and verifies the addresses
// with a dial-back. Both sides import this package so the JSON keys cannot
// drift apart.
package metalnode

import (
	"encoding/json"
	"fmt"
	"net/netip"
)

// ReportAnnotation is the Lease annotation that carries the agent's Report.
const ReportAnnotation = "llmkube.ai/metal-node-report"

// Candidate sources, in the vocabulary the agent's address selection uses.
const (
	SourceOverride   = "override"
	SourceTailnet    = "tailnet"
	SourceLAN        = "lan"
	SourceDockerHost = "docker-host"
)

const (
	// MaxAnnotationBytes bounds the annotation the controller will decode.
	// A full report with MaxCandidates addresses is well under 2 KiB.
	MaxAnnotationBytes = 8 * 1024
	// MaxCandidates bounds the addresses one report may carry, which also
	// bounds the verifier pod's run time (one short probe per address).
	MaxCandidates = 16

	maxShortString = 128
	maxSource      = 32
)

// Report is what the agent tells the cluster about itself. Every field is a
// claim made by the Mac.
type Report struct {
	Version      string      `json:"version,omitempty"`
	Chip         string      `json:"chip,omitempty"`
	MemoryBytes  int64       `json:"memoryBytes,omitempty"`
	GPUCores     int32       `json:"gpuCores,omitempty"`
	MetalVersion string      `json:"metalVersion,omitempty"`
	IngressPort  int32       `json:"ingressPort,omitempty"`
	IngressSPKI  string      `json:"ingressSPKI,omitempty"`
	Candidates   []Candidate `json:"candidates,omitempty"`
}

// Candidate is one address the agent believes the cluster can reach it at,
// in the agent's order of preference.
type Candidate struct {
	Address string `json:"address"`
	Source  string `json:"source"`
}

// Encode renders r as the annotation value.
func Encode(r Report) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode metal node report: %w", err)
	}
	return string(raw), nil
}

// Decode parses and validates an annotation value. Its errors never quote
// the agent's values, so a caller can put them in status as they are. Unknown fields are
// ignored, so an older controller still reads a newer agent's report.
// Candidate addresses come back in canonical form (IPv4 never as 4-in-6).
// The SPKI pin's format is the consumer's to check, against the same rule
// the relay applies.
func Decode(raw string) (Report, error) {
	if len(raw) > MaxAnnotationBytes {
		return Report{}, fmt.Errorf("report is %d bytes, over the %d bytes allowed", len(raw), MaxAnnotationBytes)
	}
	var r Report
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Report{}, fmt.Errorf("decode metal node report: %w", err)
	}
	if err := r.validate(); err != nil {
		return Report{}, err
	}
	return r, nil
}

func (r *Report) validate() error {
	if r.IngressPort < 0 || r.IngressPort > 65535 {
		return fmt.Errorf("ingressPort %d is not a TCP port", r.IngressPort)
	}
	if r.MemoryBytes < 0 {
		return fmt.Errorf("memoryBytes %d is negative", r.MemoryBytes)
	}
	if r.GPUCores < 0 {
		return fmt.Errorf("gpuCores %d is negative", r.GPUCores)
	}
	for _, f := range []struct {
		name, value string
	}{
		{"version", r.Version},
		{"chip", r.Chip},
		{"metalVersion", r.MetalVersion},
		{"ingressSPKI", r.IngressSPKI},
	} {
		if err := checkPrintable(f.name, f.value, maxShortString); err != nil {
			return err
		}
	}
	if len(r.Candidates) > MaxCandidates {
		return fmt.Errorf("candidates has %d entries, over the %d allowed", len(r.Candidates), MaxCandidates)
	}
	for i := range r.Candidates {
		c := &r.Candidates[i]
		addr, err := netip.ParseAddr(c.Address)
		if err != nil || addr.Zone() != "" {
			return fmt.Errorf("candidates[%d].address is not an IP address", i)
		}
		c.Address = addr.Unmap().String()
		if !validSource(c.Source) {
			return fmt.Errorf("candidates[%d].source must be 1-%d lowercase letters, digits or '-'", i, maxSource)
		}
	}
	return nil
}

// checkPrintable bounds a free-text claim before it reaches MetalNode status.
func checkPrintable(name, value string, limit int) error {
	if len(value) > limit {
		return fmt.Errorf("%s is %d bytes, over the %d allowed", name, len(value), limit)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func validSource(s string) bool {
	if s == "" || len(s) > maxSource {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
