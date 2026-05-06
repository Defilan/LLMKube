/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tui

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/defilantech/llmkube/catalog"
	"github.com/defilantech/llmkube/pkg/cli/internal/specbuilder"
)

// defaultAccelerator returns the platform's expected accelerator for new
// deploys. Apple Silicon → metal, everything else → cpu (we don't probe
// for nvidia-smi here because the metal-agent + GPU operator already pick
// the right accelerator at admission time; a "cuda" default that fails on
// CPU-only hosts would be worse than this conservative fallback).
func defaultAccelerator() string {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "metal"
	}
	return "cpu"
}

// deployFormModel collects deploy options for a single Model + InferenceService
// pair and applies them on submit. Wraps huh.Form to inherit field validation,
// keyboard navigation (Tab / Shift-Tab / Enter), and themed rendering for free.
//
// The form is constructed from a "seed" Input (catalog defaults or local-only
// best guesses) so the user only edits fields they want to change.
type deployFormModel struct {
	form *huh.Form

	// Numeric fields back select/input widgets and are cast in buildInput().
	// contextSize is an int (huh.Select[int]) since we offer power-of-2
	// presets; parallelSlots/replicas stay strings to keep custom tuning open.
	name          string
	namespace     string
	source        string // editable Model.spec.source — see #405 for why
	accelerator   string
	runtime       string
	contextSize   int
	parallelSlots string
	replicas      string
	flashAttn     bool
	jinja         bool
	cacheTypeK    string
	cacheTypeV    string
	memoryFrac    string

	// passthrough fields the form doesn't ask about but Build() needs.
	modelFormat string
	cpu         string
	memory      string
	quant       string

	// terminal status
	width   int
	height  int
	applied bool
	err     error

	// On a successful Apply, deployedName/Namespace populate so the root
	// model can hand them off to the status view.
	deployedName      string
	deployedNamespace string
}

// deployedMsg is sent when the form successfully applies a Model + ISVC.
// The root model uses it to transition to viewStatus.
type deployedMsg struct {
	name      string
	namespace string
}

// formCancelMsg is sent when the user presses Esc to abandon the form.
// The root model returns to viewBrowser.
type formCancelMsg struct{}

