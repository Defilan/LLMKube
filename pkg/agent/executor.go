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

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/hfsource"
)

type ExecutorConfig struct {
	Name        string
	Namespace   string
	ModelSource string
	ModelName   string
	// SourceSecretRef is the Model's spec.sourceSecretRef, used to resolve
	// credentials for s3:// fetches on the metal path. May be nil for non-s3
	// sources.
	SourceSecretRef *corev1.LocalObjectReference
	GPULayers       int32
	ContextSize     int
	Jinja           bool

	// RopeScaling* map to llama.cpp's RoPE context-extension flags, resolved
	// from InferenceService.spec.ropeScaling at the agent boundary. Empty
	// RopeScalingType omits the flags entirely. Factor maps to --rope-scale;
	// OrigCtx (when > 0) maps to --yarn-orig-ctx.
	RopeScalingType    string
	RopeScalingFactor  string
	RopeScalingOrigCtx int

	// FlashAttention enables llama.cpp's --flash-attn flag. On Apple Silicon
	// this is a clear win for long-context agentic workloads (prevents the
	// ~25% decode degradation observed at 4K+ context on Qwen-class models).
	// Defaults true at the agent → executor boundary.
	FlashAttention bool

	// Mlock pins model weights and KV cache so macOS's wired collector cannot
	// evict our Metal GPU buffers under memory pressure. Defaults true.
	Mlock bool

	// Threads sets --threads. Zero means auto-detect from performance core
	// count via detectPerfCoreCount(); a non-positive detection result causes
	// the flag to be omitted (let llama-server pick).
	Threads int

	// BatchSize sets --batch-size. Zero falls back to 2048, which prompt
	// processing benchmarks on M-series chips treat as a sweet spot.
	BatchSize int

	// UBatchSize sets --ubatch-size. Zero omits the flag (use llama-server's
	// own default).
	UBatchSize int

	// ParallelSlots maps to --parallel. Values <= 1 omit the flag (one slot
	// is the llama-server default and adding the flag is just noise).
	ParallelSlots int

	// CacheTypeK / CacheTypeV are the resolved llama.cpp KV cache types,
	// already passed through CRD custom-vs-standard resolution at the agent
	// boundary. Empty omits the corresponding flag.
	CacheTypeK string
	CacheTypeV string

	// MoeCPUOffload maps to --cpu-moe (offload all MoE expert layers to CPU).
	MoeCPUOffload bool

	// MoeCPULayers maps to --n-cpu-moe (offload first N MoE layers to CPU).
	// Zero omits the flag.
	MoeCPULayers int

	// NoKvOffload maps to --no-kv-offload (keep KV cache on host RAM).
	NoKvOffload bool

	// TensorOverrides become repeated --override-tensor flags.
	TensorOverrides []string

	// MetadataOverrides become repeated --override-kv flags.
	MetadataOverrides []string

	// NoWarmup maps to --no-warmup (skip the prompt-processing warmup pass).
	NoWarmup bool

	// ReasoningBudget maps to --reasoning-budget. Zero omits both this and
	// ReasoningBudgetMessage.
	ReasoningBudget int

	// ReasoningBudgetMessage maps to --reasoning-budget-message. Ignored
	// unless ReasoningBudget > 0.
	ReasoningBudgetMessage string

	// Mode is the serving mode (chat, embedding, rerank) resolved from
	// InferenceService.spec.mode. Empty defaults to chat (no extra flags).
	Mode string

	// ExtraArgs are appended to the command line as-is, last, so they can
	// override any earlier flag llama-server emitted (last-wins).
	ExtraArgs []string

	// TurboQuantBits sets the KV cache quantization bit width for the oMLX
	// runtime (3, 6, or 8). Maps to oMLX --kv-cache-quant. When set, the
	// oMLX daemon uses TurboQuant to compress the KV cache, reducing memory
	// usage by up to 67% with minimal speed impact (~7% overhead). Only
	// meaningful for the omlx runtime; ignored by llamacpp and other runtimes.
	// Requires oMLX v0.3.4+ (which introduced 3-bit TurboQuant) or a later
	// dev build (6-bit and 8-bit options).
	// +optional
	TurboQuantBits int

	// PagedSSDCacheDir maps to oMLX --paged-ssd-cache-dir. When non-empty,
	// the oMLX daemon uses a paged cache backed by the specified directory,
	// allowing models to exceed available RAM by paging KV cache blocks to
	// SSD. Only meaningful for the omlx runtime; ignored by llamacpp and
	// other runtimes.
	PagedSSDCacheDir string

	// HotCacheMaxSize maps to oMLX --hot-cache-max-size. A string value like
	// "100GB" or "50GB". Only meaningful for the omlx runtime; ignored by
	// llamacpp and other runtimes.
	HotCacheMaxSize string

	// PagedSSDCacheMaxSize maps to oMLX --paged-ssd-cache-max-size. A string
	// value like "200GB" or "500GB". Only meaningful for the omlx runtime;
	// ignored by llamacpp and other runtimes.
	PagedSSDCacheMaxSize string
}

