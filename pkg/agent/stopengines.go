package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// DefaultOllamaPort is the port the agent expects Ollama on when none is set.
const DefaultOllamaPort = 11434

// StopEnginesConfig configures RunStopEngines.
type StopEnginesConfig struct {
	StateDir string
	// Namespace is only used in log lines; the records carry their own.
	Namespace string
	// K8sClient withdraws each service's endpoint before its engine stops.
	// Nil skips the withdrawal.
	K8sClient client.Client
	// OllamaPort is the Ollama daemon's port; 0 means DefaultOllamaPort.
	OllamaPort int
	Logger     *zap.SugaredLogger
}

// RunStopEngines stops every engine recorded under <stateDir>/engines without
// a running agent: it withdraws the service's endpoint (best effort), stops the
// engine, and removes the record once the engine is confirmed stopped. The
// agent must already be stopped, or its health monitor can respawn an engine.
// The returned error joins every record it could not clear.
func RunStopEngines(ctx context.Context, cfg StopEnginesConfig) error {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	home, _ := os.UserHomeDir()
	stateDir, err := resolveStateDir(cfg.StateDir, home)
	if err != nil {
		return err
	}
	inspect := newProcessInspector()
	store := newEngineRecordStore(filepath.Join(stateDir, "engines"), "", inspect, nil)
	recs, listErrs := store.List()
	errs := listErrs
	var registry *ServiceRegistry
	if cfg.K8sClient != nil {
		registry = NewServiceRegistry(cfg.K8sClient, "", logger, "")
	}
	for _, rec := range recs {
		key := rec.Namespace + "/" + rec.Name
		if registry != nil {
			withdrawEndpoint(ctx, registry, rec, logger)
		}
		if err := stopRecordedEngine(ctx, cfg, rec, inspect); err != nil {
			logger.Warnw("could not stop engine; keeping its record", "key", key, "pid", rec.PID, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		if err := store.Remove(rec.Namespace, rec.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		logger.Infow("stopped engine", "key", key, "pid", rec.PID, "runtime", rec.Runtime)
	}
	return errors.Join(errs...)
}

// withdrawEndpoint flips the service's endpoint to not ready. It is best
// effort: a failure is logged and the engine is stopped regardless.
func withdrawEndpoint(ctx context.Context, registry *ServiceRegistry, rec engineRecord, logger *zap.SugaredLogger) {
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Namespace: rec.Namespace, Name: rec.Name}}
	if _, err := registry.WithdrawEndpointIfPresent(ctx, isvc); err != nil {
		logger.Warnw("could not withdraw the endpoint", "key", rec.Namespace+"/"+rec.Name, "error", err)
	}
}

// stopRecordedEngine stops one recorded engine and returns nil only when it is
// confirmed stopped (or was already gone), so the caller may drop the record.
func stopRecordedEngine(ctx context.Context, cfg StopEnginesConfig, rec engineRecord, inspect processInspector) error {
	switch {
	case rec.Runtime == runtimeOMLX:
		// The oMLX daemon is not adopted, so it is not signalled either.
		return nil
	case rec.PID <= 0:
		if rec.Runtime != runtimeOllama {
			return nil
		}
		port := cfg.OllamaPort
		if port == 0 {
			port = DefaultOllamaPort
		}
		return NewOllamaExecutor(port, cfg.Logger).UnloadModel(ctx, rec.ModelID)
	case rec.PGID != rec.PID:
		return fmt.Errorf("record has pgid %d for pid %d: not a group leader, left alone", rec.PGID, rec.PID)
	}
	// The start token is the identity proof: a dead PID or a PID another
	// program now holds means the engine is gone; only then is the
	// executable compared, so an unrelated live PID is never reported.
	if err := verifyEngineIdentity(rec.PID, rec.StartToken, inspect); err != nil {
		if errors.Is(err, errProcessAlreadyGone) {
			return nil
		}
		return err
	}
	exes, err := inspect.Executables(rec.PID)
	if err != nil {
		return fmt.Errorf("process %d: cannot read its executable, left alone: %w", rec.PID, err)
	}
	if !executableMatches(exes, rec.Executable) {
		return fmt.Errorf("process %d is not the recorded executable %q, left alone", rec.PID, rec.Executable)
	}
	if err := stopEngineGroup(rec.PID, rec.PGID, rec.StartToken, inspect); !engineStopped(err) {
		return err
	}
	return nil
}
