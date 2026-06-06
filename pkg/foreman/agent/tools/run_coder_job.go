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

package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
)

// coderJobTemplate is the YAML template run_coder_job renders for each
// run. Lives in coder_job_template.yaml; embedded so the binary ships
// without an external configmap dependency, exactly like the gate Job.
//
//go:embed coder_job_template.yaml
var coderJobTemplate string

// Coder-Job verdict strings. Unlike the gate's GATE-* set, the coder Job
// reproduces the RunTask verdict vocabulary (GO / NO-GO / INCOMPLETE)
// plus a synthetic ERROR for run-failures the run-task body never reached
// a verdict for (image-pull, OOM, deadline, apiserver poll lag). These
// match foremanv1alpha1.AgenticTaskVerdict string values so the executor
// can pass them straight through.
const (
	coderVerdictGo         = "GO"
	coderVerdictNoGo       = "NO-GO"
	coderVerdictIncomplete = "INCOMPLETE"
	coderVerdictError      = "ERROR"
)

// RunCoderJobConfig is the static configuration the foreman-agent hands
// to the submitter. Per-run values (task name/namespace) come through
// RunCoderJobArgs. Defaults mirror the gate Job where they overlap
// (resources, TTL, poll cadence) and the Agent's ExecutionSpec where the
// installer overrides them (image, SA, deadline, resources).
type RunCoderJobConfig struct {
	// Namespace is where the coder Job is submitted. Defaults to
	// "foreman-system".
	Namespace string

	// Image is the per-task container image (foreman-agent binary plus
	// the project toolchain). Comes from Agent.spec.execution.image.
	// Defaults to "ghcr.io/defilantech/foreman-agent:latest" so the
	// zero-value config is constructable in tests.
	Image string

	// ServiceAccountName runs the Job pod under a least-privilege SA.
	// Empty omits the field (pod runs under the namespace default SA).
	ServiceAccountName string

	// ActiveDeadlineSeconds bounds wall-clock per run. Default 3600
	// (60 min) matches ExecutionSpec's CRD default; coder runs are
	// longer than the gate's 30 min.
	ActiveDeadlineSeconds int64

	// TTLSecondsAfterFinished bounds how long the Job + its Pod linger
	// after completion for log retrieval. Default 86400 (24 h), same as
	// the gate.
	TTLSecondsAfterFinished int32

	// Resource sizing. Defaults match the gate template (2/4 CPU,
	// 4Gi/8Gi memory).
	CPURequest string
	CPULimit   string
	MemRequest string
	MemLimit   string

	// GitCredentialsSecret is the Secret name mounted at /secrets/git
	// for the clone + push. RBAC + provisioning is a later task; this is
	// the conventional name the template references. Defaults to
	// "foreman-git-credentials".
	GitCredentialsSecret string

	// ModelAuthSecret, when non-empty, mounts a Secret at /secrets/model
	// for remote model endpoint auth. Empty omits the mount entirely.
	ModelAuthSecret string

	// PollInterval is how often Run polls Job.Status while waiting for a
	// terminal phase. Default 5s; tests inject milliseconds.
	PollInterval time.Duration

	// PollTimeout caps Run's wall-clock wait for a terminal Job.Status.
	// Defaults to twice ActiveDeadlineSeconds so the Job's own deadline
	// always fires first; hitting this surfaces as an ERROR verdict
	// (apiserver lag, not a model NO-GO).
	PollTimeout time.Duration

	// LogTailFn fetches the last MaxLogTailBytes of the pod log. Same
	// seam as the gate's: the controller-runtime fake client does not
	// support pod-log subresource reads, so production wires a real
	// kubernetes.Interface here, tests stub a static string. May be nil;
	// an empty LogTail then surfaces in the result.
	LogTailFn func(ctx context.Context, namespace, jobName string) string

	// NameFn lets tests pin Job names so polling can resolve them. Default
	// produces "foreman-coder-<task-name>-<unix-ms>".
	NameFn func(taskName string) string
}

// RunCoderJobArgs are the per-run inputs: which AgenticTask the Job runs.
type RunCoderJobArgs struct {
	// TaskName is the AgenticTask name passed to run-task --task.
	TaskName string

	// TaskNamespace is the AgenticTask namespace passed to
	// run-task --namespace. Defaults to "default" when empty.
	TaskNamespace string
}