// ProcessExecutor is the interface that both llama-server and oMLX executors
// implement. It abstracts process lifecycle so the agent is runtime-agnostic.
type ProcessExecutor interface {
	StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error)
	StopProcess(pid int) error
}

// DefaultLlamaServerStartupTimeout is how long the agent waits for a freshly
// spawned llama-server to respond on /health. Was 30s historically; that's
// fine for sub-30 GB models but breaks for anything larger because llama.cpp's
// mlock pass + warmup grows roughly linearly with model size. Empirically an
// 84 GB model (MiniMax M2.7 IQ3_S on M5 Max) takes ~30+ seconds just for
// mlock; the original timeout would kill the process just before it would
// have been ready. 120s gives generous headroom for the largest models that
// fit in 128 GB unified memory while still failing fast on real breakage.
const DefaultLlamaServerStartupTimeout = 120 * time.Second

type MetalExecutor struct {
	llamaServerBin string
	modelStorePath string
	logger         *zap.SugaredLogger
	startupTimeout time.Duration
	// fixedPort, when non-zero, is the port every spawned llama-server binds
	// instead of an ephemeral one. Set via SetPort. A fixed port gives native
	// OpenAI-compatible clients a stable endpoint across process respawns.
	fixedPort int

	// namespace and k8sClient let the executor resolve a Model's
	// sourceSecretRef for s3:// credentials (resolveS3Credentials). Both are
	// optional: when nil, s3:// sources fail with a clear message rather than
	// silently falling through to an anonymous GET. They are set via
	// WithKubeClient.
	namespace string
	k8sClient client.Client
	caCerts   [][]byte
}

// Option configures a MetalExecutor. Used to wire the Kubernetes client and
// namespace the s3 fetch path needs.
type Option func(*MetalExecutor)

// WithKubeClient attaches the agent's controller-runtime client and the
// InferenceService/Model namespace so ensureModel can resolve sourceSecretRef
// for s3:// sources. caCerts is the set of PEM CA bundles the operator trusts
// (from the same caCertConfigMap the controller uses), so a private MinIO
// behind a self-signed CA is reachable.
func WithKubeClient(namespace string, c client.Client, caCerts [][]byte) Option {
	return func(e *MetalExecutor) {
		e.namespace = namespace
		e.k8sClient = c
		e.caCerts = caCerts
	}
}

func NewMetalExecutor(llamaServerBin, modelStorePath string, logger *zap.SugaredLogger, opts ...Option) *MetalExecutor {
	e := &MetalExecutor{
		llamaServerBin: llamaServerBin,
		modelStorePath: modelStorePath,
		logger:         logger,
		startupTimeout: DefaultLlamaServerStartupTimeout,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// SetStartupTimeout overrides the default llama-server startup timeout.
// Values <= 0 are coerced back to DefaultLlamaServerStartupTimeout.
func (e *MetalExecutor) SetStartupTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultLlamaServerStartupTimeout
	}
	e.startupTimeout = d
}

