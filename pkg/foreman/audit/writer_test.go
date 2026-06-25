/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package audit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWriteRecordCreatesDurableConfigMap(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	rec := Record{SchemaVersion: SchemaVersion, Task: TaskRef{Name: "coder-89", Namespace: "default"}, Verdict: "GO"}

	if err := WriteRecord(context.Background(), c, "default", rec, logr.Discard()); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "default", Name: "foreman-audit-coder-89"}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("audit ConfigMap not created: %v", err)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Errorf("audit ConfigMap MUST NOT be owner-ref'd (must survive task GC), got %d refs", len(cm.OwnerReferences))
	}
	if cm.Labels[AuditLabel] != "true" || cm.Labels[AuditTaskLabel] != "coder-89" {
		t.Errorf("labels wrong: %v", cm.Labels)
	}
	var got Record
	if err := json.Unmarshal([]byte(cm.Data[auditDataKey]), &got); err != nil {
		t.Fatalf("decode audit.json: %v", err)
	}
	if got.Verdict != "GO" || got.Task.Name != "coder-89" {
		t.Errorf("round-trip wrong: %+v", got)
	}
}

func TestWriteRecordIdempotentUpsert(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	rec := Record{SchemaVersion: SchemaVersion, Task: TaskRef{Name: "t", Namespace: "default"}, Verdict: "GO"}
	ctx := context.Background()
	if err := WriteRecord(ctx, c, "default", rec, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	rec.Verdict = "NO-GO"
	if err := WriteRecord(ctx, c, "default", rec, logr.Discard()); err != nil {
		t.Fatalf("second write: %v", err)
	}
	var cm corev1.ConfigMap
	_ = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "foreman-audit-t"}, &cm)
	var got Record
	_ = json.Unmarshal([]byte(cm.Data[auditDataKey]), &got)
	if got.Verdict != "NO-GO" {
		t.Errorf("upsert did not update, verdict=%q", got.Verdict)
	}
}
