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

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/defilantech/llmkube/internal/relay"
)

// relayReadyTimeout bounds how long a /readyz probe waits on the ingress
// before reporting not-ready. It must stay well under kubelet's default
// probe timeout so a slow ingress reports 503 instead of the probe itself
// timing out with no useful status.
const relayReadyTimeout = 2 * time.Second

// relayConfigFromFlags builds a relay.Config from flag values, falling back
// to the controller-set environment variables (RELAY_TARGET, RELAY_UPSTREAM,
// RELAY_SPKI_PIN, RELAY_TOKEN_FILE) when a flag is empty.
//
// The controller wires the relay pod through env rather than flags so that
// rotating the pinned SPKI or the token path only touches the pod spec's
// env, which rolls the pod; the flags exist for local testing against a
// metal-agent ingress without a controller in the loop.
func relayConfigFromFlags(target, upstream, spkiPin, tokenFile string) relay.Config {
	return relay.Config{
		Target:    firstNonEmpty(target, os.Getenv("RELAY_TARGET")),
		Upstream:  firstNonEmpty(upstream, os.Getenv("RELAY_UPSTREAM")),
		SPKIPin:   firstNonEmpty(spkiPin, os.Getenv("RELAY_SPKI_PIN")),
		TokenFile: firstNonEmpty(tokenFile, os.Getenv("RELAY_TOKEN_FILE")),
	}
}

func firstNonEmpty(flagVal, envVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return envVal
}

// newRelayMux builds the two muxes a relay pod serves.
//
// data forwards everything to the pinned ingress via r; it carries the
// inference traffic and is the only thing bound to --listen. metrics carries
// /livez and /readyz, which the controller probes on --metrics-bind-address
// alongside /metrics (mounted by the caller, not here, so this function has
// no dependency on the metrics registry and is easy to exercise directly).
//
// Split out from runRelay so a test can drive both muxes in-process against
// a real *relay.Relay without binding any sockets.
func newRelayMux(r *relay.Relay) (data, metrics *http.ServeMux) {
	data = http.NewServeMux()
	data.Handle("/", r)

	metrics = http.NewServeMux()
	metrics.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	metrics.HandleFunc("GET /readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), relayReadyTimeout)
		defer cancel()
		if err := r.Ready(ctx); err != nil {
			http.Error(w, "relay not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return data, metrics
}

// runRelay serves relay mode end to end: it never loads the router config
// and never constructs a Kubernetes client, so a relay pod needs no RBAC and
// no ConfigMap mount, only the env the controller sets and the projected
// token Secret. It binds both listeners, serves until SIGINT/SIGTERM, drains
// within shutdownTimeout, and returns the process exit code.
func runRelay(logger *slog.Logger, listen, metricsListen string, shutdownTimeout time.Duration, cfg relay.Config) int {
	// Relay mode has no data-plane health surface of its own: the readyz and
	// livez probes the controller relies on to gate traffic live only on the
	// metrics listener. Disabling it (the router path's supported, silent
	// "run without metrics" mode) would leave the pod with no way to report
	// readiness at all, so it is refused here instead of starting unprobeable.
	if metricsDisabled(metricsListen) {
		logger.Error("relay mode requires --metrics-bind-address: the readyz/livez probes are served there",
			"metrics-bind-address", metricsListen)
		return 1
	}
	if err := listenConflict(listen, metricsListen); err != nil {
		logger.Error("invalid listener configuration", "error", err)
		return 1
	}

	r, err := relay.New(cfg, logger)
	if err != nil {
		logger.Error("construct relay", "error", err, "target", cfg.Target)
		return 1
	}

	dataMux, metricsMux := newRelayMux(r)
	metricsMux.Handle("GET /metrics", newMetricsHandler())

	dataSrv := &http.Server{
		Addr:              listen,
		Handler:           dataMux,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: streaming chat completions can be long-lived,
		// matching the router path in main().
	}
	metricsSrv := &http.Server{
		Addr:              metricsListen,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Bind synchronously, before serving anything, so a bind failure on
	// either listener is one clear startup error rather than a goroutine
	// that fails after the process has already logged "listening".
	dataLn, err := net.Listen("tcp", listen)
	if err != nil {
		logger.Error("relay data listener failed to bind", "address", listen, "error", err)
		return 1
	}
	metricsLn, err := net.Listen("tcp", metricsListen)
	if err != nil {
		logger.Error("relay metrics listener failed to bind", "address", metricsListen, "error", err)
		_ = dataLn.Close()
		return 1
	}

	serverErr := make(chan error, 2)
	go func() {
		logger.Info("relay listening", "address", dataLn.Addr().String(), "target", cfg.Target, "upstream", cfg.Upstream)
		if err := dataSrv.Serve(dataLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("relay data listener: %w", err)
		}
	}()
	go func() {
		logger.Info("relay metrics/probes listening", "address", metricsLn.Addr().String())
		if err := metricsSrv.Serve(metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("relay metrics listener: %w", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
		logger.Info("shutdown signal received; draining in-flight requests")
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = metricsSrv.Shutdown(ctx)
		if err := dataSrv.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			return 1
		}
		logger.Info("relay stopped cleanly")
		return 0
	case err := <-serverErr:
		logger.Error("relay server failed", "error", err)
		return 1
	}
}