// SetPort fixes the port every spawned llama-server binds. A value <= 0
// (the default) keeps the historical behavior of allocating an ephemeral
// port per process. Only one llama-server can use a given fixed port, which
// matches the one-process-per-agent expectation of the Metal path.
func (e *MetalExecutor) SetPort(port int) {
	if port < 0 {
		port = 0
	}
	e.fixedPort = port
}

func (e *MetalExecutor) StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error) {
	modelPath, err := e.ensureModel(ctx, config.ModelSource, config.ModelName, config.SourceSecretRef)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure model: %w", err)
	}

	port := e.fixedPort
	if port == 0 {
		var err error
		port, err = e.allocatePort()
		if err != nil {
			return nil, fmt.Errorf("failed to allocate port: %w", err)
		}
	}

	args := buildLlamaServerArgs(modelPath, port, config)

	cmd := exec.Command(e.llamaServerBin, args...)

	cmd.Env = append(os.Environ(),
		"GGML_METAL_ENABLE=1",
		"GGML_METAL_PATH_RESOURCES=/usr/local/share/llama.cpp",
	)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start llama-server: %w", err)
	}

	process := &ManagedProcess{
		Name:      config.Name,
		Namespace: config.Namespace,
		PID:       cmd.Process.Pid,
		Port:      port,
		ModelPath: modelPath,
		StartedAt: time.Now(),
		Healthy:   false,
	}

	if err := e.waitForHealthy(port, e.startupTimeout); err != nil {
		if stopErr := e.StopProcess(process.PID); stopErr != nil {
			e.logger.Warnw("failed to stop unhealthy process after health check failure",
				"pid", process.PID, "port", port, "error", stopErr)
		}
		return nil, fmt.Errorf("process failed health check after %s: %w", e.startupTimeout, err)
	}

	process.Healthy = true
	return process, nil
}

func (e *MetalExecutor) StopProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()

	select {
	case <-time.After(10 * time.Second):
		_ = process.Kill()
		return fmt.Errorf("process %d did not exit gracefully, killed", pid)
	case err := <-done:
		return err
	}
}

func (e *MetalExecutor) ensureModel(ctx context.Context, source, name string, secretRef s3SecretRef) (string, error) {
	filename := filepath.Base(source)
	localPath := filepath.Join(e.modelStorePath, name, filename)

	if info, err := os.Stat(localPath); err == nil && info.Size() > 0 {
		e.logger.Debugw("model already downloaded", "path", localPath)
		return localPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create model directory: %w", err)
	}

	e.logger.Infow("downloading model", "source", source, "destination", localPath)
	if err := e.fetchModel(ctx, source, localPath, secretRef); err != nil {
		return "", fmt.Errorf("failed to download model: %w", err)
	}

	e.logger.Infow("model downloaded", "path", localPath)
	return localPath, nil
}

// fetchModel downloads source to filePath. s3:// sources are routed through a
// sigv4-signed client (the metal half of #1449, which #1450 fixed for the
// controller path): the raw source never reaches a plain GET.
func (e *MetalExecutor) fetchModel(ctx context.Context, source, filePath string, secretRef s3SecretRef) error {
	if isS3Source(source) {
		return e.downloadS3(ctx, source, filePath, secretRef)
	}
	// Gated and private Hugging Face repositories need a bearer token (#1750).
	// Read from the same sourceSecretRef the S3 path uses, and attach it only
	// for Hugging Face sources (hf:// or huggingface.co) so a Model pointing
	// at another host never sees it. The hf:// source downloadFile receives is
	// resolved to its huggingface.co form before the request is built, so the
	// scheme works the same as the init-container path.
	var token string
	if isHFAuthHost(source) && secretRef != nil {
		token = e.resolveHFToken(ctx, secretRef.Name)
	}
	return e.downloadFile(ctx, source, filePath, token)
}

