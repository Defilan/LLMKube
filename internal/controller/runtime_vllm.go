package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// vllmLog is a package-level logger used for construction-time warnings from
// BuildArgs.
var vllmLog = logf.Log.WithName("runtime.vllm")

// Condition types set by the InferenceService reconciler when a vLLM-specific
// portion of the spec is structurally invalid but not fatal to reconciliation.
const (
	// ConditionVLLMSpecValid is True when the VLLMConfig is internally
	// consistent. It is False when, for example, speculative decoding is
	// enabled without a draft model reference.
	ConditionVLLMSpecValid = "VLLMSpecValid"

	// RuntimeVLLM is the InferenceService.Spec.Runtime value that selects the
	// vLLM backend. Kept as a named constant so callers can cross-check the
	// runtime without duplicating the string literal.
	RuntimeVLLM = "vllm"
)

type VLLMBackend struct{}

func (b *VLLMBackend) ContainerName() string { return "vllm" }

// DefaultImage pins the newest stable vLLM release (verified 2026-07-21 via
// github.com/vllm-project/vllm/releases and the Docker Hub tag list).
// Blackwell (sm_100/B200) has been first-class since before v0.20.0 (FA4
// default on SM100, MXFP4 CUTLASS MoE; see the v0.20.0 release notes), so any
// tag at or above that floor is B200-safe; v0.25.1 is simply current. The
// default build ships CUDA 13 userspace; fleets on the 570 driver branch can
// pin the v0.25.1-cu129 variant via runtimeImages.vllm or spec.image.
func (b *VLLMBackend) DefaultImage() string { return "vllm/vllm-openai:v0.25.1" }
func (b *VLLMBackend) DefaultPort() int32   { return 8000 }

// SupportedArchitectures reports the architectures the operator-chosen vLLM
// image supports. The default vllm/vllm-openai image is published for both
// amd64 and arm64, so no constraint is applied (#1479).
func (b *VLLMBackend) SupportedArchitectures() []string { return nil }
func (b *VLLMBackend) NeedsModelInit() bool             { return true }
func (b *VLLMBackend) DefaultHPAMetric() string         { return "vllm:num_requests_running" }

// BuildCommand returns the entrypoint for the vLLM container, mirroring
// SGLangBackend/PersonaPlexBackend. The stock vllm/vllm-openai image already
// entrypoints on "vllm serve" (the positional-model form BuildArgs emits), so
// setting it explicitly is behavior-preserving for that image while making the
// runtime image-agnostic: a custom image (e.g. a community AMD ROCm/gfx1151
// build) no longer has to match the stock entrypoint or set spec.command to
// avoid exec'ing the positional model path as the container process.
func (b *VLLMBackend) BuildCommand() []string {
	return []string{"vllm", "serve"}
}

// DisableServiceLinks returns true so the operator emits Pods with
// `enableServiceLinks: false`. vLLM v0.20+ logs a warning for every K8s
// service-link env var that matches the `VLLM_*` prefix; in a namespace with
// multiple vLLM Services that's per-pod per-other-service noise. DNS-based
// service discovery is unaffected — the env vars were a legacy mechanism.
func (b *VLLMBackend) DisableServiceLinks() bool { return true }

