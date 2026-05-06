/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tui

import (
	"fmt"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// newK8sClient returns a controller-runtime client for the user's current
// kubeconfig context with the LLMKube CRDs registered. Returns a friendly
// error when no kubeconfig is reachable so the TUI can render a "no cluster"
// banner instead of crashing.
func newK8sClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("no kubeconfig found: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to register llmkube scheme: %w", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to construct k8s client: %w", err)
	}
	return c, nil
}