// downloadS3 fetches an s3:// source into filePath using AWS SigV4 signing and
// the operator's trusted CA bundle, mirroring the controller's parseS3GGUFMetadata
// (internal/controller/model_controller.go) and the init container's signed curl
// (buildS3DownloadCommand, internal/controller/model_storage.go). secretRef is
// the Model's spec.sourceSecretRef; when nil the fetch fails clearly rather than
// falling back to an anonymous GET.
func (e *MetalExecutor) downloadS3(ctx context.Context, source, filePath string, secretRef s3SecretRef) error {
	bucket, _, err := parseS3Source(source)
	if err != nil {
		return err
	}

	var secretName string
	if secretRef != nil {
		secretName = secretRef.Name
	}
	creds, err := e.resolveS3Credentials(ctx, source, secretName)
	if err != nil {
		return err
	}

	httpClient, objectURL, err := e.s3DownloadClient(source, creds)
	if err != nil {
		return err
	}

	e.logger.Infow("downloading model from S3", "bucket", bucket, "destination", filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, objectURL, nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("s3 GET %s: %s", objectURL, resp.Status)
	}

	// S3 does not resume: it writes the plain ".partial" path and starts from
	// zero every attempt (sigv4 range signing is unverified). This is the same
	// behaviour it had before resume existed (#1765).
	return e.copyToFileNoResume(filePath, resp.Body, resp.ContentLength)
}

// hfNormalize is the source resolver the download path applies before the
// request is built: hf:// sources become their huggingface.co HTTPS resolve
// URLs, everything else passes through. A variable so tests can pin the
// resolved host to a reachable server.
var hfNormalize = hfsource.NormalizeHFSource