// BuildArgs generates the vLLM server CLI arguments. Arguments are emitted in a
// deterministic order so snapshot-style tests and diff reviews stay stable:
//
//  1. --model, --host, --port (always)
//  2. Typed VLLMConfig flags, top-to-bottom as declared on the struct
//  3. Tensor parallel auto-derived from GPU count (when user did not set it)
//  4. ExtraArgs (user escape hatch, always last so user flags win)
//
// Flags follow a strict "only-emit-on-explicit-opt-in" rule: boolean toggles
// whose zero value matches vLLM's own default are only emitted when the user
// set them to true. This keeps generated pod specs minimal and lets us track
// vLLM upstream default changes without needing an operator release.
func (b *VLLMBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, _ string, port int32) []string {
	source := modelPath
	if source == "" {
		// Serve path: hand vLLM the bare "org/name[@rev]" repo id, not a resolve
		// URL (which it rejects). See hfServeArg.
		source = hfServeArg(model.Spec.Source)
	}
	// vLLM v0.20 deprecated --model in favor of a positional argument; --model
	// will be removed in a future minor. The positional form is supported by
	// every vLLM release we run against, so this works on both v0.19 (silent
	// accept) and v0.20+ (no deprecation warning).
	args := []string{
		source,
		"--port", fmt.Sprintf("%d", port),
	}

	// BindAddress: default "::" (dual-stack wildcard, #972/#973). Skip if
	// user already set --host in extraArgs (extraArgs wins).
	if !hasMatchingExtraArg(isvc.Spec.ExtraArgs, "host") {
		bindAddr := "::"
		if isvc.Spec.BindAddress != "" {
			bindAddr = isvc.Spec.BindAddress
		}
		args = append(args, "--host", bindAddr)
	}
	// NOTE: vLLM has no --enable-metrics flag. Its OpenAI server always exposes
	// Prometheus metrics at /metrics, so there is nothing to enable; passing the
	// flag makes vLLM's argument parser reject the whole command and the pod
	// never starts (#1030). The PodMonitor scrapes /metrics unconditionally.

	var err error

	cfg := isvc.Spec.VLLMConfig
	gpuCount := resolveGPUCount(isvc, model)
	if cfg != nil {
		args = appendTensorParallelSize(args, cfg.TensorParallelSize)
		args = appendPipelineParallelSize(args, cfg.PipelineParallelSize)
		args = appendMaxModelLen(args, cfg.MaxModelLen)
		args = appendQuantization(args, cfg.Quantization)
		args = appendDtype(args, cfg.Dtype)
		args = appendKVCacheDtype(args, cfg.KVCacheDtype, cfg.KVCacheCustomDtype)
		args = appendEnablePrefixCaching(args, cfg.EnablePrefixCaching)
		args = appendEnableChunkedPrefill(args, cfg.EnableChunkedPrefill)
		args = appendMaxNumBatchedTokens(args, cfg.MaxNumBatchedTokens)
		args = appendAttentionBackend(args, cfg.AttentionBackend)
		args, err = appendSpeculativeModel(args, cfg.Speculative)
		if err != nil {
			vllmLog.Info(
				err.Error(),
				"inferenceService", isvc.Name,
				"namespace", isvc.Namespace,
			)
		}
		args = appendEnableExpertParallel(args, cfg.EnableExpertParallel)

		args, err = appendCPUOffloadGB(args, cfg.CPUOffloadGB, gpuCount)
		if err != nil {
			vllmLog.Info(
				err.Error(),
				"inferenceService", isvc.Name,
				"namespace", isvc.Namespace,
			)
		}
		args, err = appendGPUMemoryUtilization(args, cfg.GPUMemoryUtilization, gpuCount)
		if err != nil {
			vllmLog.Info(
				err.Error(),
				"inferenceService", isvc.Name,
				"namespace", isvc.Namespace,
			)
		}
	}

	if gpuCount > 1 && (cfg == nil || cfg.TensorParallelSize == nil) {
		args = appendTensorParallelSize(args, &gpuCount)
	}

	args, err = appendMaxNumSeqsArgs(args, isvc.Spec.ParallelSlots, isvc.Spec.ExtraArgs)
	if err != nil {
		vllmLog.Info(
			err.Error(),
			"inferenceService", isvc.Name,
			"namespace", isvc.Namespace,
		)
	}

	if len(isvc.Spec.ExtraArgs) > 0 {
		args = append(args, isvc.Spec.ExtraArgs...)
	}

	return args
}

// ValidateVLLMConfig checks the VLLMConfig for structurally invalid
// combinations that are non-fatal to reconciliation but should be surfaced as
// a status condition. Returns (reason, message) when invalid; empty strings
// when the config is fine. The caller is expected to translate these into a
// metav1.Condition on the InferenceService status.
//
// Only one (reason, message) is returned, so check ordering is precedence:
// SpeculativeMissingModel (a flag that gets skipped entirely) outranks
// CPUOffloadUnverified (a flag with an open upstream reliability report).
func ValidateVLLMConfig(isvc *inferencev1alpha1.InferenceService) (reason, message string) {
	if isvc == nil || isvc.Spec.VLLMConfig == nil {
		return "", ""
	}
	cfg := isvc.Spec.VLLMConfig
	if cfg.Speculative != nil && cfg.Speculative.Enabled != nil && *cfg.Speculative.Enabled {
		if cfg.Speculative.Model == "" {
			return "SpeculativeMissingModel",
				"spec.vllmConfig.speculative.enabled is true but spec.vllmConfig.speculative.model is empty; speculative decoding flags will be skipped"
		}
	}
	// TODO(#1320): remove once vllm-project/vllm#48468 ships in DefaultImage.
	if cfg.CPUOffloadGB != nil && *cfg.CPUOffloadGB > 0 {
		return "CPUOffloadUnverified", cpuOffloadIneffectiveMessage
	}
	return "", ""
}