// newDeployForm builds a form pre-filled from the seed. The seed comes from
// the browser's selected row: a catalog entry or a local-only model.
func newDeployForm(seed seedInput) deployFormModel {
	// Accelerator default: prefer the seed's hint (catalog entry or local
	// format detection), but fall back to the platform default. This makes
	// the form do the right thing on Apple Silicon out of the box.
	accel := seed.accelerator
	if accel == "" {
		accel = defaultAccelerator()
	}

	// Context size default: pick the closest preset >= seed.contextSize so
	// the dropdown's initial selection is meaningful. Catalog entries with
	// non-power-of-2 sizes get rounded up.
	ctxDefault := nearestContextPreset(seed.contextSize)

	// For "Detected on disk" rows the seed's modelSource is a host path that
	// the metal-agent on the Mac can read but the controller pod inside
	// kind cannot. Pre-resolve to a https HF URL when we recognize the
	// filename pattern; otherwise keep the host path and let the user fix
	// it in the form (the description explains why).
	source := suggestRemoteSource(seed.modelSource)

	m := deployFormModel{
		name:          seed.name,
		namespace:     "default",
		source:        source,
		accelerator:   accel,
		runtime:       seed.runtime,
		contextSize:   ctxDefault,
		parallelSlots: "1",
		replicas:      "1",
		flashAttn:     accel != "cpu", // ON for metal/cuda, OFF for cpu (no benefit)
		// Jinja defaults to ON because agentic-coding workflows (opencode,
		// aider, cline) all rely on tool-call chat templates, and that's
		// the dominant use case for a local LLMKube deploy. Users who only
		// want plain chat can flip it off in the form.
		jinja:       true,
		cacheTypeK:  "f16",
		cacheTypeV:  "f16",
		memoryFrac:  "",
		modelFormat: seed.modelFormat,
		cpu:         seed.cpu,
		memory:      seed.memory,
		quant:       seed.quant,
	}

	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title(fmt.Sprintf("Deploy: %s", seed.name)).
				Description(fmt.Sprintf("Source: %s\nFormat: %s\nQuant:  %s",
					trim(seed.modelSource, 60), seed.modelFormat, seed.quant)),

			huh.NewInput().Title("Name").Value(&m.name).
				Validate(nonEmpty("name")),
			huh.NewInput().Title("Namespace").Value(&m.namespace).
				Validate(nonEmpty("namespace")),
			huh.NewInput().
				Title("Model source").
				Description("HuggingFace URL (https://huggingface.co/.../file.gguf), HF repo ID (org/name), or pvc:// path. Avoid raw host paths (/Users/...) — the controller pod inside Kubernetes cannot read them. The metal-agent uses filepath.Base(source) to find the local file, so the filename in the URL must match what's on disk.").
				Value(&m.source).
				Validate(nonEmpty("model source")),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Accelerator").
				Description(acceleratorRecommendation()).
				Options(
					huh.NewOption("Apple Metal", "metal"),
					huh.NewOption("NVIDIA CUDA", "cuda"),
					huh.NewOption("CPU only", "cpu"),
				).Value(&m.accelerator),

			huh.NewSelect[string]().Title("Runtime").
				Description("llama.cpp is the safe default. Pick vllm-swift for Apple Silicon if you've installed the bridge dylib; vLLM/TGI/Ollama for in-cluster pods.").
				Options(
					huh.NewOption("llama.cpp (default, GGUF)", "llamacpp"),
					huh.NewOption("vLLM (in-cluster, NVIDIA)", "vllm"),
					huh.NewOption("vllm-swift (Apple Silicon native)", "vllm-swift"),
					huh.NewOption("TGI (HuggingFace)", "tgi"),
					huh.NewOption("Ollama", "ollama"),
				).Value(&m.runtime),
		),
		huh.NewGroup(
			huh.NewSelect[int]().Title("Context size").
				Description(contextSizeRecommendation()).
				Options(contextSizeOptions()...).
				Value(&m.contextSize),

			huh.NewInput().Title("Parallel slots").
				Description("Concurrent requests the runtime will batch. 1 for solo agentic coding (best per-request latency); 4-8 for multi-user serving.").
				Value(&m.parallelSlots).
				Validate(boundedInt("parallel slots", 1, 64)),

			huh.NewInput().Title("Replicas").
				Description("1 for dev (default). 2+ only if you need HA, which is rare on a single-host metal-agent.").
				Value(&m.replicas).
				Validate(boundedInt("replicas", 0, 10)),

			huh.NewConfirm().Title("Flash attention?").
				Description("Recommended ON for metal/CUDA (large speedup at long contexts). No benefit on CPU.").
				Value(&m.flashAttn),

			huh.NewConfirm().Title("Jinja templates (tool calling)?").
				Description("ON if you're using tool-calling models (qwen3, llama-3, etc.) with agents like opencode or aider. OFF for pure chat.").
				Value(&m.jinja),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("KV cache type (K)").
				Description("f16 is fastest and highest quality. q8_0 saves ~50% KV memory at <0.01 KL divergence; only worth it on 32-48GB hosts.").
				Options(kvOptions()...).Value(&m.cacheTypeK),

			huh.NewSelect[string]().Title("KV cache type (V)").
				Description("Match K unless you've benchmarked an asymmetric scheme on your hardware.").
				Options(kvOptions()...).Value(&m.cacheTypeV),

			huh.NewInput().
				Title("Memory fraction (metal only)").
				Description("Fraction of available unified memory the agent may use (0.0–1.0). Empty to skip. Default 0.9 is safe on 128 GB Macs.").
				Value(&m.memoryFrac).
				Validate(optionalFraction()),
		),
	)
	return m
}

func (m deployFormModel) Init() tea.Cmd {
	return m.form.Init()
}

func (m deployFormModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok && km.String() == "esc" {
		// Esc abandons the form and returns to the browser. Without this,
		// huh swallows Esc as "go back one field" which can leave the user
		// stuck on the first field.
		return m, func() tea.Msg { return formCancelMsg{} }
	}

	form, cmd := m.form.Update(msg)
	if f, ok := form.(*huh.Form); ok {
		m.form = f
	}

	if m.form.State == huh.StateCompleted && !m.applied {
		m.applied = true
		return m, m.applyCmd()
	}

	return m, cmd
}