// downloadFile fetches url into filePath, resuming from a validator-keyed
// partial left by an interrupted attempt. token, when non-empty, is sent as a
// bearer credential on the FIRST hop only.
//
// Resume works by asking upstream for a validator (a HEAD reading ETag plus
// Content-Length), deriving the partial name from it, and, when a matching
// partial survives, sending "Range: bytes=N-" for the remainder. Like curl -C -,
// the Range request sends no If-Range, so the content key is what makes the
// resume safe: a content change yields a different validator, hence a different
// partial name, so the abandoned bytes are never appended onto. An origin that
// ignores the Range and answers 200 is handled by discarding the partial and
// rewriting from zero.
//
// Every transfer, the from-zero one included, writes the probed validator's
// partial, so an interruption at any offset leaves progress the next attempt
// resumes. Only the probe-failure path (downloadFull with an empty validator)
// has no key to write under and uses the fixed empty-validator name.
//
// The redirect handling is deliberate and stricter than net/http's default.
// Go strips Authorization only when a redirect leaves the registrable domain
// (shouldCopyHeaderOnRedirect uses isDomainOrSubdomain), so it keeps the header
// for a subdomain, and keeps it for a different port on the same host. Hugging
// Face answers a weights request with a redirect to its CDN, and whether that
// lands on another domain or a subdomain is not ours to depend on: a token
// scoped to huggingface.co has no business reaching a content host either way,
// which is also why huggingface_hub does not send it there. hfRedirectStripper
// therefore drops the header on ANY change of host.
func (e *MetalExecutor) downloadFile(ctx context.Context, url, filePath, token string) error {
	url = hfNormalize(url)

	httpClient := http.DefaultClient
	if token != "" {
		httpClient = hfRedirectStripper()
	}

	validator, size, err := probeValidator(ctx, httpClient, url, token)
	if err != nil {
		// A failed probe is not fatal to the download: treat the content as
		// unknown, start from zero, and let the GET below surface any real error.
		e.logger.Debugw("validator probe failed; downloading without resume", "url", url, "error", err)
		return e.downloadFull(ctx, httpClient, url, filePath, token, "")
	}

	partPath := validatorPartialPath(filePath, validator)

	// Sweep partials left by a different validator before starting. Content
	// keying already guarantees those stale bytes can never be resumed into (the
	// key differs), but leaving them lets them accumulate across content
	// changes: an unpinned repo whose bytes rotate keeps a fresh key on every
	// change and drops the old partial each time this runs, which bounds the
	// debris. The current key is exempt so an interrupted transfer keeps its own
	// resumable partial.
	sweepStalePartials(filePath, partPath)

	var resumeFrom int64
	if fi, statErr := os.Stat(partPath); statErr == nil && fi.Mode().IsRegular() {
		// The partial is keyed on the validator, so its very name proves it
		// belongs to the current content: a change in length or ETag yields a
		// different key and this stat would miss it.
		switch {
		case size > 0 && fi.Size() < size:
			resumeFrom = fi.Size()
		case size > 0 && fi.Size() == size:
			// Already complete: a ranged request would 416 and the full-body
			// path would rewrite identical bytes anyway, so publish the partial
			// as-is. Renaming the content-keyed partial onto the final path also
			// clears the key, so a later content change re-downloads cleanly
			// instead of splicing onto these bytes.
			return os.Rename(partPath, filePath)
		default:
			_ = os.Remove(partPath)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// A 206 response is verified against the full object size (probeFullSize), so
	// the total after the append is expected; a 200 is verified against the
	// response's own Content-Length.
	switch resp.StatusCode {
	case http.StatusOK:
		// A 200 in response to a Range request means the server ignored the
		// range and is sending the whole object; the partial must not be
		// appended to (that would splice). resumeFrom is 0 here, so os.Create
		// truncates the probed validator's partial and the transfer starts from
		// zero, keeping that partial resumable if this attempt is interrupted.
		return e.copyToFileResume(partPath, filePath, resp.Body, resp.ContentLength, 0)
	case http.StatusPartialContent:
		if resumeFrom == 0 {
			// A 206 with no partial to append to cannot be trusted; restart.
			return e.downloadFull(ctx, httpClient, url, filePath, token, validator)
		}
		// ContentLength is the remaining bytes; total on disk is resumeFrom +
		// written, which must equal the full size the probe saw.
		expected := resumeFrom + resp.ContentLength
		if full := probeFullSize(validator, resp); full > 0 && expected != full {
			_ = os.Remove(partPath)
			return e.downloadFull(ctx, httpClient, url, filePath, token, validator)
		}
		return e.copyToFileResume(partPath, filePath, resp.Body, expected, resumeFrom)
	default:
		return fmt.Errorf("bad status: %s", resp.Status)
	}
}

// downloadFull performs a non-resuming GET of url and publishes the body at
// filePath via the partial keyed on partValidator. It is the from-zero path
// taken when there is nothing to resume or a resume was invalidated; the caller
// passes the probed validator so an interrupted restart stays resumable, or the
// empty validator when the probe failed and no key is known.
func (e *MetalExecutor) downloadFull(
	ctx context.Context,
	httpClient *http.Client,
	url, filePath, token, partValidator string,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}
	partPath := validatorPartialPath(filePath, partValidator)
	return e.copyToFileResume(partPath, filePath, resp.Body, resp.ContentLength, 0)
}

// probeValidator HEADs url on the given client and returns the raw validator
// ("CL<len>ET<etag>") plus the parsed size, mirroring the init-container probes so
// the same bytes produce the same partial key in-cluster and on the metal path.
func probeValidator(
	ctx context.Context,
	httpClient *http.Client,
	url, token string,
) (validator string, size int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", 0, fmt.Errorf("HEAD %s: %s", url, resp.Status)
	}
	etag := resp.Header.Get("ETag")
	sizeStr := resp.Header.Get("Content-Length")
	if sizeStr == "" {
		sizeStr = fmt.Sprintf("%d", resp.ContentLength)
	}
	size, _ = strconv.ParseInt(sizeStr, 10, 64)
	return "CL" + sizeStr + "ET" + etag, size, nil
}