// CoderJobResult is the parsed outcome of a coder Job run. It maps the
// run-task body's RunTaskResult (read from the pod log tail) plus the
// Job-level outcome onto a verdict the executor can fold into a *Result.
type CoderJobResult struct {
	// Verdict is GO / NO-GO / INCOMPLETE / ERROR.
	Verdict string

	// Summary is the one-line "what happened".
	Summary string

	// Branch is the branch the run targeted (pushed on GO).
	Branch string

	// CommitSHA is the head commit pushed on a GO verdict.
	CommitSHA string

	// CommitMessage is the model's commit message on a GO verdict.
	CommitMessage string

	// FailureReason is a short machine-ish reason set only on an ERROR
	// verdict (job-failed, poll-timeout, create-failed, render-failed).
	FailureReason string

	// LogTail is the captured pod log tail, for operator triage.
	LogTail string

	// JobName / Namespace identify the submitted Job.
	JobName   string
	Namespace string
}

// RunCoderJob submits a per-task coder Job (Agent.spec.execution.mode=Job),
// polls for terminal Job status, fetches the pod log tail, and parses the
// RunTask result + sentinel out of it. It is the sibling of
// RunGateJobTool: same render -> create -> poll -> log-tail -> map
// structure, but the Job runs `foreman-agent run-task` rather than the
// gate's `make <checks>`.
//
// RunCoderJob is NOT an LLM tool (it has no Schema / Execute on the
// agent.Tool interface): the executor calls Run directly when an Agent's
// ExecutionSpec selects Job mode. The seam that lets the agent package
// reach this code without an import cycle (tools imports agent, not the
// reverse) is agent.CoderJobSubmitter; cmd/foreman-agent wires a closure
// over Run into the executor.
type RunCoderJob struct {
	// Client is the controller-runtime client used to Create + Get the
	// Job. Required.
	Client client.Client

	// Cfg is the static configuration; defaults fill in via
	// applyCoderConfigDefaults at Run time.
	Cfg RunCoderJobConfig
}

// Submit implements agent.CoderJobSubmitter. It folds the per-request
// fields the executor supplies (task identity + the Agent's ExecutionSpec
// overrides for image / SA / deadline) onto a copy of the static config,
// runs the Job, and maps the parsed tools.CoderJobResult onto the
// agent.CoderJobResult the executor consumes. Per-request overrides win
// over the static Cfg defaults; an empty override field falls through to
// the configured default.
func (r *RunCoderJob) Submit(ctx context.Context, req agent.CoderJobRequest) (agent.CoderJobResult, error) {
	sub := &RunCoderJob{Client: r.Client, Cfg: r.Cfg}
	if req.Image != "" {
		sub.Cfg.Image = req.Image
	}
	if req.ServiceAccountName != "" {
		sub.Cfg.ServiceAccountName = req.ServiceAccountName
	}
	if req.ActiveDeadlineSeconds != nil {
		sub.Cfg.ActiveDeadlineSeconds = *req.ActiveDeadlineSeconds
	}

	res, err := sub.Run(ctx, RunCoderJobArgs{
		TaskName:      req.TaskName,
		TaskNamespace: req.TaskNamespace,
	})
	if err != nil {
		return agent.CoderJobResult{}, err
	}
	return agent.CoderJobResult{
		Verdict:       res.Verdict,
		Summary:       res.Summary,
		Branch:        res.Branch,
		CommitSHA:     res.CommitSHA,
		CommitMessage: res.CommitMessage,
		FailureReason: res.FailureReason,
		LogTail:       res.LogTail,
		JobName:       res.JobName,
	}, nil
}

