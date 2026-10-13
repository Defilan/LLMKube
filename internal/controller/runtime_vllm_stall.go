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

package controller

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// vllmStallDefaultWindowSeconds is the stall window when
// spec.vllmConfig.stallDetection.windowSeconds is unset. The kv-cache
// heartbeat keeps even multi-minute prefills safe, so the margin exists to
// keep ordinary scheduler hiccups between probes from looking like stalls;
// restart latency is roughly window + liveness period x failure threshold
// (about 165s at the defaults).
const vllmStallDefaultWindowSeconds int32 = 120

// stallSignals names the Prometheus series a runtime's stall detector reads.
// Busy gauges decide whether the engine has work in flight; progress
// counters advancing (or resetting) and progress gauges changing prove it is
// still moving; required series must all be present or the probe fails
// open, because a restart on missing telemetry would be a false positive.
type stallSignals struct {
	Busy     []string
	Counters []string
	Gauges   []string
	Required []string
}

// vllmStallSignals: vLLM's prompt and generation counters only advance when
// a prefill completes or a request finishes, so a long prefill leaves both
// flat; vllm:kv_cache_usage_perc climbs throughout and is the mid-prefill
// heartbeat (measured on vLLM v0.31.0, a ~200K-token prefill: counters flat
// for 13.7s, kv usage rising every second). The kv gauge is optional.
var vllmStallSignals = stallSignals{
	Busy:     []string{"vllm:num_requests_running"},
	Counters: []string{"vllm:generation_tokens_total", "vllm:prompt_tokens_total"},
	Gauges:   []string{"vllm:kv_cache_usage_perc"},
	Required: []string{"vllm:num_requests_running", "vllm:generation_tokens_total", "vllm:prompt_tokens_total"},
}

// stallScript is the stall detector the exec liveness probe runs, rendered
// per runtime from stallSignals. It is the single source of the detection
// logic: the tests execute exactly the rendered bytes against a stub
// /metrics server. The template must contain no bare % byte, so fmt.Sprintf
// stays safe. The LLMKUBE_STALL_STATE_FILE env var lets the tests point the
// state file at a temp dir; production probes use the /tmp default baked
// into the script.
//
// Behavior:
//   - metrics unreachable: exit 1. This probe replaced the /health liveness,
//     so an engine the probe cannot reach must still be restarted.
//   - a required series absent: exit 0. Missing telemetry fails open; engine
//     death is covered by the fetch-failure branch above.
//   - no busy gauge above zero: record the baseline and exit 0. An idle
//     engine is never stalled.
//   - busy with a counter below its last recorded value: record a fresh
//     baseline and exit 0. Counters are monotonic within an engine lifetime,
//     so a decrease means the metrics world restarted.
//   - busy with progress (a counter above its last recorded value, or a
//     progress gauge changed beyond float noise): record and exit 0.
//   - busy with every signal flat for windowSeconds: exit 1. The baseline is
//     kept, so later probes keep failing until kubelet restarts the
//     container; the restart also clears the wedge.
//
// The host is localhost so the probe reaches the server whichever address
// family it binds: urllib tries every address localhost resolves to. A probe
// pinned to 127.0.0.1 is refused by a server bound IPv6-only (#2027).
//
// The timestamp is epoch seconds: each kubelet probe exec is a fresh
// process, so a per-process monotonic clock cannot carry state between them.
// /tmp is the container's ephemeral filesystem, so a restarted container
// starts with no baseline and cannot inherit a stale one.
//
// The parser mirrors parsePrometheusGaugeSum exactly, NaN included:
// float("nan") parses and every comparison against it is false, so a NaN
// value reads as flat.
const stallScript = `import json, os, sys, tempfile, time, urllib.request

METRICS_URL = "http://%[1]s:%[2]d/metrics"
WINDOW = %[3]d
BUSY = %[4]s
COUNTERS = %[5]s
GAUGES = %[6]s
REQUIRED = %[7]s
STATE_PATH = os.environ.get("LLMKUBE_STALL_STATE_FILE", "/tmp/llmkube-stall.json")
EPS = 1e-9


def fetch(url):
    try:
        with urllib.request.urlopen(url, timeout=4) as resp:
            if resp.status != 200:
                print("stall-probe: metrics status", resp.status, file=sys.stderr)
                return None
            return resp.read().decode("utf-8", "replace")
    except Exception as exc:
        print("stall-probe: metrics fetch failed:", exc, file=sys.stderr)
        return None


def gauge_sum(body, metric):
    # Mirrors parsePrometheusGaugeSum in drain_before_rollout.go: prefix
    # match, optional {labels} or a space, last field is the value, summed
    # across labeled series, comment lines skipped.
    total, found = 0.0, False
    for raw in body.split("\n"):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if not line.startswith(metric):
            continue
        rest = line[len(metric):]
        if not rest or (rest[0] != "{" and rest[0] != " "):
            continue
        fields = line.split()
        if len(fields) < 2:
            continue
        try:
            total += float(fields[-1])
            found = True
        except ValueError:
            continue
    return total, found


def read_state():
    try:
        with open(STATE_PATH) as f:
            state = json.load(f)
        return dict(state["values"]), float(state["ts"])
    except Exception:
        return None


def write_state(values, ts):
    state = {"values": values, "ts": ts}
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(STATE_PATH) or ".")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(state, f)
        os.replace(tmp, STATE_PATH)
    except Exception as exc:
        print("stall-probe: state write failed:", exc, file=sys.stderr)
        try:
            os.unlink(tmp)
        except OSError:
            pass


body = fetch(METRICS_URL)
if body is None:
    sys.exit(1)

values, found = {}, {}
for name in set(BUSY + COUNTERS + GAUGES + REQUIRED):
    values[name], found[name] = gauge_sum(body, name)
missing = [name for name in REQUIRED if not found[name]]
if missing:
    print("stall-probe: required metrics absent; cannot judge:", missing, file=sys.stderr)
    sys.exit(0)

now = time.time()
current = {name: values[name] for name in COUNTERS + GAUGES if found[name]}
if not any(found[name] and values[name] > 0 for name in BUSY):
    write_state(current, now)
    sys.exit(0)

prev = read_state()
if prev is None:
    write_state(current, now)
    sys.exit(0)
prev_values, prev_ts = prev

for name in COUNTERS:
    if name in current and name in prev_values and current[name] < prev_values[name]:
        write_state(current, now)
        sys.exit(0)

progress = any(name in current and name in prev_values and current[name] > prev_values[name]
               for name in COUNTERS)
progress = progress or any(name in current and name in prev_values
                           and abs(current[name] - prev_values[name]) > EPS
                           for name in GAUGES)
if progress:
    write_state(current, now)
    sys.exit(0)

if now - prev_ts >= WINDOW:
    print("stall-probe: busy with no counter or gauge progress for",
          now - prev_ts, "s (window", WINDOW, "s)", file=sys.stderr)
    sys.exit(1)

sys.exit(0)
`