// cpuOffloadIneffectiveMessage is shared by the VLLMSpecValid condition and
// the reconcile-time Warning Event so the two surfaces cannot drift. Worded
// as a dated factual claim so it stays true if upstream later fixes it.
const cpuOffloadIneffectiveMessage = "spec.vllmConfig.cpuOffloadGB is set. " +
	"vLLM's --cpu-offload-gb is reported silently ineffective in some " +
	"configurations on current releases (vllm-project/vllm#48468, open): the " +
	"flag is accepted but no weights offload, so VRAM-tight models OOM instead " +
	"of spilling to host RAM. It is verified working in others (see the " +
	"cpu-offload sample). Confirm it took effect in the server logs, via " +
	"'Offloader set to' / 'offloaded parameters' lines or a Model-loading size " +
	"below the full footprint, before relying on it"

func (b *VLLMBackend) BuildProbes(port int32) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
	startup := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 180,
	}
	liveness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    15,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	readiness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 3,
	}
	return startup, liveness, readiness
}

// IdleProbe returns a probe closure that checks vLLM /metrics for
// `vllm:num_requests_running` gauge sum. Idle when sum == 0. Absent metric
// returns (false, nil) — fail-closed, treats unknown as busy.
func (b *VLLMBackend) IdleProbe(_ *inferencev1alpha1.InferenceService, client *http.Client) func(ctx context.Context, baseURL string) (bool, error) {
	return func(ctx context.Context, baseURL string) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/metrics", nil)
		if err != nil {
			return false, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Errorf("failed to query /metrics: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("/metrics returned status %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, fmt.Errorf("failed to read /metrics response: %w", err)
		}

		sum, found := parsePrometheusGaugeSum(string(body), "vllm:num_requests_running")
		if !found {
			return false, nil
		}
		return sum == 0, nil
	}
}

func (b *VLLMBackend) BuildEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar {
	cfg := isvc.Spec.VLLMConfig
	if cfg != nil && cfg.HFTokenSecretRef != nil {
		return []corev1.EnvVar{{
			Name:      "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: cfg.HFTokenSecretRef},
		}}
	}
	return nil
}

// BuildMultiNodeArgs renders vLLM's native multi-node flags for one rank.
// vLLM's own launcher: rank 0 runs the API server, every other rank runs
// --headless and joins the torch.distributed rendezvous at master-addr.
func (b *VLLMBackend) BuildMultiNodeArgs(isvc *inferencev1alpha1.InferenceService, rank int32) []string {
	mn := isvc.Spec.MultiNode
	if mn == nil || len(mn.Members) == 0 {
		return nil
	}
	master := ""
	if f := mn.Members[0].Fabric; f != nil {
		master = f.Address
	}
	args := []string{
		"--nnodes", fmt.Sprintf("%d", len(mn.Members)),
		"--node-rank", fmt.Sprintf("%d", rank),
		"--master-addr", master,
		"--master-port", fmt.Sprintf("%d", mn.RendezvousPortOrDefault()),
		"--distributed-executor-backend", "mp",
	}
	if rank > 0 {
		args = append(args, "--headless")
	}
	return args
}

// BuildMultiNodeEnv renders the fabric env for one rank: which NIC carries the
// bootstrap sockets, which HCA NCCL may use, and vLLM's own host IP. Names
// differ per node for the same physical link, which is why they come from the
// member and not the service.
func (b *VLLMBackend) BuildMultiNodeEnv(isvc *inferencev1alpha1.InferenceService, rank int32) []corev1.EnvVar {
	mn := isvc.Spec.MultiNode
	if mn == nil || rank < 0 || int(rank) >= len(mn.Members) {
		return nil
	}
	var env []corev1.EnvVar
	if f := mn.Members[rank].Fabric; f != nil {
		if f.Address != "" {
			env = append(env, corev1.EnvVar{Name: "VLLM_HOST_IP", Value: f.Address})
		}
		if f.SocketInterface != "" {
			for _, name := range []string{"NCCL_SOCKET_IFNAME", "GLOO_SOCKET_IFNAME", "TP_SOCKET_IFNAME"} {
				env = append(env, corev1.EnvVar{Name: name, Value: f.SocketInterface})
			}
		}
		if f.IBHCA != "" {
			env = append(env, corev1.EnvVar{Name: "NCCL_IB_HCA", Value: f.IBHCA})
		}
	}
	if mn.IBGIDIndex != nil {
		env = append(env, corev1.EnvVar{Name: "NCCL_IB_GID_INDEX", Value: fmt.Sprintf("%d", *mn.IBGIDIndex)})
	}
	return env
}
