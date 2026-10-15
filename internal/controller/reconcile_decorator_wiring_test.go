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
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestReconcileDecoratorReachesTheController pins where the deadline is
// applied (#2069): a reconciler's ReconcileDecorator has to be applied inside
// SetupWithManager, at the builder's Complete site, because that argument is
// what the controller stores as its Reconciler. If the application is
// dropped, the controller calls the bare reconciler and the decorator is
// never invoked; every unit test of the decorator itself stays green, so
// this test is the only guard on the wiring.
func TestReconcileDecoratorReachesTheController(t *testing.T) {
	testEnv := &envtest.Environment{
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("stop envtest: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(inferencev1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	hits := make(chan struct{}, 8)
	rec := &ModelReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorder("decorator-wiring-test"),
	}
	rec.ReconcileDecorator = func(inner reconcile.Reconciler) reconcile.Reconciler {
		return reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			select {
			case hits <- struct{}{}:
			default:
			}
			// The inner reconciler's behavior is not this test's subject
			// (it needs no fields beyond the client to have run), so the
			// wrapper short-circuits to keep the test hermetic.
			return reconcile.Result{}, nil
		})
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup model controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	select {
	case <-mgr.Elected():
	case err := <-done:
		t.Fatalf("manager exited during startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("manager did not start")
	}

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "decorator-wiring", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf", Format: "gguf", Quantization: "Q4_K_M",
		},
	}
	if err := mgr.GetClient().Create(context.Background(), model); err != nil {
		t.Fatalf("create model: %v", err)
	}

	select {
	case <-hits:
	case <-time.After(10 * time.Second):
		t.Fatal("the decorator was never invoked: SetupWithManager did not apply it at the Complete site")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("manager exited with an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("manager did not stop")
	}
}