// validatorSize parses the length out of a "CL<len>ET..." validator string, or -1
// when it is unparseable (an empty/absent probe result).
func validatorSize(validator string) int64 {
	if !strings.HasPrefix(validator, "CL") {
		return -1
	}
	rest := strings.TrimPrefix(validator, "CL")
	et := strings.Index(rest, "ET")
	if et < 0 {
		et = len(rest)
	}
	n, err := strconv.ParseInt(rest[:et], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// probeFullSize returns the total size the ranged response implies, used to
// verify the appended bytes complete the object. It prefers the size the probe
// already read (the validator) and falls back to the instance-length in the
// response's Content-Range header, returning 0 when neither is known (nothing to
// check against).
func probeFullSize(validator string, resp *http.Response) int64 {
	if n := validatorSize(validator); n > 0 {
		return n
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if slash := strings.LastIndexByte(cr, '/'); slash >= 0 {
			if total, err := strconv.ParseInt(cr[slash+1:], 10, 64); err == nil {
				return total
			}
		}
	}
	return 0
}

// validatorPartialPath names the partial that holds resume progress for a
// download, or the scratch file a from-zero transfer writes. The name embeds the
// first 12 hex of sha256(validator) so the bytes a transfer holds are always the
// bytes the current validator implies. A caller that has a validator always
// passes it, from-zero transfers included, so the partial a fresh attempt leaves
// behind is the one a later attempt looks for; only the probe-failure path, which
// has no validator, collapses to the fixed empty-validator name.
func validatorPartialPath(filePath, validator string) string {
	sum := sha256.Sum256([]byte(validator))
	key := hex.EncodeToString(sum[:])[:12]
	return filepath.Join(filepath.Dir(filePath), "."+filepath.Base(filePath)+"."+key+".partial")
}

// sweepStalePartials deletes every content-keyed partial for filePath in its
// directory except keep. A partial whose key does not match the current
// validator belongs to different content and cannot be resumed, so removing it
// here keeps such partials from accumulating across content changes; the resume
// key itself is always kept so an interrupted transfer retains its progress.
func sweepStalePartials(filePath, keep string) {
	dir := filepath.Dir(filePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := "." + filepath.Base(filePath) + "."
	for _, ent := range entries {
		name := ent.Name()
		if !ent.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".partial") {
			continue
		}
		if filepath.Join(dir, name) == keep {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// hfRedirectStripper returns a client that removes the Authorization header
// whenever a redirect changes the host, comparing host and port rather than
// registrable domain. Everything else matches http.DefaultClient, including
// the ten-redirect ceiling that CheckRedirect is expected to enforce.
func hfRedirectStripper() *http.Client {
	c := *http.DefaultClient
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}
	return &c
}

// copyToFileNoResume streams r into filePath atomically through the plain
// ".partial" sibling: it writes to filePath+".partial" first and renames into
// place only on success, so an interrupted transfer never leaves a truncated
// model that the stat check in ensureModel would treat as cached. When
// contentLength > 0 it verifies the byte count matches, so a mid-stream
// connection drop does not leave a truncated model. This is the non-resuming
// path (S3), byte-for-byte the behaviour it had before resume was added.
func (e *MetalExecutor) copyToFileNoResume(filePath string, r io.Reader, contentLength int64) error {
	return e.copyToFileResume(filePath+".partial", filePath, r, contentLength, 0)
}

// copyToFileResume streams r into partPath (opened for append at resumeFrom, or
// created/truncated at 0), then renames it onto filePath on success. It reports
// an error, without publishing, if the total size does not match contentLength
// (when known) so a truncated transfer is never mistaken for a complete one.
//
// The partial is deliberately kept on a mid-stream failure and only removed on a
// size mismatch or a rename failure: a resumable error leaves the partial in
// place so a later attempt resumes, while a size mismatch means the partial is
// untrustworthy.
func (e *MetalExecutor) copyToFileResume(
	partPath, filePath string,
	r io.Reader,
	contentLength, resumeFrom int64,
) error {
	var out *os.File
	var err error
	if resumeFrom > 0 {
		// The file is published (renamed) to the same path a full download
		// writes; 0600 matches the umask-collapsed result and gosec's default.
		out, err = os.OpenFile(partPath, os.O_WRONLY|os.O_APPEND, 0o600)
	} else {
		out, err = os.Create(partPath)
	}
	if err != nil {
		return err
	}
	written, cerr := io.Copy(out, r)
	if cerr != nil {
		_ = out.Close()
		return cerr
	}
	if err := out.Close(); err != nil {
		return err
	}

	total := resumeFrom + written
	if contentLength > 0 && total != contentLength {
		_ = os.Remove(partPath)
		return fmt.Errorf("download truncated: expected %d bytes, got %d", contentLength, total)
	}

	if err := os.Rename(partPath, filePath); err != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("failed to rename downloaded model: %w", err)
	}
	return nil
}

func (e *MetalExecutor) waitForHealthy(port int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	healthURL := fmt.Sprintf("http://localhost:%d/health", port)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for health check")
		case <-ticker.C:
			resp, err := http.Get(healthURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				_ = resp.Body.Close()
				return nil
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
		}
	}
}

// hasMatchingExtraArg reports whether extraArgs already carries argName in
// either the "--name" or "--name=value" form. Mirrors the controller helper so
// the agent path does not duplicate flags the user set explicitly.
func hasMatchingExtraArg(extraArgs []string, argName string) bool {
	arg := fmt.Sprintf("--%s", argName)
	inlineArg := fmt.Sprintf("--%s=", argName)
	for _, v := range extraArgs {
		if v == arg || strings.HasPrefix(v, inlineArg) {
			return true
		}
	}
	return false
}

// appendModeArgs wires the llama.cpp flags for embedding and rerank serving,
// mirroring the controller's runtime_llamacpp arg builder. A reranker needs
// both --reranking and --embedding; flags already in extraArgs win and are not
// duplicated. Chat (or empty) adds nothing.
//
// For embedding and rerank modes, --cache-ram 0 is appended because llama.cpp
// fills its host prompt cache up to --cache-ram (default 8 GiB) and never
// releases it, while the cache read path is gated to completion tasks.
// (#1406)
func appendModeArgs(args []string, mode string, extraArgs []string) []string {
	switch mode {
	case inferencev1alpha1.ServingModeRerank:
		if !hasMatchingExtraArg(extraArgs, "reranking") {
			args = append(args, "--reranking")
		}
		if !hasMatchingExtraArg(extraArgs, "embedding") {
			args = append(args, "--embedding")
		}
		if !hasMatchingExtraArg(extraArgs, "pooling") {
			args = append(args, "--pooling", "rank")
		}
		if !hasMatchingExtraArg(extraArgs, "cache-ram") {
			args = append(args, "--cache-ram", "0")
		}
	case inferencev1alpha1.ServingModeEmbedding:
		if !hasMatchingExtraArg(extraArgs, "embedding") {
			args = append(args, "--embedding")
		}
		if !hasMatchingExtraArg(extraArgs, "pooling") {
			args = append(args, "--pooling", "last")
		}
		if !hasMatchingExtraArg(extraArgs, "cache-ram") {
			args = append(args, "--cache-ram", "0")
		}
	}
	return args
}

// buildLlamaServerArgs constructs the command-line argument vector for the
// llama-server child process. It is split out from StartProcess so it can be
// unit tested without spawning a real process and so the Apple-Silicon-specific
// optimizations are inspectable in one place.
func buildLlamaServerArgs(modelPath string, port int, config ExecutorConfig) []string {
	gpuLayers := config.GPULayers
	if gpuLayers == 0 {
		gpuLayers = 99
	}

	args := []string{
		"--model", modelPath,
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", port),
		"--n-gpu-layers", fmt.Sprintf("%d", gpuLayers),
		"--ctx-size", fmt.Sprintf("%d", config.ContextSize),
	}

	// Prometheus metrics, unless the user already asked for it in ExtraArgs
	// (#1384). This mirrors the reranking/embedding/pooling guards below and
	// the two controller paths; it was the one operator-owned flag here that
	// was still emitted unconditionally.
	if !hasMatchingExtraArg(config.ExtraArgs, "metrics") {
		args = append(args, "--metrics")
	}

	// RoPE context extension (InferenceService.spec.ropeScaling). ExtraArgs
	// still come last, so a user override there wins over these.
	if config.RopeScalingType != "" {
		args = append(args, "--rope-scaling", config.RopeScalingType)
		if config.RopeScalingFactor != "" {
			args = append(args, "--rope-scale", config.RopeScalingFactor)
		}
		if config.RopeScalingOrigCtx > 0 {
			args = append(args, "--yarn-orig-ctx", fmt.Sprintf("%d", config.RopeScalingOrigCtx))
		}
	}

	if config.ParallelSlots > 1 {
		args = append(args, "--parallel", fmt.Sprintf("%d", config.ParallelSlots))
	}

	if config.FlashAttention {
		args = append(args, "--flash-attn", "on")
	}

	if config.Mlock {
		args = append(args, "--mlock")
	}

	if config.CacheTypeK != "" {
		args = append(args, "--cache-type-k", config.CacheTypeK)
	}
	if config.CacheTypeV != "" {
		args = append(args, "--cache-type-v", config.CacheTypeV)
	}

	if config.MoeCPUOffload {
		args = append(args, "--cpu-moe")
	}
	if config.MoeCPULayers > 0 {
		args = append(args, "--n-cpu-moe", fmt.Sprintf("%d", config.MoeCPULayers))
	}
	if config.NoKvOffload {
		args = append(args, "--no-kv-offload")
	}
	for _, override := range config.TensorOverrides {
		args = append(args, "--override-tensor", override)
	}
	for _, override := range config.MetadataOverrides {
		args = append(args, "--override-kv", override)
	}

	threads := config.Threads
	if threads == 0 {
		threads = detectPerfCoreCount()
	}
	if threads > 0 {
		args = append(args, "--threads", fmt.Sprintf("%d", threads))
	}

	batchSize := config.BatchSize
	if batchSize == 0 {
		batchSize = 2048
	}
	args = append(args, "--batch-size", fmt.Sprintf("%d", batchSize))

	if config.UBatchSize > 0 {
		args = append(args, "--ubatch-size", fmt.Sprintf("%d", config.UBatchSize))
	}

	if config.NoWarmup {
		args = append(args, "--no-warmup")
	}

	if config.ReasoningBudget > 0 {
		args = append(args, "--reasoning-budget", fmt.Sprintf("%d", config.ReasoningBudget))
		if config.ReasoningBudgetMessage != "" {
			args = append(args, "--reasoning-budget-message", config.ReasoningBudgetMessage)
		}
	}

	if config.Jinja {
		args = append(args, "--jinja")
	}

	args = appendModeArgs(args, config.Mode, config.ExtraArgs)

	// ExtraArgs comes last so user-provided overrides actually override.
	if len(config.ExtraArgs) > 0 {
		args = append(args, config.ExtraArgs...)
	}

	return args
}

// allocatePort asks the kernel for an unused TCP port by binding to
// "127.0.0.1:0" and immediately closing the listener. The returned port
// is guaranteed free at the moment of the call; there is a small TOCTOU
// window before llama-server binds on the same port. For the Metal
// executor that window is microseconds since we exec the child process
// synchronously, so a collision is vanishingly unlikely in practice.
func (e *MetalExecutor) allocatePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