func (m deployFormModel) View() string {
	if m.err != nil {
		return styleBox.Render(
			styleHeader.Render("Deploy failed") + "\n\n" +
				styleBadgePending.Render(m.err.Error()) + "\n\n" +
				styleHelp.Render("esc back to browser · q quit"),
		)
	}
	if m.applied {
		return styleBox.Render(
			styleHeader.Render("Applying...") + "\n\n" +
				styleDim.Render("Sending Model + InferenceService to the cluster."),
		)
	}
	return styleBox.Render(m.form.View())
}

// applyCmd builds specs from the form input, creates them on the cluster,
// and emits deployedMsg on success.
func (m *deployFormModel) applyCmd() tea.Cmd {
	return func() tea.Msg {
		k8s, err := newK8sClient()
		if err != nil {
			return deployErrMsg{err: fmt.Errorf("no cluster available: %w", err)}
		}

		input, err := m.buildInput()
		if err != nil {
			return deployErrMsg{err: err}
		}

		model, isvc := specbuilder.Build(input)

		ctx := context.Background()
		if err := k8s.Create(ctx, model); err != nil {
			return deployErrMsg{err: fmt.Errorf("create Model: %w", err)}
		}
		if err := k8s.Create(ctx, isvc); err != nil {
			// Best-effort cleanup of the dangling Model so the user can re-try
			// from the form without a name collision on the next attempt.
			_ = k8s.Delete(ctx, model)
			return deployErrMsg{err: fmt.Errorf("create InferenceService: %w", err)}
		}
		return deployedMsg{name: input.Name, namespace: input.Namespace}
	}
}

// deployErrMsg routes apply failures back into the form so the user can fix
// and retry without leaving the screen. Wired through Update by the caller.
type deployErrMsg struct {
	err error
}

// buildInput converts the form's typed fields into a specbuilder.Input.
// huh's Validate already gated numeric ranges, so we trust the strconv
// results here.
func (m deployFormModel) buildInput() (specbuilder.Input, error) {
	parallel, _ := strconv.Atoi(m.parallelSlots)
	replicas, _ := strconv.Atoi(m.replicas)

	in := specbuilder.Input{
		Name:           strings.TrimSpace(m.name),
		Namespace:      strings.TrimSpace(m.namespace),
		ModelSource:    strings.TrimSpace(m.source),
		ModelFormat:    m.modelFormat,
		Quantization:   m.quant,
		Accelerator:    m.accelerator,
		CPU:            m.cpu,
		Memory:         m.memory,
		Runtime:        m.runtime,
		Replicas:       int32(replicas),
		ContextSize:    int32(m.contextSize),
		ParallelSlots:  int32(parallel),
		FlashAttention: ptrBool(m.flashAttn),
		Jinja:          ptrBool(m.jinja),
		CacheTypeK:     m.cacheTypeK,
		CacheTypeV:     m.cacheTypeV,
	}

	if frac := strings.TrimSpace(m.memoryFrac); frac != "" {
		v, err := strconv.ParseFloat(frac, 64)
		if err != nil {
			return in, fmt.Errorf("memory fraction: %w", err)
		}
		in.MetalMemoryFraction = &v
	}

	return in, nil
}

// seedInput is the minimal data the browser passes in when the user picks
// a row. Catalog entries fill in more fields than local-only rows; the form
// uses sensible defaults for anything missing.
type seedInput struct {
	name        string
	modelSource string
	modelFormat string
	accelerator string
	runtime     string
	contextSize int
	flashAttn   bool
	cpu         string
	memory      string
	quant       string
}

// seedFromCatalog builds a seed from a catalog entry. Defaults align with
// what `llmkube deploy <id>` does today: CPU accelerator, llamacpp runtime,
// flash-attention off (the controller flips it on for metal).
func seedFromCatalog(id string, m *catalog.Model) seedInput {
	ctx := m.ContextSize
	if ctx == 0 {
		ctx = 8192
	}
	return seedInput{
		name:        id,
		modelSource: m.Source,
		modelFormat: "gguf",
		accelerator: "cpu",
		runtime:     "llamacpp",
		contextSize: ctx,
		flashAttn:   false,
		cpu:         orDefault(m.Resources.CPU, "500m"),
		memory:      orDefault(m.Resources.Memory, "1Gi"),
		quant:       m.Quantization,
	}
}

