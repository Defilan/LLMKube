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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// modelRouterBudgetPollInterval paces the status poll for a router that
// declares budgets. The proxy owns the rolling-window counters and pushes
// them nowhere, so the reconciler has to ask on a timer rather than wait for a
// watched object to change.
const modelRouterBudgetPollInterval = 30 * time.Second

// budgetProbeTimeout bounds a single admin poll so an unresponsive proxy
// cannot stall the reconcile loop.
const budgetProbeTimeout = 5 * time.Second

// budgetUsageWire mirrors the JSON served by the router-proxy budget admin
// endpoint. It is deliberately separate from the router package's internal
// BudgetUsage so the status contract survives changes to the store.
type budgetUsageWire struct {
	Name       string  `json:"name"`
	UsedTokens int64   `json:"usedTokens"`
	UsedUSD    float64 `json:"usedUSD"`
	MaxTokens  int64   `json:"maxTokens"`
	MaxUSD     float64 `json:"maxUSD"`
}

// routerHasBudgets reports whether the router declares any budget to enforce.
// Budget utilization is only meaningful when at least one cap exists.
func routerHasBudgets(mr *inferencev1alpha1.ModelRouter) bool {
	return mr.Spec.Policy != nil && len(mr.Spec.Policy.Budgets) > 0
}

// budgetStatusURL resolves the proxy admin URL for a router, honouring the
// reconciler's test override when set.
func (r *ModelRouterReconciler) budgetStatusURL(mr *inferencev1alpha1.ModelRouter) string {
	if r.BudgetStatusURL != nil {
		return r.BudgetStatusURL(mr)
	}
	return routerProxyBudgetEndpoint(mr)
}

// budgetStatusFromUsage maps the proxy's per-budget snapshot into the CRD's
// status shape. Entries that share a Name are summed, consumption and caps
// alike: a team-scoped budget yields one counter per header value, each
// carrying that value's cap, and BudgetStatus has no field to tell the values
// apart, so status reports one aggregate per named budget. Utilization is the
// larger of the token and dollar fractions when both caps are set, matching
// BudgetStatus's documented semantics.
func budgetStatusFromUsage(usage []budgetUsageWire) []inferencev1alpha1.BudgetStatus {
	type aggregate struct {
		tokens    int64
		usd       float64
		maxTokens int64
		maxUSD    float64
	}
	byName := map[string]*aggregate{}
	order := []string{}
	for _, u := range usage {
		agg, ok := byName[u.Name]
		if !ok {
			agg = &aggregate{}
			byName[u.Name] = agg
			order = append(order, u.Name)
		}
		agg.tokens += u.UsedTokens
		agg.usd += u.UsedUSD
		agg.maxTokens += u.MaxTokens
		agg.maxUSD += u.MaxUSD
	}
	sort.Strings(order)

	out := make([]inferencev1alpha1.BudgetStatus, 0, len(order))
	for _, name := range order {
		agg := byName[name]
		out = append(out, inferencev1alpha1.BudgetStatus{
			Name:        name,
			TokensUsed:  agg.tokens,
			USDUsed:     strconv.FormatFloat(agg.usd, 'f', 6, 64),
			Utilization: strconv.FormatFloat(budgetUtilization(agg.tokens, agg.usd, agg.maxTokens, agg.maxUSD), 'f', 6, 64),
		})
	}
	return out
}

// budgetUtilization returns the larger of the token and dollar fractions for
// the caps that are set. With no cap at all it returns 0.
func budgetUtilization(tokens int64, usd float64, maxTokens int64, maxUSD float64) float64 {
	util := 0.0
	if maxTokens > 0 {
		util = float64(tokens) / float64(maxTokens)
	}
	if maxUSD > 0 {
		if u := usd / maxUSD; u > util {
			util = u
		}
	}
	return util
}

// probeBudgetUtilization reads the proxy's budget snapshot. A transport error
// or a non-200 is returned to the caller so it can leave the previous status
// in place rather than publish a zeroed one.
func probeBudgetUtilization(ctx context.Context, url string) ([]budgetUsageWire, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("budget probe request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("budget probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("budget probe: proxy answered %s", resp.Status)
	}
	var usage []budgetUsageWire
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return nil, fmt.Errorf("budget probe decode: %w", err)
	}
	return usage, nil
}