// stallProbeHost is where the probe reaches the server from inside the pod.
const stallProbeHost = "localhost"

// pyList renders a Go string slice as a Python list literal (JSON's string
// list syntax is valid Python).
func pyList(names []string) string {
	b, _ := json.Marshal(names)
	return string(b)
}

// stallProbeCommandFor builds the argv for a runtime's stall-detecting
// liveness probe. The detector tests execute the returned bytes against a
// stub /metrics server, so what is shipped is what is tested.
func stallProbeCommandFor(sig stallSignals, port, windowSeconds int32) []string {
	return []string{"python3", "-c", fmt.Sprintf(stallScript, stallProbeHost, port, windowSeconds,
		pyList(sig.Busy), pyList(sig.Counters), pyList(sig.Gauges), pyList(sig.Required))}
}

// stallProbeCommand is the vLLM stall probe argv.
func stallProbeCommand(port, windowSeconds int32) []string {
	return stallProbeCommandFor(vllmStallSignals, port, windowSeconds)
}

// stallLivenessProbe wraps a stall probe argv in the liveness timings the
// /health liveness it replaces used.
func stallLivenessProbe(cmd []string) corev1.Probe {
	return corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: cmd},
		},
		PeriodSeconds:    15,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
}

// stallDetectionEnabled resolves spec.vllmConfig.stallDetection: the window
// to use, and whether the feature is opted into. Enabled mirrors
// SpeculativeConfig.Enabled: nil or false means off.
func stallDetectionEnabled(isvc *inferencev1alpha1.InferenceService) (int32, bool) {
	cfg := isvc.Spec.VLLMConfig
	if cfg == nil || cfg.StallDetection == nil ||
		cfg.StallDetection.Enabled == nil || !*cfg.StallDetection.Enabled {
		return 0, false
	}
	window := vllmStallDefaultWindowSeconds
	if w := cfg.StallDetection.WindowSeconds; w != nil {
		window = *w
	}
	return window, true
}

// CustomizeProbes implements ProbeCustomizer. With stall detection opted
// into, the liveness probe stops asking vLLM /health (which stays 200 while
// a wedged engine holds requests) and instead execs the detector script;
// startup and readiness stay on /health. spec.probeOverrides.liveness still
// wins because the deployment builder applies overrides after this.
func (b *VLLMBackend) CustomizeProbes(
	isvc *inferencev1alpha1.InferenceService,
	port int32,
	startup, liveness, readiness *corev1.Probe,
) {
	window, enabled := stallDetectionEnabled(isvc)
	if !enabled {
		return
	}
	*liveness = stallLivenessProbe(stallProbeCommand(port, window))
}