// seedFromLocal builds a seed from a local-disk find. Fewer hints available
// than from a catalog entry; we default the accelerator to the platform's
// best fit (metal on Apple Silicon, cpu elsewhere). MLX-format finds force
// metal because no other accelerator can read MLX weights. Context size
// defaults are RAM-aware so the form preselects a sensible value: bumping
// from 8K to 64K on a 128 GB Mac saves the user from accepting a default
// that runs them out of context in 5 minutes.
func seedFromLocal(l *LocalModel) seedInput {
	accel := defaultAccelerator()
	if l.Format == "mlx" {
		accel = "metal"
	}
	return seedInput{
		name:        sanitizeName(l.DisplayName),
		modelSource: l.Path,
		modelFormat: l.Format,
		accelerator: accel,
		runtime:     "llamacpp",
		contextSize: defaultContextForHost(),
		flashAttn:   accel == "metal",
		cpu:         "500m",
		memory:      "2Gi",
		quant:       l.Quant,
	}
}

// defaultContextForHost picks an initial context size based on host memory.
// Mirrors the bands described in contextSizeRecommendation() so the form's
// description and its preselected value tell the same story. Always returns
// a value that's already in contextSizePresets.
func defaultContextForHost() int {
	ramGB := hostMemoryGB()
	switch {
	case ramGB >= 128:
		return 65536 // 64K — agentic-coding sweet spot on 128 GB
	case ramGB >= 64:
		return 32768 // 32K — comfortable on 64 GB
	case ramGB >= 32:
		return 16384 // 16K
	default:
		return 8192 // 8K — safe everywhere, including unknown hosts
	}
}

// suggestRemoteSource converts well-known local-file paths to the equivalent
// HuggingFace URL when the filename pattern matches a recognizable upstream
// (bartowski's GGUFs, unsloth's GGUFs, mlx-community's MLX repos). Leaves
// other paths untouched so the user can edit them in the form.
//
// Background: the Model controller running inside the cluster can't read host
// paths like /Users/<you>/llmkube-models/foo.gguf, but the metal-agent on the
// host can. The metal-agent's executor uses filepath.Base(source) to compute
// the on-disk path, which means a remote URL with the same filename resolves
// to the same local file. Substituting the URL here means the controller
// stops hot-spinning (#405) without forcing a re-download.
func suggestRemoteSource(localPath string) string {
	if localPath == "" {
		return localPath
	}
	// Catalog and HF source URLs already start with https://. Pass through.
	if strings.HasPrefix(localPath, "https://") || strings.HasPrefix(localPath, "http://") {
		return localPath
	}
	if strings.HasPrefix(localPath, "pvc://") || strings.HasPrefix(localPath, "hf://") {
		return localPath
	}

	base := strings.ToLower(filenameBase(localPath))

	// Pattern: a Qwen 3.x A3B GGUF from unsloth's mirror. Most-common case
	// for users who downloaded via `huggingface-cli` against unsloth/* repos.
	// We match the family rather than exact filenames so future quant variants
	// (Q4_K_M, IQ3_S etc.) all resolve.
	if strings.Contains(base, "qwen3") && strings.Contains(base, "a3b") && strings.HasSuffix(base, ".gguf") {
		return fmt.Sprintf("https://huggingface.co/unsloth/%s/resolve/main/%s",
			"Qwen3.6-35B-A3B-GGUF", filenameBase(localPath))
	}

	// Bartowski-style: <Base>-GGUF/<Base>-<Quant>.gguf — the convention the
	// catalog uses today. Catalog entries already arrive with the full URL
	// so this branch only fires for off-catalog bartowski downloads.
	if strings.HasSuffix(base, ".gguf") && strings.Contains(base, "instruct") {
		// We don't know the upstream org without metadata; leave the path
		// alone so the user can edit it. Better to flag than to guess wrong.
		return localPath
	}

	return localPath
}

// filenameBase is filepath.Base inlined to avoid pulling path/filepath into
// this file's imports just for one call. The deploy form doesn't otherwise
// need it.
func filenameBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// sanitizeName lower-cases and replaces underscores/dots so the ID is a
// valid Kubernetes resource name. The user can override in the form.
func sanitizeName(s string) string {
	r := strings.ToLower(s)
	r = strings.ReplaceAll(r, "_", "-")
	r = strings.ReplaceAll(r, ".", "-")
	return r
}

// --- helpers ---

func nonEmpty(label string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", label)
		}
		return nil
	}
}

