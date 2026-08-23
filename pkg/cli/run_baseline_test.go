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

package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/defilantech/llmkube/pkg/foreman/audit"
)

// auditCMIn builds an audit-record ConfigMap in the given namespace, shaped
// the way pkg/foreman/audit's writer shapes it: the audit label, plus the
// JSON Record under the audit.json data key.
func baselineCMIn(t *testing.T, namespace, name, agent, kind, verdict string, elapsed float64) *corev1.ConfigMap {
	t.Helper()
	rec := audit.Record{
		SchemaVersion: "foreman.audit.v1",
		Verdict:       verdict,
		// BuildRecord derives this from AgenticTask.SucceededOnTarget(),
		// which is true exactly for a Succeeded task whose verdict is GO
		// or GATE-PASS. Keep the fixture consistent with the verdict so it
		// still describes a record the writer could actually emit.
		SucceededOnTarget: verdict == "GO" || verdict == "GATE-PASS",
		ElapsedSec:        elapsed,
		Agent:             &audit.AgentRef{Name: agent},
		Task:              audit.TaskRef{Name: name, Kind: kind},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreman-audit-" + name,
			Namespace: namespace,
			Labels:    map[string]string{audit.AuditLabel: "true"},
		},
		Data: map[string]string{"audit.json": string(b)},
	}
}

func baselineCM(t *testing.T, name, agent, kind, verdict string, elapsed float64) *corev1.ConfigMap {
	t.Helper()
	return baselineCMIn(t, "default", name, agent, kind, verdict, elapsed)
}

func TestBaselineFor_MediansSuccessfulIssueFixRuns(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(
		baselineCM(t, "a", "qwen38-coder", "issue-fix", "GO", 3000),
		baselineCM(t, "b", "qwen38-coder", "issue-fix", "GO", 3600),
		baselineCM(t, "c", "qwen38-coder", "issue-fix", "GO", 9000),
		// Excluded: different agent, non-terminal-good verdict, wrong kind.
		baselineCM(t, "d", "other-coder", "issue-fix", "GO", 60),
		baselineCM(t, "e", "qwen38-coder", "issue-fix", "NO-GO", 60),
		baselineCM(t, "f", "qwen38-coder", "review", "GO", 60),
	).Build()

	got, err := BaselineFor(context.Background(), c, "default", "qwen38-coder")
	if err != nil {
		t.Fatalf("BaselineFor: %v", err)
	}
	if got != 3600*time.Second {
		t.Errorf("BaselineFor = %v, want %v (median of 3000/3600/9000, mean would be 5200)", got, 3600*time.Second)
	}
}

func TestBaselineFor_NoHistoryReturnsDefault(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	got, err := BaselineFor(context.Background(), c, "default", "qwen38-coder")
	if err != nil {
		t.Fatalf("BaselineFor: %v", err)
	}
	if got != DefaultBaseline {
		t.Errorf("BaselineFor = %v, want the default %v", got, DefaultBaseline)
	}
}

