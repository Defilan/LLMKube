package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
	"github.com/defilantech/llmkube/pkg/foreman/agent/worktracker"
)

// These pin the two tool-side bypasses from #1298. Each fails against the
// pre-fix code: a third-party seam was injectable, satisfied the interface,
// and was then routed around at the moment it mattered.

// fakeCodeHost records the slug it was asked to resolve and answers with a
// non-GitHub URL, so a github.com result proves the seam was bypassed.
type fakeCodeHost struct{ asked []string }

func (f *fakeCodeHost) ResolveCloneURL(repoSlug string) string {
	f.asked = append(f.asked, repoSlug)
	return "https://forge.example.org/" + repoSlug + ".git"
}

func (f *fakeCodeHost) EnsureChangeRequest(
	context.Context, string, string, string, string, string,
) (string, bool, error) {
	return "", false, nil
}

func (f *fakeCodeHost) HeadCommitSubject(context.Context, string, string) (string, error) {
	return "", nil
}

var _ codehost.CodeHost = (*fakeCodeHost)(nil)

// TestGateJobCloneURLUsesInjectedCodeHost: the gate Job must clone from the
// injected provider. Before the fix the config hardcoded
// CloneURLBase="https://github.com" and nothing assigned it outside tests, so
// a Forgejo CodeHost still produced a github.com clone inside the Job.
func TestGateJobCloneURLUsesInjectedCodeHost(t *testing.T) {
	fake := &fakeCodeHost{}
	cfg := applyConfigDefaults(RunGateJobToolConfig{CodeHost: fake})

	got := resolveGateCloneURL(cfg, "group/sub/project", "")
	want := "https://forge.example.org/group/sub/project.git"
	if got != want {
		t.Errorf("clone URL = %q, want %q (injected CodeHost was bypassed)", got, want)
	}
	if strings.Contains(got, "github.com") {
		t.Errorf("clone URL still points at github.com: %q", got)
	}
	if len(fake.asked) != 1 || fake.asked[0] != "group/sub/project" {
		t.Errorf("CodeHost.ResolveCloneURL not consulted with the repo slug: %v", fake.asked)
	}
}

// An explicit cloneURL argument is how a fork-style push target reaches the
// Job, so it must still win over the seam.
func TestGateJobExplicitCloneURLStillWins(t *testing.T) {
	cfg := applyConfigDefaults(RunGateJobToolConfig{CodeHost: &fakeCodeHost{}})
	got := resolveGateCloneURL(cfg, "owner/name", "https://explicit.example/x.git")
	if got != "https://explicit.example/x.git" {
		t.Errorf("explicit cloneURL was overridden: %q", got)
	}
}

// With no seam injected, behaviour is unchanged: empty, so the template falls
// back to CloneURLBase + repo.
func TestGateJobWithoutCodeHostKeepsTemplateFallback(t *testing.T) {
	cfg := applyConfigDefaults(RunGateJobToolConfig{})
	if got := resolveGateCloneURL(cfg, "owner/name", ""); got != "" {
		t.Errorf("expected empty so the template uses CloneURLBase, got %q", got)
	}
	if cfg.CloneURLBase != "https://github.com" {
		t.Errorf("GitHub default should be untouched, got %q", cfg.CloneURLBase)
	}
}

// fakeWorkItems answers with content no GitHub client would return, so a
// GitHub-shaped answer proves the tool held its own client.
type fakeWorkItems struct {
	gotRepo string
	gotID   string
}

func (f *fakeWorkItems) Get(_ context.Context, repoSlug, id string) (*worktracker.WorkItem, error) {
	f.gotRepo, f.gotID = repoSlug, id
	return &worktracker.WorkItem{
		ID: id, Title: "from-injected-seam", Body: "b", State: "open",
		Labels: []string{"forge"},
	}, nil
}

var _ worktracker.WorkItems = (*fakeWorkItems)(nil)

// TestFetchIssueUsesInjectedWorkItems: before the fix the tool was built with
// its own githubissue.NewClient() in BuildAll, so injecting a WorkItems seam
// changed nothing and the model still read GitHub.
func TestFetchIssueUsesInjectedWorkItems(t *testing.T) {
	fake := &fakeWorkItems{}
	tool := &FetchIssueTool{
		WorkItems: fake,
		// Deliberately no Fetcher and no Token: an injected seam carries
		// its own auth, so a non-GitHub provider must not need a
		// GITHUB_TOKEN to be present.
	}

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"group/sub","number":7}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.gotRepo != "group/sub" || fake.gotID != "7" {
		t.Errorf("seam called with repo=%q id=%q, want group/sub and 7", fake.gotRepo, fake.gotID)
	}
	out, ok := res.Output.(map[string]any)
	if !ok {
		t.Fatalf("Output is %T, want map[string]any", res.Output)
	}
	if got := out["title"]; got != "from-injected-seam" {
		t.Errorf("title = %v, want the injected seam's value (tool bypassed the seam)", got)
	}
	// The output contract is unchanged: number stays the int requested.
	if got := out["number"]; got != 7 {
		t.Errorf("number = %v (%T), want int 7", got, got)
	}
}

// BuildAll must actually thread the seams; wiring them into ToolDeps and then
// not passing them would reproduce the bug with extra steps.
func TestBuildAllThreadsSeamsIntoTools(t *testing.T) {
	fakeCH := &fakeCodeHost{}
	fakeWI := &fakeWorkItems{}
	built := BuildAll(ToolDeps{
		Workspace: t.TempDir(),
		Token:     func() (string, error) { return "t", nil },
		WorkItems: fakeWI,
		CodeHost:  fakeCH,
	})

	var sawIssue, sawGate bool
	for _, tl := range built {
		switch v := tl.(type) {
		case *FetchIssueTool:
			sawIssue = true
			if v.WorkItems != worktracker.WorkItems(fakeWI) {
				t.Error("fetch_issue did not receive the injected WorkItems")
			}
		case *RunGateJobTool:
			sawGate = true
			if v.Cfg.CodeHost != codehost.CodeHost(fakeCH) {
				t.Error("run_gate_job did not receive the injected CodeHost")
			}
		}
	}
	if !sawIssue || !sawGate {
		t.Fatalf("expected both tools in BuildAll (issue=%v gate=%v)", sawIssue, sawGate)
	}
}