func positiveInt(label string) func(string) error {
	return func(s string) error {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("%s must be a number", label)
		}
		if v <= 0 {
			return fmt.Errorf("%s must be > 0", label)
		}
		return nil
	}
}

func boundedInt(label string, lo, hi int) func(string) error {
	return func(s string) error {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("%s must be a number", label)
		}
		if v < lo || v > hi {
			return fmt.Errorf("%s must be in [%d, %d]", label, lo, hi)
		}
		return nil
	}
}

func optionalFraction() func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("must be a number between 0.0 and 1.0")
		}
		if v <= 0 || v > 1.0 {
			return fmt.Errorf("must be in (0.0, 1.0]")
		}
		return nil
	}
}

func kvOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("f16 (default)", "f16"),
		huh.NewOption("q8_0", "q8_0"),
		huh.NewOption("q5_0", "q5_0"),
		huh.NewOption("q4_0", "q4_0"),
	}
}

// contextSizePresets are the most-common power-of-2 context sizes. We use
// these as the dropdown options on the form. Anything in between (e.g. the
// 96K some catalog entries declare) gets rounded up to the next preset by
// nearestContextPreset.
var contextSizePresets = []int{
	4 * 1024,        // 4K — debug / quick prompts
	8 * 1024,        // 8K — typical chat default
	16 * 1024,       // 16K
	32 * 1024,       // 32K
	64 * 1024,       // 64K — strong agentic-coding default
	128 * 1024,      // 128K
	256 * 1024,      // 256K — Qwen3.6-A3B max
	512 * 1024,      // 512K
	1024 * 1024,     // 1M
	2 * 1024 * 1024, // 2M
}

// contextSizeOptions builds the huh.Select options for context size, rendering
// the integer values as "8K", "256K", "1M" etc. for readability.
func contextSizeOptions() []huh.Option[int] {
	out := make([]huh.Option[int], 0, len(contextSizePresets))
	for _, n := range contextSizePresets {
		out = append(out, huh.NewOption(formatTokens(n), n))
	}
	return out
}

// formatTokens renders a token count as 4K / 64K / 1M / 2M for the dropdown
// labels. Always uses the closest power-of-2 unit.
func formatTokens(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%dM tokens", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dK tokens", n/1024)
	default:
		return fmt.Sprintf("%d tokens", n)
	}
}

// nearestContextPreset returns the smallest preset >= n, or the largest preset
// when n exceeds 2M. Used to pick the dropdown's initial selection from a
// catalog entry's declared context size.
func nearestContextPreset(n int) int {
	if n <= 0 {
		return 8 * 1024
	}
	for _, p := range contextSizePresets {
		if p >= n {
			return p
		}
	}
	return contextSizePresets[len(contextSizePresets)-1]
}

// contextSizeRecommendation returns a description string with sizing guidance
// based on host RAM. Best-effort: when host memory can't be read, falls back
// to a generic recommendation listing the common-case bands.
func contextSizeRecommendation() string {
	ramGB := hostMemoryGB()
	switch {
	case ramGB >= 128:
		return fmt.Sprintf("Host has ~%d GB unified memory. 256K fits comfortably for 30-35B models; 128K+ is the agentic-coding sweet spot.", ramGB)
	case ramGB >= 64:
		return fmt.Sprintf("Host has ~%d GB. 64K-128K is the sweet spot; 256K only with KV quantization.", ramGB)
	case ramGB >= 32:
		return fmt.Sprintf("Host has ~%d GB. 16K-32K is comfortable; 64K only with KV quantization (q8_0).", ramGB)
	case ramGB > 0:
		return fmt.Sprintf("Host has ~%d GB. 8K-16K recommended; larger contexts will swap.", ramGB)
	default:
		return "Pick the smallest context that fits your workload. 8K-16K for chat, 32K-64K for agentic coding, 128K+ for long-document tasks."
	}
}

// acceleratorRecommendation returns guidance hinted at the user's platform.
func acceleratorRecommendation() string {
	if defaultAccelerator() == "metal" {
		return "On Apple Silicon, Metal is the right pick. CPU and CUDA shown for cross-platform deploys."
	}
	return "Pick CUDA for NVIDIA GPU pods, Metal for Apple Silicon (via metal-agent), CPU for small/portable models."
}

func ptrBool(b bool) *bool { return &b }

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