// TestBaselineFor_FiltersAreLoadBearing pins each selection rule on its own.
//
// The happy-path test above cannot do this: its three excluded records all
// carry elapsed=60, and adding one 60 to {3000,3600,4200} leaves the median
// at 3600, so dropping any single filter still passes it. Every case here is
// built so that removing exactly one rule from BaselineFor changes the
// answer. Each case names the mutation it exists to catch.
func TestBaselineFor_FiltersAreLoadBearing(t *testing.T) {
	const agent = "qwen38-coder"

	// A labelled audit ConfigMap holding JSON that is not a Record.
	malformed := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreman-audit-malformed",
			Namespace: "default",
			Labels:    map[string]string{audit.AuditLabel: "true"},
		},
		Data: map[string]string{"audit.json": "{ this is not json"},
	}
	// A labelled audit ConfigMap with no audit.json key at all.
	keyless := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreman-audit-keyless",
			Namespace: "default",
			Labels:    map[string]string{audit.AuditLabel: "true"},
		},
		Data: map[string]string{"other.json": "{}"},
	}
	// Well-formed records that are NOT labelled as audit records. Three of
	// them, so that dropping the label selector drags the median down to 60
	// rather than leaving it at 3600.
	unlabelled := func(name string) *corev1.ConfigMap {
		cm := baselineCM(t, name, agent, "issue-fix", "GO", 60)
		cm.Labels = nil
		return cm
	}

	cases := []struct {
		name string
		// mutation names the single production change this case detects.
		mutation string
		objs     []client.Object
		want     time.Duration
	}{
		{
			name:     "records for other agents are excluded",
			mutation: "delete the rec.Agent.Name != agent guard",
			objs: []client.Object{
				baselineCM(t, "mine", agent, "issue-fix", "GO", 3600),
				baselineCM(t, "x1", "other-coder", "issue-fix", "GO", 60),
				baselineCM(t, "x2", "other-coder", "issue-fix", "GO", 60),
				baselineCM(t, "x3", "other-coder", "issue-fix", "GO", 60),
			},
			want: 3600 * time.Second,
		},
		{
			name:     "records with no agent block are excluded",
			mutation: "delete the rec.Agent == nil guard",
			objs: []client.Object{
				baselineCM(t, "mine", agent, "issue-fix", "GO", 3600),
				baselineAgentlessCM(t, "n1", "issue-fix", "GO", 60),
				baselineAgentlessCM(t, "n2", "issue-fix", "GO", 60),
				baselineAgentlessCM(t, "n3", "issue-fix", "GO", 60),
			},
			want: 3600 * time.Second,
		},
		{
			name:     "unsuccessful verdicts are excluded",
			mutation: "delete the !rec.SucceededOnTarget guard",
			objs: []client.Object{
				baselineCM(t, "mine", agent, "issue-fix", "GO", 3600),
				baselineCM(t, "x1", agent, "issue-fix", "NO-GO", 60),
				baselineCM(t, "x2", agent, "issue-fix", "CODER-GATE-FAILED", 60),
				baselineCM(t, "x3", agent, "issue-fix", "NEEDS-VERIFICATION", 60),
			},
			want: 3600 * time.Second,
		},
		{
			name:     "GATE-PASS counts as cleanly successful",
			mutation: "replace rec.SucceededOnTarget with rec.Verdict == GO",
			objs: []client.Object{
				baselineCM(t, "gp", agent, "issue-fix", "GATE-PASS", 1800),
			},
			want: 1800 * time.Second,
		},
		{
			name:     "non issue-fix kinds are excluded",
			mutation: "delete the rec.Task.Kind != issue-fix guard",
			objs: []client.Object{
				baselineCM(t, "mine", agent, "issue-fix", "GO", 3600),
				baselineCM(t, "x1", agent, "review", "GO", 60),
				baselineCM(t, "x2", agent, "review", "GO", 60),
				baselineCM(t, "x3", agent, "review", "GO", 60),
			},
			want: 3600 * time.Second,
		},
		{
			name:     "other namespaces are excluded",
			mutation: "drop client.InNamespace(namespace) from the List",
			objs: []client.Object{
				baselineCM(t, "mine", agent, "issue-fix", "GO", 3600),
				baselineCMIn(t, "other-ns", "x1", agent, "issue-fix", "GO", 60),
				baselineCMIn(t, "other-ns", "x2", agent, "issue-fix", "GO", 60),
				baselineCMIn(t, "other-ns", "x3", agent, "issue-fix", "GO", 60),
			},
			want: 3600 * time.Second,
		},
		{
			name:     "unlabelled ConfigMaps are excluded",
			mutation: "drop client.MatchingLabels{audit.AuditLabel: true} from the List",
			objs: []client.Object{
				baselineCM(t, "mine", agent, "issue-fix", "GO", 3600),
				unlabelled("u1"), unlabelled("u2"), unlabelled("u3"),
			},
			want: 3600 * time.Second,
		},
		{
			// 60/3600/36000: mean is 13220s, so a mean would fail here, and
			// so would picking the min or the max.
			name:     "the middle value is taken, not the mean or an end",
			mutation: "replace secs[n/2] with the mean, secs[0], or secs[len(secs)-1]",
			objs: []client.Object{
				baselineCM(t, "m1", agent, "issue-fix", "GO", 36000),
				baselineCM(t, "m2", agent, "issue-fix", "GO", 60),
				baselineCM(t, "m3", agent, "issue-fix", "GO", 3600),
			},
			want: 3600 * time.Second,
		},
		{
			// Two records is the NORMAL state for a freshly-added agent,
			// which is exactly when the baseline is load-bearing. The
			// upper-middle element would return 36000 here, and at
			// DefaultStallFactor that is a 25-hour stall threshold bought
			// by one pathological run. The lower-middle would return 60.
			name:     "an even count averages the middle two, it does not pick a side",
			mutation: "use secs[len/2] or secs[(len-1)/2] instead of averaging on even n",
			objs: []client.Object{
				baselineCM(t, "e1", agent, "issue-fix", "GO", 60),
				baselineCM(t, "e2", agent, "issue-fix", "GO", 36000),
			},
			want: 18030 * time.Second,
		},
		{
			// Four records: upper-middle gives 3600, lower-middle 3000,
			// the true median 3300. Pins the even branch at a length where
			// both off-by-one variants are still plausible-looking.
			name:     "an even count of four averages the middle two",
			mutation: "use secs[len/2] or secs[(len-1)/2] instead of averaging on even n",
			objs: []client.Object{
				baselineCM(t, "f1", agent, "issue-fix", "GO", 60),
				baselineCM(t, "f2", agent, "issue-fix", "GO", 3000),
				baselineCM(t, "f3", agent, "issue-fix", "GO", 3600),
				baselineCM(t, "f4", agent, "issue-fix", "GO", 9000),
			},
			want: 3300 * time.Second,
		},
		{
			// The reason SucceededOnTarget beats a Verdict string compare:
			// a GO verdict on a task that never reached Phase == Succeeded
			// is not a completed run, and its elapsed time is not evidence
			// of how long the agent takes. The old string check let these
			// in. See AgenticTask.SucceededOnTarget().
			name:     "a GO verdict on a task that never succeeded is excluded",
			mutation: "replace rec.SucceededOnTarget with the old GO/GATE-PASS string compare",
			objs: []client.Object{
				baselineCM(t, "mine", agent, "issue-fix", "GO", 3600),
				baselineUnfinishedCM(t, "u1", agent, "issue-fix", "GO", 60),
				baselineUnfinishedCM(t, "u2", agent, "issue-fix", "GO", 60),
				baselineUnfinishedCM(t, "u3", agent, "issue-fix", "GATE-PASS", 60),
			},
			want: 3600 * time.Second,
		},
		{
			name:     "records with no elapsed time are ignored, not counted as zero",
			mutation: "delete the rec.ElapsedSec > 0 guard",
			objs: []client.Object{
				baselineCM(t, "z1", agent, "issue-fix", "GO", 0),
				baselineCM(t, "z2", agent, "issue-fix", "GO", 0),
				baselineCM(t, "z3", agent, "issue-fix", "GO", 0),
			},
			want: DefaultBaseline,
		},
		{
			name:     "a malformed record is skipped, not fatal",
			mutation: "return the json.Unmarshal error instead of continuing",
			objs:     []client.Object{malformed, keyless},
			want:     DefaultBaseline,
		},
		{
			name:     "a malformed record does not suppress the usable ones",
			mutation: "return the json.Unmarshal error instead of continuing",
			objs: []client.Object{
				malformed,
				baselineCM(t, "mine", agent, "issue-fix", "GO", 3600),
			},
			want: 3600 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithObjects(tc.objs...).Build()
			got, err := BaselineFor(context.Background(), c, "default", agent)
			if err != nil {
				t.Fatalf("BaselineFor: %v", err)
			}
			if got != tc.want {
				t.Errorf("BaselineFor = %v, want %v (regression catches: %s)", got, tc.want, tc.mutation)
			}
		})
	}
}