// Run renders the coder Job, submits it, polls for terminal status,
// fetches the log tail, and returns the parsed CoderJobResult. It never
// returns a Go error for a data-shaped outcome (NO-GO, run-failure): those
// come back as a populated result with the appropriate verdict. A Go error
// is reserved for caller-misuse (nil Client, empty TaskName).
func (r *RunCoderJob) Run(ctx context.Context, args RunCoderJobArgs) (CoderJobResult, error) {
	if r.Client == nil {
		return CoderJobResult{}, errors.New("run_coder_job: Client is required")
	}
	if args.TaskName == "" {
		return CoderJobResult{}, errors.New("run_coder_job: task name is required")
	}
	if args.TaskNamespace == "" {
		args.TaskNamespace = "default"
	}

	cfg := applyCoderConfigDefaults(r.Cfg)
	jobName := cfg.NameFn(args.TaskName)

	rendered, err := renderCoderJob(coderRendererInput{
		Name:                    jobName,
		Namespace:               cfg.Namespace,
		Image:                   cfg.Image,
		TaskName:                args.TaskName,
		TaskNamespace:           args.TaskNamespace,
		ServiceAccountName:      cfg.ServiceAccountName,
		ActiveDeadlineSeconds:   cfg.ActiveDeadlineSeconds,
		TTLSecondsAfterFinished: cfg.TTLSecondsAfterFinished,
		CPURequest:              cfg.CPURequest,
		CPULimit:                cfg.CPULimit,
		MemRequest:              cfg.MemRequest,
		MemLimit:                cfg.MemLimit,
		GitCredentialsSecret:    cfg.GitCredentialsSecret,
		ModelAuthSecret:         cfg.ModelAuthSecret,
	})
	if err != nil {
		return r.errorResult(jobName, cfg.Namespace, "render: "+err.Error(), ""), nil
	}

	if err := r.Client.Create(ctx, rendered); err != nil {
		return r.errorResult(jobName, cfg.Namespace, "create job: "+err.Error(), ""), nil
	}

	jobVerdict, jobReason := r.pollForTerminal(ctx, cfg, jobName)

	logTail := ""
	if cfg.LogTailFn != nil {
		logTail = cfg.LogTailFn(ctx, cfg.Namespace, jobName)
		if len(logTail) > MaxLogTailBytes {
			logTail = logTail[len(logTail)-MaxLogTailBytes:]
		}
	}

	res := CoderJobResult{
		JobName:   jobName,
		Namespace: cfg.Namespace,
		LogTail:   logTail,
	}

	if jobVerdict == coderVerdictError {
		// The Job itself failed (image-pull / OOM / deadline / poll lag):
		// run-task never reached a verdict. Surface ERROR with the reason
		// and whatever log we managed to capture.
		res.Verdict = coderVerdictError
		res.FailureReason = jobReason
		res.Summary = "coder Job failed before producing a verdict: " + jobReason
		return res, nil
	}

	// Job completed (Succeeded). Parse the RunTask result out of the log.
	// run-task always exits 0 on a data-shaped outcome (GO/NO-GO/
	// INCOMPLETE) and exits non-zero only on a system error -- which the
	// jobVerdict==ERROR branch above already handled. So a Succeeded Job
	// carries a parseable RunTaskResult line.
	parsed := parseRunTaskLog(logTail)
	res.Verdict = parsed.Verdict
	res.Summary = parsed.Summary
	res.Branch = parsed.Branch
	res.CommitSHA = parsed.CommitSHA
	res.CommitMessage = parsed.CommitMessage
	if res.Verdict == "" {
		// Succeeded Job but no recognizable result line: treat as a
		// run-failure rather than silently dropping it.
		res.Verdict = coderVerdictError
		res.FailureReason = "no FOREMAN-RESULT line in pod log"
		res.Summary = "coder Job completed but emitted no parseable result"
	}
	if res.Verdict == coderVerdictError && res.FailureReason == "" {
		res.FailureReason = "run-task reported ERROR"
	}
	return res, nil
}

// pollForTerminal blocks until Job.Status reports a terminal phase or
// PollTimeout elapses. Returns ("", "") on a clean Succeeded; returns
// (coderVerdictError, reason) on Failure / NotFound / timeout / context
// cancellation. A Succeeded Job is signalled by an empty verdict so the
// caller proceeds to parse the log.
func (r *RunCoderJob) pollForTerminal(
	ctx context.Context, cfg RunCoderJobConfig, jobName string,
) (string, string) {
	deadline := time.Now().Add(cfg.PollTimeout)
	key := types.NamespacedName{Namespace: cfg.Namespace, Name: jobName}

	for {
		var job batchv1.Job
		if err := r.Client.Get(ctx, key, &job); err != nil {
			if apierrors.IsNotFound(err) {
				return coderVerdictError, "job disappeared before reaching a terminal phase"
			}
			return coderVerdictError, "apiserver poll failed: " + err.Error()
		}

		switch {
		case job.Status.Succeeded >= 1:
			return "", ""
		case job.Status.Failed >= 1:
			return coderVerdictError, "Job failed (image-pull, OOM, or active-deadline exceeded)"
		}

		if time.Now().After(deadline) {
			return coderVerdictError,
				fmt.Sprintf("Job did not reach a terminal phase within %s", cfg.PollTimeout)
		}

		select {
		case <-ctx.Done():
			return coderVerdictError, "context cancelled while polling Job: " + ctx.Err().Error()
		case <-time.After(cfg.PollInterval):
		}
	}
}

