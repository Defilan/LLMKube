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
// Package-level testNS is a fresh namespace per test, so tests sharing a
// server never see each other's objects. That is only sound because no test in
// this package calls t.Parallel: the helpers assign testNS before the test
// runs, and a parallel test would race another's assignment.
var (
	sharedEnvMu        sync.Mutex
	sharedEnvs         = map[string]*sharedEnv{}
	controlPlanes      atomic.Int32
	controlPlaneStarts []string
	testNS             = metav1.NamespaceDefault
)

// maxTestControlPlanes is the number of control planes a run of this package
// may start: four shared CRD sets (base, base+aigateway, base+pyrra,
// base+inference-extension), two private CRD subsets, the Ginkgo suite's, and
// one of headroom. Exceeding it means a test went back to its own environment.
const maxTestControlPlanes = 8

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
	key := strings.Join(crdPaths, "\x00")
	sharedEnvMu.Lock()
	shared, ok := sharedEnvs[key]
	var startErr error
	if !ok {
		shared, startErr = startTestEnv(crdPaths, t.Name())
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
// returns it, so tests that share the server never collide.
func newTestNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "llmkube-test-"}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create test namespace: %v", err)
	}
	return ns.Name
}

// newTestNS points testNS at a fresh namespace. Every env helper calls it
// before returning, so a test and its fixtures agree on one namespace.
func newTestNS(t *testing.T, c client.Client) {
	testNS = newTestNamespace(t, c)
}

// TestMain stops the shared environments after all tests have run, and fails
// the package (not just one test) when a start went past the budget: a
// ran-away control plane count is what pushed internal/controller into go
// test's timeout (#2141).
func TestMain(m *testing.M) {
	code := m.Run()
	stopSharedTestEnvs()
	if n := controlPlanes.Load(); n > maxTestControlPlanes {
		fmt.Fprintf(os.Stderr,
			"envtest control planes started: %d (budget %d); shared set and its first caller:\n  %s\n",
			n, maxTestControlPlanes, strings.Join(controlPlaneStarts, "\n  "))
		code = 1
	}
	os.Exit(code)
}