// baselineUnfinishedCM builds a record whose Verdict reads as a success but
// whose task never reached Phase == Succeeded, so BuildRecord would have set
// SucceededOnTarget false. This is the case the verdict-string check got
// wrong and the record's own flag gets right.
func baselineUnfinishedCM(t *testing.T, name, agent, kind, verdict string, elapsed float64) *corev1.ConfigMap {
	t.Helper()
	cm := baselineCM(t, name, agent, kind, verdict, elapsed)
	patchRecord(t, cm, func(rec *audit.Record) { rec.SucceededOnTarget = false })
	return cm
}

// patchRecord rewrites the audit.json payload of cm through mutate.
func patchRecord(t *testing.T, cm *corev1.ConfigMap, mutate func(*audit.Record)) {
	t.Helper()
	var rec audit.Record
	if err := json.Unmarshal([]byte(cm.Data["audit.json"]), &rec); err != nil {
		t.Fatal(err)
	}
	mutate(&rec)
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	cm.Data["audit.json"] = string(b)
}

// agentlessCM builds an audit record with no agent block at all, which is
// what the writer emits when the Agent could not be resolved.
func baselineAgentlessCM(t *testing.T, name, kind, verdict string, elapsed float64) *corev1.ConfigMap {
	t.Helper()
	cm := baselineCM(t, name, "", kind, verdict, elapsed)
	patchRecord(t, cm, func(rec *audit.Record) { rec.Agent = nil })
	return cm
}