// errorResult builds a CoderJobResult for a failure that occurred before
// (or instead of) a clean Job completion (render error, create error).
func (r *RunCoderJob) errorResult(jobName, namespace, reason, logTail string) CoderJobResult {
	return CoderJobResult{
		Verdict:       coderVerdictError,
		Summary:       "coder Job did not run: " + reason,
		FailureReason: reason,
		LogTail:       logTail,
		JobName:       jobName,
		Namespace:     namespace,
	}
}

// --- result parsing -------------------------------------------------------

// parseRunTaskLog scans a pod log tail for the RunTask result line
// (agent.RunTaskResultPrefix + JSON) and, failing that, for the sentinel
// tokens. The structured JSON line is authoritative; the sentinel scan is
// the fallback for a truncated log that lost the JSON line but kept the
// shorter sentinel.
func parseRunTaskLog(logTail string) agent.RunTaskResult {
	var out agent.RunTaskResult
	for _, line := range strings.Split(logTail, "\n") {
		if idx := strings.Index(line, agent.RunTaskResultPrefix); idx >= 0 {
			payload := line[idx+len(agent.RunTaskResultPrefix):]
			var rt agent.RunTaskResult
			if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &rt); err == nil {
				return rt
			}
		}
	}
	// No parseable JSON line: fall back to sentinel scan.
	switch {
	case strings.Contains(logTail, agent.RunTaskSentinelGo):
		out.Verdict = coderVerdictGo
	case strings.Contains(logTail, agent.RunTaskSentinelNoGo):
		out.Verdict = coderVerdictNoGo
	case strings.Contains(logTail, agent.RunTaskSentinelIncomplete):
		out.Verdict = coderVerdictIncomplete
	case strings.Contains(logTail, agent.RunTaskSentinelError):
		out.Verdict = coderVerdictError
	}
	return out
}

// --- rendering ------------------------------------------------------------

// coderRendererInput is the struct text/template binds against for the
// coder Job template. Kept separate from the public Config + args structs
// so the template stays stable across signature changes upstream.
type coderRendererInput struct {
	Name                    string
	Namespace               string
	Image                   string
	TaskName                string
	TaskNamespace           string
	ServiceAccountName      string
	ActiveDeadlineSeconds   int64
	TTLSecondsAfterFinished int32
	CPURequest              string
	CPULimit                string
	MemRequest              string
	MemLimit                string
	GitCredentialsSecret    string
	ModelAuthSecret         string
}

func renderCoderJob(in coderRendererInput) (*batchv1.Job, error) {
	if in.TaskNamespace == "" {
		in.TaskNamespace = "default"
	}
	if in.TaskName == "" {
		in.TaskName = "unknown"
	}
	tmpl, err := template.New("coder-job").Parse(coderJobTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, in); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	var job batchv1.Job
	if err := yaml.Unmarshal(buf.Bytes(), &job); err != nil {
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}
	return &job, nil
}

// applyCoderConfigDefaults fills in every empty field with the documented
// default. Kept separate from the struct definition so the zero-value
// RunCoderJobConfig stays trivially constructable in tests.
func applyCoderConfigDefaults(c RunCoderJobConfig) RunCoderJobConfig {
	if c.Namespace == "" {
		c.Namespace = "foreman-system"
	}
	if c.Image == "" {
		c.Image = "ghcr.io/defilantech/foreman-agent:latest"
	}
	if c.ActiveDeadlineSeconds == 0 {
		c.ActiveDeadlineSeconds = 3600
	}
	if c.TTLSecondsAfterFinished == 0 {
		c.TTLSecondsAfterFinished = 86400
	}
	if c.CPURequest == "" {
		c.CPURequest = "2"
	}
	if c.CPULimit == "" {
		c.CPULimit = "4"
	}
	if c.MemRequest == "" {
		c.MemRequest = "4Gi"
	}
	if c.MemLimit == "" {
		c.MemLimit = "8Gi"
	}
	if c.GitCredentialsSecret == "" {
		c.GitCredentialsSecret = "foreman-git-credentials"
	}
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.PollTimeout == 0 {
		c.PollTimeout = 2 * time.Duration(c.ActiveDeadlineSeconds) * time.Second
	}
	if c.NameFn == nil {
		c.NameFn = func(taskName string) string {
			name := fmt.Sprintf("foreman-coder-%s-%d", sanitizeName(taskName), time.Now().UnixMilli())
			if len(name) > 63 {
				name = name[:63]
			}
			return name
		}
	}
	return c
}
