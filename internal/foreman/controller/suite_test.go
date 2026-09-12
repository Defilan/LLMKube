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

// Package controller_test (in-package: see go file `package controller`)
// hosts the envtest suite for the foreman API group's reconcilers. The
// shape mirrors internal/controller/suite_test.go so the project has a
// single recognizable testing pattern for both API groups.
package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

func TestForemanControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	suiteConfig := types.NewDefaultSuiteConfig()
	suiteConfig.RandomSeed = ginkgoSeed()
	RunSpecs(t, "Foreman Controller Suite", suiteConfig)
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	err := foremanv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	By("bootstrapping test environment with foreman CRDs")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
	}

	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// ginkgoSeed resolves the suite's random seed. Ginkgo randomizes top-level
// containers per run, so a test that leaks shared cluster state passes or
// fails on the draw (#1693). The seed is reported on every run so a failure
// can be replayed from the log alone, and GINKGO_SEED pins it for the replay:
// the Makefile picks one per run, exports it, and echoes it into the log
// (`make test GINKGO_SEED=<n>` to replay). Mirrors the helper in
// internal/controller/suite_test.go, as the suite files mirror each other.
func ginkgoSeed() int64 {
	if v := os.Getenv("GINKGO_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil && n > 0 {
			fmt.Printf("ginkgo seed: %d (pinned by GINKGO_SEED)\n", n)
			return n
		}
		fmt.Printf("ginkgo seed: ignoring invalid GINKGO_SEED %q, using a fresh one\n", v)
	}
	seed := time.Now().Unix()
	fmt.Printf("ginkgo seed: %d\n", seed)
	return seed
}

// getFirstFoundEnvTestBinaryDir mirrors the helper in
// internal/controller/suite_test.go: when running tests from an IDE
// without the Makefile, locate the kube-apiserver / etcd binaries
// `make setup-envtest` placed under bin/k8s/<version>.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
