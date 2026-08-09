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

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// A private CA must be ADDITIVE to the system trust store, never a replacement.
//
// Setting CURL_CA_BUNDLE to a single file overrides the system bundle
// entirely, so enabling --ca-cert-configmap for a private endpoint silently
// breaks every PUBLIC source on the fleet. Observed 2026-08-09: after the flag
// was enabled for a private object store, a Hugging Face pull failed with
//
//	curl: (60) SSL certificate OpenSSL verify result:
//	      unable to get local issuer certificate (20)
//
// while every private-endpoint download kept working, which is what made it
// hard to spot: the fleet had been pulling from the private store all day.

func caCmd() string {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	cmd := "echo download"
	addCACertVolume(&volumes, &mounts, &cmd, "llmkube-ca-cert")
	return cmd
}

func TestCACertBundle_KeepsSystemTrust(t *testing.T) {
	cmd := caCmd()

	// The system bundle must be part of what curl ends up trusting. Without
	// this the private CA is the ONLY trusted root and every public TLS
	// endpoint fails to verify.
	if !strings.Contains(cmd, "/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("system CA bundle is never referenced, so public sources cannot verify:\n%s", cmd)
	}
	// And the custom CA must still be trusted, or the private endpoint breaks.
	if !strings.Contains(cmd, "/custom-certs") {
		t.Errorf("custom CA is not referenced:\n%s", cmd)
	}
	if !strings.Contains(cmd, "CURL_CA_BUNDLE") {
		t.Errorf("CURL_CA_BUNDLE is never set:\n%s", cmd)
	}
}

// The combined bundle must not be written to the system path: the init
// container may run read-only or as a non-root user, and clobbering the image's
// trust store is a side effect no download command should have.
func TestCACertBundle_DoesNotWriteToSystemPath(t *testing.T) {
	cmd := caCmd()
	for _, bad := range []string{
		"> /etc/ssl/certs/ca-certificates.crt",
		">/etc/ssl/certs/ca-certificates.crt",
		">> /etc/ssl/certs/ca-certificates.crt",
	} {
		if strings.Contains(cmd, bad) {
			t.Errorf("command writes to the system trust store (%q):\n%s", bad, cmd)
		}
	}
}

// Absent config stays a no-op: no CA plumbing at all, so the image's own trust
// store is used untouched.
func TestCACertBundle_NoOpWhenUnset(t *testing.T) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	cmd := "echo download"
	addCACertVolume(&volumes, &mounts, &cmd, "")
	if cmd != "echo download" {
		t.Errorf("command mutated with no configmap set: %s", cmd)
	}
	if len(volumes) != 0 || len(mounts) != 0 {
		t.Errorf("volumes/mounts added with no configmap set: %d/%d", len(volumes), len(mounts))
	}
}

// The volume and mount still have to be wired, whatever the command does.
func TestCACertBundle_MountsTheConfigMap(t *testing.T) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	cmd := "echo download"
	addCACertVolume(&volumes, &mounts, &cmd, "llmkube-ca-cert")
	if len(volumes) != 1 || volumes[0].ConfigMap == nil ||
		volumes[0].ConfigMap.Name != "llmkube-ca-cert" {
		t.Fatalf("configmap volume not wired: %+v", volumes)
	}
	if len(mounts) != 1 || mounts[0].MountPath != "/custom-certs" {
		t.Fatalf("mount not wired: %+v", mounts)
	}
}
