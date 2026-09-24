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

package router

import (
	"encoding/json"
	"net/http"
)

// MountAdmin registers the read-only admin endpoints. They are mounted on the
// metrics listener rather than the inference listener so budget state stays
// off the data plane and NetworkPolicies can scope who reads it. The operator
// polls the same endpoint to publish ModelRouter.status.budgetUtilization.
func (p *Proxy) MountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/budgets", p.handleBudgets)
}

// handleBudgets serves the rolling-window budget snapshot. It reports what the
// proxy has enforced; it never mutates the store.
func (p *Proxy) handleBudgets(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p.budgets.Snapshot())
}
