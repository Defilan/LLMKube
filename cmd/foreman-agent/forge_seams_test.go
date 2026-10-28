/*
Copyright 2026.

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
	"encoding/json"
	"strings"
	"testing"

	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/worktracker"
)

type recordingWorkItems struct{ asked []string }

func (r *recordingWorkItems) Get(_ context.Context, repoSlug, id string) (*worktracker.WorkItem, error) {
	r.asked = append(r.asked, repoSlug+"#"+id)
	return &worktracker.WorkItem{ID: id, Title: "from the injected forge"}, nil
}

// The agent's tools reach the forge seams the executor was given. The
// registry factory left ToolDeps.WorkItems and CodeHost nil, so fetch_issue
// and the gate's clone URL always fell back to GitHub whatever seam the
// executor held (#1298).
func TestRegistryFactoryThreadsForgeSeamsIntoTools(t *testing.T) {
	items := &recordingWorkItems{}
	factory := makeRegistryFactory(fake.NewClientBuilder().Build(), k8sfake.NewClientset(), "foreman", "",
		forgeSeams{workItems: items})
	ag := &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{Tools: []string{"fetch_issue"}}}
	reg, err := factory(context.Background(), t.TempDir(), ag, false)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	res, err := reg.Dispatch(context.Background(), "fetch_issue", json.RawMessage(`{"repo":"group/project","number":7}`))
	if err != nil {
		t.Fatalf("fetch_issue: %v", err)
	}
	if len(items.asked) != 1 || items.asked[0] != "group/project#7" {
		t.Errorf("injected WorkItems calls = %v, want one for group/project#7", items.asked)
	}
	if out, _ := json.Marshal(res.Output); !strings.Contains(string(out), "from the injected forge") {
		t.Errorf("fetch_issue output = %s, want the injected forge's work item", out)
	}
}
