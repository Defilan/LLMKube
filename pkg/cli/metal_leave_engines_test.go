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

package cli

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/defilantech/llmkube/pkg/metaljoin"
)

// joinedMac joins a fake Mac whose agent binary has stop-engines.
func joinedMac(t *testing.T) (*fakeMac, string) {
	t.Helper()
	_, _, ns := metalEnvtest(t)
	mac := newFakeMac(t)
	script := []byte("#!/bin/sh\n# usage: llmkube-metal-agent " + metaljoin.StopEnginesCommand + " [flags]\n")
	if err := os.WriteFile(mac.agentBin, script, 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	joinString, _ := issueJoinString(t, ns)
	if _, stderr, err := runMetal(t, "join", joinString, "--wait", "0"); err != nil {
		t.Fatalf("join: %v\n%s", err, stderr)
	}
	mac.calls = nil
	return mac, ns
}

func requireExist(t *testing.T, what string, files ...string) {
	t.Helper()
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s: %s is gone (%v), want it kept", what, f, err)
		}
	}
}

func agentRuns(calls []string) []string {
	var runs []string
	for _, c := range calls {
		if strings.HasPrefix(c, "agent ") {
			runs = append(runs, c)
		}
	}
	return runs
}

// TestMetalLeaveStopsTheEnginesBeforeRemovingTheCredentials: engines outlive
// the agent (#2079), so leave runs the agent's stop-engines. It runs once the
// agent has exited, so no engine can start after it reads the records, and
// while the kubeconfig is still there to withdraw the endpoints with.
func TestMetalLeaveStopsTheEnginesBeforeRemovingTheCredentials(t *testing.T) {
	mac, ns := joinedMac(t)
	paths := mac.paths()
	mac.stopPolls = 3
	credentialsPresent := false
	mac.onAgent = func([]string) error {
		_, errKC := os.Stat(paths.Kubeconfig)
		_, errTok := os.Stat(paths.Token)
		credentialsPresent = errKC == nil && errTok == nil
		return nil
	}

	stdout, stderr, err := runMetal(t, "leave")
	if err != nil {
		t.Fatalf("leave: %v\n%s", err, stderr)
	}
	want := []string{"agent " + mac.agentBin + " stop-engines --kubeconfig " + paths.Kubeconfig + " --namespace " + ns}
	if got := agentRuns(mac.calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("agent runs %q, want %q after the agent exited (launchctl calls %q)", got, want, mac.calls)
	}
	if !credentialsPresent {
		t.Error("stop-engines ran after leave removed the kubeconfig or token it withdraws endpoints with")
	}
	for _, f := range []string{paths.Plist, paths.Kubeconfig, paths.Token} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after leave: %v, want removed", f, err)
		}
	}
	if !strings.Contains(stdout, "stopping the engines") {
		t.Errorf("leave does not say it stops the engines: %q", stdout)
	}
}

// TestMetalLeaveKeepsEverythingWhenStopEnginesFails: an engine may still run,
// so leave removes nothing and can be run again, and the second run stops the
// engines and finishes.
func TestMetalLeaveKeepsEverythingWhenStopEnginesFails(t *testing.T) {
	mac, _ := joinedMac(t)
	paths := mac.paths()
	mac.onAgent = func([]string) error { return errors.New("exit status 1") }

	_, _, err := runMetal(t, "leave")
	if err == nil || !strings.Contains(err.Error(), "stop-engines") ||
		!strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("leave = %v, want the stop-engines failure reported", err)
	}
	requireExist(t, "a failed stop-engines", paths.Plist, paths.Kubeconfig, paths.Token)

	mac.onAgent = nil
	mac.calls = nil
	if _, stderr, err := runMetal(t, "leave"); err != nil {
		t.Fatalf("second leave: %v\n%s", err, stderr)
	}
	if got := agentRuns(mac.calls); len(got) != 1 {
		t.Errorf("second leave ran the agent %q, want stop-engines once", got)
	}
	if _, err := os.Stat(paths.Kubeconfig); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("kubeconfig after the second leave: %v, want removed", err)
	}
}

// TestMetalLeaveWaitsOutAnAgentThatWillNotStop: an agent still running could
// start an engine after stop-engines, so leave runs nothing and removes
// nothing until launchd has unloaded it.
func TestMetalLeaveWaitsOutAnAgentThatWillNotStop(t *testing.T) {
	mac, _ := joinedMac(t)
	paths := mac.paths()
	mac.stopPolls = 1 << 30
	prev := metalAgentUnloadTimeout
	metalAgentUnloadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { metalAgentUnloadTimeout = prev })

	_, _, err := runMetal(t, "leave")
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("leave = %v, want it to report the agent still running", err)
	}
	if got := agentRuns(mac.calls); len(got) != 0 {
		t.Errorf("leave ran %q while the agent was still loaded", got)
	}
	requireExist(t, "an agent that would not stop", paths.Plist, paths.Kubeconfig, paths.Token)
}

// TestMetalLeaveWithTheAgentBinaryGone: stop-engines cannot run, so leave
// warns and finishes rather than keeping the Mac joined.
func TestMetalLeaveWithTheAgentBinaryGone(t *testing.T) {
	mac, _ := joinedMac(t)
	if err := os.Remove(mac.agentBin); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runMetal(t, "leave")
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if !strings.Contains(stderr, "were not stopped") {
		t.Errorf("leave does not warn that the engines were not stopped: %q", stderr)
	}
	if got := agentRuns(mac.calls); len(got) != 0 {
		t.Errorf("leave ran a missing agent binary: %q", got)
	}
}

// TestMetalAgentPlistArgsReadsWhatJoinWrites feeds the parser the plist the
// real renderer writes, escaping included.
func TestMetalAgentPlistArgsReadsWhatJoinWrites(t *testing.T) {
	args := []string{"/Users/a & b/bin/llmkube-metal-agent", "--kubeconfig", "/Users/a & b/<kc>", "--namespace", "ns"}
	raw, err := renderMetalAgentPlist(args, "/Users/a & b", "/Users/a & b/agent.log")
	if err != nil {
		t.Fatal(err)
	}
	got, err := metalAgentPlistArgs(raw)
	if err != nil || !reflect.DeepEqual(got, args) {
		t.Fatalf("metalAgentPlistArgs = %q, %v; want %q", got, err, args)
	}
}

// TestMetalLeaveRefusesRoot: leave runs the binary its LaunchAgent names, a
// path the user can write, so under sudo it would run that path as root.
func TestMetalLeaveRefusesRoot(t *testing.T) {
	mac, _ := joinedMac(t)
	mac.uid = 0
	paths := mac.paths()
	_, _, err := runMetal(t, "leave")
	if err == nil || !strings.Contains(err.Error(), "not as root") {
		t.Fatalf("leave as root = %v, want it refused", err)
	}
	if len(mac.calls) != 0 {
		t.Errorf("leave as root ran %q, want nothing run", mac.calls)
	}
	requireExist(t, "leave as root", paths.Plist, paths.Kubeconfig, paths.Token)
}
