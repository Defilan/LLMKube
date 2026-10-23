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
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Control planes are started once per distinct CRD path set and shared by
// every test that asks for the same set (#2141). A start is an etcd plus a
// kube-apiserver, about 4 s; the tests below used to start 63 of them and the
// package ran into go test's 10 minute timeout on CI runners.
//
// Package-level testNS is the namespace the current test's env helper created,
// so tests sharing a server never see each other's objects. It changes when a
// test calls an env helper, and it changes when a test calls
// newTestNamespace, so a test that takes its own namespace still finds
// everything it names off testNS in the namespace it created. That is only
// sound because no test in this package calls t.Parallel: a parallel test
// would race another's assignment.
var (
	sharedEnvMu        sync.Mutex
	sharedEnvs         = map[string]*sharedEnv{}
	controlPlanes      atomic.Int32
	controlPlaneStarts []string
	testNS             = metav1.NamespaceDefault
)

// maxTestControlPlanes is the number of control planes a run of this package
// may start: four shared CRD sets (base, base+aigateway, base+pyrra,
// base+inference-extension), two private CRD subsets, and the Ginkgo suite's,
// which counts itself in suite_test.go. A clean run starts 7 and anything
// above fails here, so a genuinely needed fifth shared set raises this
// constant deliberately rather than silently spending slack.
const maxTestControlPlanes = 7

type sharedEnv struct {
	env    *envtest.Environment
	cfg    *rest.Config
	client client.Client
}

// sharedTestEnv returns the control plane for the given CRD path set, starting
// it on first use, and a fresh namespace in testNS for the calling test. The
// stop function is a no-op: TestMain stops every shared server after all tests
// have run. A new CRD path set here costs a full start, so add one only for a
// set the tests genuinely need.
func sharedTestEnv(t *testing.T, crdPaths ...string) (client.Client, *rest.Config, func()) {
	t.Helper()
	// Sorted, so the same set written in a different order is the same set: an
	// order-sensitive key would quietly cost another full control plane.
	sorted := slices.Clone(crdPaths)
	slices.Sort(sorted)
	key := strings.Join(sorted, "\x00")
	sharedEnvMu.Lock()
	shared, ok := sharedEnvs[key]
	var startErr error
	if !ok {
		shared, startErr = startTestEnv(sorted, t.Name())
		if startErr == nil {
			sharedEnvs[key] = shared
		}
	}
	sharedEnvMu.Unlock()
	if startErr != nil {
		t.Fatalf("start shared envtest %v: %v", crdPaths, startErr)
	}
	newTestNS(t, shared.client)
	return shared.client, shared.cfg, func() {}
}

// privateTestEnv starts a control plane for one test, for the cases that need
// CRDs ABSENT or a subset of them, and stops it when the test returns. It
// counts against the same budget as a shared one.
func privateTestEnv(t *testing.T, crdPaths ...string) (client.Client, *rest.Config, func()) {
	t.Helper()
	env, err := startTestEnv(crdPaths, t.Name())
	if err != nil {
		t.Fatalf("start envtest %v: %v", crdPaths, err)
	}
	newTestNS(t, env.client)
	return env.client, env.cfg, func() { _ = env.env.Stop() }
}

// startTestEnv boots one environment and registers it with the budget.
func startTestEnv(crdPaths []string, startedBy string) (*sharedEnv, error) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, err
	}
	controlPlanes.Add(1)
	controlPlaneStarts = append(controlPlaneStarts, startedBy)
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		_ = env.Stop()
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	return &sharedEnv{env: env, cfg: cfg, client: c}, nil
}

// stopSharedTestEnvs stops every shared environment. Called from TestMain.
func stopSharedTestEnvs() {
	sharedEnvMu.Lock()
	defer sharedEnvMu.Unlock()
	for _, shared := range sharedEnvs {
		_ = shared.env.Stop()
	}
	sharedEnvs = map[string]*sharedEnv{}
}

// newTestNamespace creates a namespace with a generated name on the server and
// makes it the current test's testNS, for a test that wants to create its
// object somewhere exact and then look it up through the shared helpers.
func newTestNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "llmkube-test-"}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create test namespace: %v", err)
	}
	testNS = ns.Name
	return ns.Name
}

// newTestNS points testNS at a fresh namespace. Every env helper calls it
// before returning, so a test and its fixtures agree on one namespace.
func newTestNS(t *testing.T, c client.Client) {
	newTestNamespace(t, c)
}

// TestMain stops the shared environments after all tests have run, and fails
// the package (not just one test) when a start went past the budget: a
// ran-away control plane count is what pushed internal/controller into go
// test's timeout (#2141). The report goes to stdout and stderr so it lands in
// the log and in the `go test -json` artifact, which is where a timeout's
// cause is read from (hack/go-test-report.sh).
func TestMain(m *testing.M) {
	code := m.Run()
	stopSharedTestEnvs()
	if n := controlPlanes.Load(); n > maxTestControlPlanes {
		report := fmt.Sprintf(
			"envtest control planes started: %d (budget %d); shared set and its first caller:\n  %s\n",
			n, maxTestControlPlanes, strings.Join(controlPlaneStarts, "\n  "))
		_, _ = fmt.Fprint(os.Stdout, report)
		_, _ = fmt.Fprint(os.Stderr, report)
		code = 1
	}
	os.Exit(code)
}

// TestNewTestNamespaceMovesTestNS pins the invariant the fixture helpers depend
// on: after newTestNamespace, testNS is the namespace it just created. A test
// that takes its own namespace and then names a fixture off testNS
// (getUnstructured, assertNotExists, setISvcReadyReplicas and the rest) would
// otherwise read whichever namespace the previous test left behind and look for
// its object in the wrong place.
func TestNewTestNamespaceMovesTestNS(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)

	ns := newTestNamespace(t, c)
	if testNS != ns {
		t.Fatalf("newTestNamespace returned %s but left testNS at %s: a test naming a fixture off testNS would look in the wrong namespace", ns, testNS)
	}
}
