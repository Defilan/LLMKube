/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AuditLabel marks an audit-record ConfigMap; AuditTaskLabel carries the
	// task name for filtered discovery (`-l foreman.llmkube.dev/audit=true`).
	AuditLabel     = "foreman.llmkube.dev/audit"
	AuditTaskLabel = "foreman.llmkube.dev/audit-task"

	// auditDataKey is the ConfigMap data key holding the JSON Record.
	auditDataKey = "audit.json"

	auditNamePrefix = "foreman-audit-"
)

// AuditConfigMapName returns the ConfigMap name for a task's audit record.
func AuditConfigMapName(taskName string) string { return auditNamePrefix + taskName }

// WriteRecord upserts a durable, NON-owner-ref'd ConfigMap holding rec, plus
// emits the record as a single structured log line. The absence of an owner
// reference is deliberate: the record must outlive the AgenticTask so it
// remains a compliance trail after task garbage-collection. namespace is the
// audit namespace (caller passes the task namespace when unset upstream).
func WriteRecord(ctx context.Context, c client.Client, namespace string, rec Record, log logr.Logger) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: marshal record: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AuditConfigMapName(rec.Task.Name),
			Namespace: namespace,
			Labels: map[string]string{
				AuditLabel:                  "true",
				AuditTaskLabel:              rec.Task.Name,
				"app.kubernetes.io/part-of": "foreman",
			},
			// No OwnerReferences: survives task GC by design.
		},
		Data: map[string]string{auditDataKey: string(data)},
	}

	existing := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: namespace, Name: cm.Name}
	switch getErr := c.Get(ctx, key, existing); {
	case apierrors.IsNotFound(getErr):
		if err := c.Create(ctx, cm); err != nil {
			return fmt.Errorf("audit: create configmap: %w", err)
		}
	case getErr == nil:
		existing.Data = cm.Data
		existing.Labels = cm.Labels
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("audit: update configmap: %w", err)
		}
	default:
		return fmt.Errorf("audit: get configmap: %w", getErr)
	}

	// Structured audit stream line for SIEM/Loki ingestion. The durable
	// ConfigMap above is the primary record; this is the streaming copy.
	log.Info("foreman.audit",
		"task", rec.Task.Name, "namespace", rec.Task.Namespace,
		"verdict", rec.Verdict, "node", rec.AssignedNode,
		"repo", rec.Repo, "issue", rec.Issue, "schemaVersion", rec.SchemaVersion)
	return nil
}
