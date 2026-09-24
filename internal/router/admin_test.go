/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestProxyAdminBudgets checks the read-only admin endpoint the reconciler
// polls: it serves the rolling-window snapshot, and it is not mounted on the
// inference listener unless MountAdmin is called.
func TestProxyAdminBudgets(t *testing.T) {
	proxy, inferenceMux, _ := budgetTestProxy(t, []Budget{
		{Name: "router-cap", Scope: BudgetScopeRouter, Window: time.Hour, MaxTokens: 1000000},
	})

	// The admin path is not served on the inference mux: it is opt-in and
	// mounted separately, so the data plane carries no admin surface.
	req := httptest.NewRequest(http.MethodGet, "/admin/budgets", nil)
	rec := httptest.NewRecorder()
	inferenceMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin path on the inference mux = %d, want 404", rec.Code)
	}

	adminMux := http.NewServeMux()
	proxy.MountAdmin(adminMux)

	// Charge a served request so the snapshot carries a non-zero window.
	served := budgetPost(t, inferenceMux, nil)
	_ = served.Body.Close()
	if served.StatusCode != http.StatusOK {
		t.Fatalf("served request status = %d, want 200", served.StatusCode)
	}

	rec = httptest.NewRecorder()
	adminMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/budgets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200", rec.Code)
	}
	var got []struct {
		Name       string `json:"name"`
		UsedTokens int64  `json:"usedTokens"`
		MaxTokens  int64  `json:"maxTokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode admin body: %v", err)
	}
	if len(got) != 1 || got[0].Name != "router-cap" || got[0].UsedTokens != 100 {
		t.Fatalf("admin snapshot = %+v, want one router-cap entry with usedTokens 100", got)
	}
}
