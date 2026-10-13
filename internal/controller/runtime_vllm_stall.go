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

// vllmStallScript is the stall detector the exec liveness probe runs. It is
// the single source of the detection logic: the tests execute exactly these
// bytes against a stub /metrics server. The script must contain no bare %
// byte, so fmt.Sprintf stays safe. The LLMKUBE_STALL_STATE_FILE env var lets
// the tests point the state file at a temp dir; production probes use the
// /tmp default baked into the script.
//
// Behavior:
//   - metrics unreachable: exit 1. This probe replaced the /health liveness,
//     so an engine the probe cannot reach must still be restarted.
//   - the running gauge or either token counter absent: exit 0. Older vLLM
//     builds may not publish them; a restart on missing telemetry would be a
//     false positive, so missing data fails open. Engine death is covered by
//     the fetch-failure branch above.
//   - running == 0: record the baseline and exit 0. An idle engine is never
//     stalled.
//   - running > 0 with a counter reset (a counter below its last recorded
//     value): record a fresh baseline and exit 0. Counters are monotonic
//     within an engine lifetime, so a decrease means the metrics world
//     restarted, not that the engine unstalled.
//   - running > 0 with progress (a counter above its last recorded value,
//     or vllm:kv_cache_usage_perc changed beyond float noise): record and
//     exit 0. Measured on vLLM v0.31.0, a ~200K-token prefill left BOTH
//     token counters flat for its whole 13.7s while kv_cache_usage_perc
//     climbed steadily, so the kv gauge is the mid-prefill heartbeat; it is
//     optional, and absent keeps the counter-only judgment.
//   - running > 0 with every signal flat for windowSeconds: exit 1. The
//     baseline is kept, so later probes keep failing until kubelet restarts
//     the container; the restart also clears the wedge.
//
// The timestamp is epoch seconds: each kubelet probe exec is a fresh
// process, so a per-process monotonic clock cannot carry state between them.
// /tmp is the container's ephemeral filesystem, so a restarted container
// starts with no baseline and cannot inherit a stale one.
//
// The parser mirrors parsePrometheusGaugeSum exactly, NaN included:
// float("nan") parses and every comparison against it is false, so a NaN
// counter reads as flat. The two vLLM counters are plain totals and do not
// emit NaN; deviating from the Go parser here would split the semantics.
const vllmStallScript = `import json, os, sys, tempfile, time, urllib.request

METRICS_URL = "http://127.0.0.1:%[1]d/metrics"
WINDOW = %[2]d
STATE_PATH = os.environ.get("LLMKUBE_STALL_STATE_FILE", "/tmp/llmkube-vllm-stall.json")
RUNNING = "vllm:num_requests_running"
GEN = "vllm:generation_tokens_total"
PROMPT = "vllm:prompt_tokens_total"
KV = "vllm:kv_cache_usage_perc"
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
        kv = state.get("kv")
        return (float(state["gen"]), float(state["prompt"]),
                None if kv is None else float(kv), float(state["ts"]))
    except Exception:
        return None


def write_state(running, gen, prompt, kv, ts):
    state = {"running": running, "gen": gen, "prompt": prompt, "kv": kv, "ts": ts}
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

running, running_found = gauge_sum(body, RUNNING)
gen, gen_found = gauge_sum(body, GEN)
prompt, prompt_found = gauge_sum(body, PROMPT)
if not running_found or not gen_found or not prompt_found:
    print("stall-probe: required metrics absent; cannot judge", file=sys.stderr)
    sys.exit(0)

kv_val, kv_found = gauge_sum(body, KV)
kv = kv_val if kv_found else None

now = time.time()
if running <= 0:
    write_state(running, gen, prompt, kv, now)
    sys.exit(0)

prev = read_state()
if prev is None:
    write_state(running, gen, prompt, kv, now)
    sys.exit(0)

prev_gen, prev_prompt, prev_kv, prev_ts = prev

# Counters are monotonic within an engine lifetime; a decrease means the
# metrics world restarted, so start a fresh window.
if gen < prev_gen or prompt < prev_prompt:
    write_state(running, gen, prompt, kv, now)
    sys.exit(0)

if gen > prev_gen or prompt > prev_prompt:
    write_state(running, gen, prompt, kv, now)
    sys.exit(0)

# kv cache usage is the mid-prefill heartbeat: it climbs while a long
# prefill fills the cache with both token counters flat. Any real change
# counts, up or down; EPS sits above float-print noise.
if prev_kv is not None and kv is not None and abs(kv - prev_kv) > EPS:
    write_state(running, gen, prompt, kv, now)
    sys.exit(0)

if now - prev_ts >= WINDOW:
    print("stall-probe: running", running, "with no counter or kv-cache progress for",
          now - prev_ts, "s (window", WINDOW, "s)", file=sys.stderr)
    sys.exit(1)

sys.exit(0)
`

// stallProbeCommand builds the argv for the stall-detecting liveness probe.
// The detector tests execute the returned bytes against a stub /metrics
// server, so what is shipped is what is tested.
func stallProbeCommand(port, windowSeconds int32) []string {
	return []string{"python3", "-c", fmt.Sprintf(vllmStallScript, port, windowSeconds)}
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
	*liveness = corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: stallProbeCommand(port, window)},
		},
		PeriodSeconds:    15,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
}
