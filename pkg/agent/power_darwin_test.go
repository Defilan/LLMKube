//go:build darwin

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

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestParsePowermetricsStream_Fixture(t *testing.T) {
	f, err := os.Open("testdata/powermetrics_sample.txt")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	var samples []applePowerSample
	if err := parsePowermetricsStream(f, func(s applePowerSample) {
		samples = append(samples, s)
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(samples) == 0 {
		t.Fatal("expected at least one sample, got 0")
	}

	for i, s := range samples {
		if s.CombinedWatts <= 0 {
			t.Errorf("sample %d: CombinedWatts %.3f, want >0", i, s.CombinedWatts)
		}
		// Combined Power = CPU + GPU + ANE per powermetrics docs. Allow 50 mW
		// rounding slop because powermetrics rounds each component to whole
		// milliwatts independently.
		sum := s.CPUWatts + s.GPUWatts + s.ANEWatts
		if diff := s.CombinedWatts - sum; diff > 0.05 || diff < -0.05 {
			t.Errorf("sample %d: combined %.3f != cpu+gpu+ane %.3f", i, s.CombinedWatts, sum)
		}
		if s.CPUWatts < 0 || s.GPUWatts < 0 || s.ANEWatts < 0 {
			t.Errorf("sample %d: negative component: cpu=%.3f gpu=%.3f ane=%.3f",
				i, s.CPUWatts, s.GPUWatts, s.ANEWatts)
		}
	}
}

// TestParsePowermetricsStream_DuplicateGPULineIgnored guards the behaviour
// described in power_darwin.go: powermetrics emits "GPU Power: <n> mW" twice
// per sample (once in cpu_power, once in gpu_power) and the parser must take
// the first occurrence so the value matches what was summed into Combined.
func TestParsePowermetricsStream_DuplicateGPULineIgnored(t *testing.T) {
	input := strings.Join([]string{
		"*** Sampled system activity (header)",
		"CPU Power: 100 mW",
		"GPU Power: 50 mW",
		"ANE Power: 0 mW",
		"Combined Power (CPU + GPU + ANE): 150 mW",
		"GPU Power: 9999 mW", // duplicate from the gpu_power section
		"",
	}, "\n")

	var got []applePowerSample
	if err := parsePowermetricsStream(strings.NewReader(input), func(s applePowerSample) {
		got = append(got, s)
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(got))
	}
	if got[0].GPUWatts != 0.05 {
		t.Errorf("want GPUWatts 0.05 (first occurrence), got %.3f", got[0].GPUWatts)
	}
	if got[0].CombinedWatts != 0.15 {
		t.Errorf("want CombinedWatts 0.15, got %.3f", got[0].CombinedWatts)
	}
}

// TestParsePowermetricsStream_PartialSampleNotEmitted ensures that a sample
// missing its Combined Power line never emits — partial parses would publish
// stale-zero gauges and corrupt InferCost's per-token math.
func TestParsePowermetricsStream_PartialSampleNotEmitted(t *testing.T) {
	input := strings.Join([]string{
		"*** Sampled system activity (header 1)",
		"CPU Power: 100 mW",
		"GPU Power: 50 mW",
		// no ANE, no Combined — process killed mid-write
		"*** Sampled system activity (header 2)",
		"CPU Power: 200 mW",
		"GPU Power: 80 mW",
		"ANE Power: 0 mW",
		"Combined Power (CPU + GPU + ANE): 280 mW",
		"",
	}, "\n")

	var got []applePowerSample
	if err := parsePowermetricsStream(strings.NewReader(input), func(s applePowerSample) {
		got = append(got, s)
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 sample (only the complete one), got %d", len(got))
	}
	if got[0].CombinedWatts != 0.28 {
		t.Errorf("want CombinedWatts 0.28, got %.3f", got[0].CombinedWatts)
	}
}

// TestDefaultPowermetricsCommand_MatchesSudoersSpec is the regression guard
// that keeps the agent's argv aligned with the shipped sudoers fragment.
// If you change defaultPowermetricsCommand without updating
// deployment/macos/sudoers.d/llmkube-powermetrics, this test will fail,
// preventing a release where the gauges stop publishing because sudo refuses
// the new argv.
func TestDefaultPowermetricsCommand_MatchesSudoersSpec(t *testing.T) {
	cmd := defaultPowermetricsCommand(context.Background(), DefaultPowermetricsBin, time.Second)

	// argv[0] must be absolute /usr/bin/sudo so a $PATH attacker can't
	// substitute their own sudo wrapper to capture our (root) NOPASSWD call.
	if cmd.Path != defaultSudoBin {
		t.Errorf("sudo path = %q, want %q", cmd.Path, defaultSudoBin)
	}
	wantArgs := []string{
		defaultSudoBin, "-n",
		DefaultPowermetricsBin,
		"--samplers", "cpu_power,gpu_power",
		"-i", "1000",
	}
	if len(cmd.Args) != len(wantArgs) {
		t.Fatalf("argv = %v (len %d), want %v (len %d)",
			cmd.Args, len(cmd.Args), wantArgs, len(wantArgs))
	}
	for i, want := range wantArgs {
		if cmd.Args[i] != want {
			t.Errorf("argv[%d] = %q, want %q", i, cmd.Args[i], want)
		}
	}

	// Cross-check against the rule `make install-powermetrics-sudo` installs
	// with its default interval: the granted command must be exactly the
	// agent's argv after `sudo -n`.
	rendered, err := renderPowermetricsSudoers(t)
	if err != nil {
		t.Fatalf("render sudoers fragment: %v", err)
	}
	assertSudoersGrantsExactly(t, rendered, cmd.Args[2:])
}

// TestPowermetricsArgv_MatchesSudoersRenderedForInterval covers non-default
// --apple-power-interval values: rendering the fragment with the documented
// APPLE_POWER_INTERVAL_MS for an interval must grant exactly the argv the
// agent runs for it, including when the agent raises a short interval to
// the 100ms floor.
func TestPowermetricsArgv_MatchesSudoersRenderedForInterval(t *testing.T) {
	cases := []struct {
		interval time.Duration
		makeVar  string
	}{
		{interval: 2500 * time.Millisecond, makeVar: "APPLE_POWER_INTERVAL_MS=2500"},
		{interval: 5 * time.Second, makeVar: "APPLE_POWER_INTERVAL_MS=5000"},
		{interval: 50 * time.Millisecond, makeVar: "APPLE_POWER_INTERVAL_MS=100"},
	}
	for _, tc := range cases {
		t.Run(tc.interval.String(), func(t *testing.T) {
			t.Parallel()
			cmd := defaultPowermetricsCommand(context.Background(), DefaultPowermetricsBin, tc.interval)
			if len(cmd.Args) < 2 || cmd.Args[0] != defaultSudoBin || cmd.Args[1] != "-n" {
				t.Fatalf("argv = %v, want it to start with %s -n", cmd.Args, defaultSudoBin)
			}
			rendered, err := renderPowermetricsSudoers(t, tc.makeVar)
			if err != nil {
				t.Fatalf("render sudoers fragment: %v", err)
			}
			assertSudoersGrantsExactly(t, rendered, cmd.Args[2:])
			assertSudoersGrantsExactly(t, rendered, powermetricsArgv(DefaultPowermetricsBin, tc.interval))
		})
	}
}

// assertSudoersGrantsExactly requires the single rule in a rendered sudoers
// fragment to grant argv and nothing else. sudoers escapes the comma in
// cpu_power\,gpu_power; the backslash is not part of the argument sudo
// matches.
func assertSudoersGrantsExactly(t *testing.T, rendered string, argv []string) {
	t.Helper()
	spec := sudoersCommandSpec(t, singleSudoersRule(t, rendered))
	if got, want := strings.ReplaceAll(spec, `\,`, ","), strings.Join(argv, " "); got != want {
		t.Errorf("sudoers rule grants %q, agent runs %q", got, want)
	}
}

// TestRenderedPowermetricsSudoers_PassesVisudo syntax-checks the fragment
// exactly as `make install-powermetrics-sudo` renders it, with the same
// `visudo -cf` the install target runs. visudo -cf on a file does not need
// root.
func TestRenderedPowermetricsSudoers_PassesVisudo(t *testing.T) {
	visudo, err := exec.LookPath("visudo")
	if err != nil {
		visudo = "/usr/sbin/visudo"
		if _, statErr := os.Stat(visudo); statErr != nil {
			t.Skipf("visudo not found: %v", err)
		}
	}
	for _, makeVars := range [][]string{nil, {"APPLE_POWER_INTERVAL_MS=2500"}} {
		rendered, err := renderPowermetricsSudoers(t, makeVars...)
		if err != nil {
			t.Fatalf("render %v: %v", makeVars, err)
		}
		path := filepath.Join(t.TempDir(), "llmkube-powermetrics")
		if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(visudo, "-cf", path).CombinedOutput(); err != nil {
			t.Errorf("visudo -cf rejected the fragment rendered with %v: %v\n%s", makeVars, err, out)
		}
	}
}

func TestPowermetricsIntervalMS(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     int
	}{
		{interval: time.Second, want: 1000},
		{interval: 2500 * time.Millisecond, want: 2500},
		{interval: 1234567 * time.Microsecond, want: 1234}, // truncated, not rounded
		{interval: 100 * time.Millisecond, want: 100},
		{interval: 99 * time.Millisecond, want: 100},
		{interval: 0, want: 100},
	}
	for _, tc := range cases {
		if got := powermetricsIntervalMS(tc.interval); got != tc.want {
			t.Errorf("powermetricsIntervalMS(%v) = %d, want %d", tc.interval, got, tc.want)
		}
	}
}

// fakeSudo writes a stand-in for /usr/bin/sudo whose policy permits only the
// argv the fragment rendered with the default interval grants, the way the
// real sudo answers `sudo -n -l <argv>`: print the command and exit 0 when
// permitted, exit 1 otherwise.
func fakeSudo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sudo")
	body := `if [ "$#" -eq 7 ] && [ "$1" = -n ] && [ "$2" = -l ] && [ "$3" = /usr/bin/powermetrics ] &&
   [ "$4" = --samplers ] && [ "$5" = cpu_power,gpu_power ] && [ "$6" = -i ] && [ "$7" = 1000 ]; then
	echo "$3 $4 $5 $6 $7"
	exit 0
fi
echo "sudo: a password is required" >&2
exit 1
`
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
	return path
}

func TestSudoPermits_OnlyTheExactArgv(t *testing.T) {
	sudo := fakeSudo(t)
	ctx := context.Background()

	if err := sudoPermits(ctx, sudo, powermetricsArgv(DefaultPowermetricsBin, time.Second)); err != nil {
		t.Errorf("sudoPermits refused the argv the policy permits: %v", err)
	}

	err := sudoPermits(ctx, sudo, powermetricsArgv(DefaultPowermetricsBin, 2*time.Second))
	if err == nil {
		t.Fatal("sudoPermits accepted an argv the policy does not permit")
	}
	if !strings.Contains(err.Error(), "a password is required") {
		t.Errorf("refusal error %q does not carry sudo's reason", err)
	}

	missing := filepath.Join(t.TempDir(), "missing-sudo")
	if err := sudoPermits(ctx, missing, powermetricsArgv(DefaultPowermetricsBin, time.Second)); err == nil {
		t.Error("sudoPermits accepted when sudo could not be run")
	}
}

// TestApplePowerSamplerRun_RefusesWhenSudoersDoesNotPermit requires Run to
// check the exact argv against sudo's policy first and, when the policy does
// not permit it, to log the refusal with the install command for this
// interval and never launch powermetrics.
func TestApplePowerSamplerRun_RefusesWhenSudoersDoesNotPermit(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	s := NewApplePowerSampler(DefaultPowermetricsBin, 2*time.Second, zap.New(core).Sugar())
	var checked []string
	s.permitCheck = func(_ context.Context, argv []string) error {
		checked = argv
		return errors.New("sudo: a password is required")
	}
	launched := false
	s.commandFactory = func(ctx context.Context, _ string, _ time.Duration) *exec.Cmd {
		launched = true
		return exec.CommandContext(ctx, "/usr/bin/true")
	}

	s.Run(context.Background())

	if launched {
		t.Fatal("Run launched powermetrics although sudo does not permit its argv")
	}
	if want := powermetricsArgv(DefaultPowermetricsBin, 2*time.Second); !slices.Equal(checked, want) {
		t.Errorf("permit check got argv %q, want %q", checked, want)
	}
	entries := logs.FilterMessageSnippet("refusing to start powermetrics").All()
	if len(entries) != 1 {
		t.Fatalf("want 1 refusal log line, got %d (all warnings: %v)", len(entries), logs.All())
	}
	want := "make install-powermetrics-sudo APPLE_POWER_INTERVAL_MS=2000"
	if got := entries[0].ContextMap()["remedy"]; got != want {
		t.Errorf("refusal remedy = %v, want %q", got, want)
	}
}

// TestApplePowerSamplerRun_QuietWhenCancelledDuringCheck covers shutdown: a
// permit check that fails because ctx was cancelled is not a policy refusal
// and must not be logged as one.
func TestApplePowerSamplerRun_QuietWhenCancelledDuringCheck(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	s := NewApplePowerSampler(DefaultPowermetricsBin, time.Second, zap.New(core).Sugar())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.permitCheck = func(ctx context.Context, _ []string) error {
		cancel()
		return ctx.Err()
	}
	launched := false
	s.commandFactory = func(ctx context.Context, _ string, _ time.Duration) *exec.Cmd {
		launched = true
		return exec.CommandContext(ctx, "/usr/bin/true")
	}

	s.Run(ctx)

	if launched {
		t.Error("Run launched powermetrics after its context was cancelled")
	}
	if n := logs.Len(); n != 0 {
		t.Errorf("want no warnings on shutdown, got %d: %v", n, logs.All())
	}
}

func TestApplePowerSamplerRun_LaunchesWhenSudoersPermits(t *testing.T) {
	s := NewApplePowerSampler(DefaultPowermetricsBin, time.Second, zap.NewNop().Sugar())
	s.permitCheck = func(context.Context, []string) error { return nil }
	launched := false
	s.commandFactory = func(ctx context.Context, _ string, _ time.Duration) *exec.Cmd {
		launched = true
		return exec.CommandContext(ctx, "/usr/bin/true")
	}

	s.Run(context.Background())

	if !launched {
		t.Error("Run did not launch powermetrics although sudo permits its argv")
	}
}

// TestNewApplePowerSampler_RejectsNonDefaultBin guards H-2 from the security
// audit: the --powermetrics-bin override flag must not produce a working
// sampler against a path the shipped sudoers entry doesn't match.
func TestNewApplePowerSampler_RejectsNonDefaultBin(t *testing.T) {
	logger := zap.NewNop().Sugar()
	s := NewApplePowerSampler("/tmp/not-the-real-powermetrics", time.Second, logger)
	if !s.disabled {
		t.Fatal("expected sampler to be disabled when bin diverges from default")
	}
	// Run must be a fast no-op — if the disable check is missed it would try
	// to fork sudo, which we definitely don't want in unit tests.
	done := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disabled sampler Run did not return promptly")
	}
}

func TestStderrCapture_TruncatesAtMax(t *testing.T) {
	var sb strings.Builder
	c := stderrCapture{w: &sb, max: 10}
	n, err := c.Write([]byte("12345"))
	if err != nil || n != 5 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}
	n, err = c.Write([]byte("67890ABCDE"))
	if err != nil || n != 10 {
		t.Fatalf("second write: n=%d err=%v", n, err)
	}
	if got := sb.String(); got != "1234567890" {
		t.Errorf("captured %q, want %q", got, "1234567890")
	}
	// Further writes are silently swallowed.
	n, err = c.Write([]byte("XYZ"))
	if err != nil || n != 3 {
		t.Fatalf("post-cap write: n=%d err=%v", n, err)
	}
	if got := sb.String(); got != "1234567890" {
		t.Errorf("after overflow %q, want %q", got, "1234567890")
	}
}
