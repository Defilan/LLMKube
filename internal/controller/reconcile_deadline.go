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
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DeadlineReconciler bounds every wrapped reconcile with a context deadline.
// A call that never returns (to the API server, or to a pod endpoint on the
// Metal path) would otherwise hold its worker forever; with the deadline it
// fails instead, the workqueue requeues the object with backoff, and the
// controller's error metric moves so the stall is visible (#2069). A timeout
// of zero or less disables the bound and passes the request through.
type DeadlineReconciler struct {
	Inner   reconcile.Reconciler
	Timeout time.Duration
	Log     logr.Logger
}

// Reconcile implements reconcile.Reconciler.
func (d *DeadlineReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if d.Timeout <= 0 {
		return d.Inner.Reconcile(ctx, req)
	}
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()

	res, err := d.Inner.Reconcile(ctx, req)
	// Only a deadline counts here. A plain cancellation means the manager is
	// shutting down, where relabeling the error buys nothing, and an inner
	// error that already reports the deadline is left alone.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		d.Log.Error(ctx.Err(), "reconcile exceeded its deadline and was cancelled",
			"timeout", d.Timeout.String(), "namespace", req.Namespace, "name", req.Name)
		err = fmt.Errorf("reconcile exceeded deadline %s: %w", d.Timeout, ctx.Err())
	}
	return res, err
}

// SetupWithManager completes the inner reconciler's controller registration
// with the wrapper in the reconcile path: the builder completes the wrapper,
// whose Reconcile applies the deadline before delegating. The reconcilers
// wrapped in cmd/main.go set their fields explicitly at construction, so
// nothing depends on the manager injecting through the wrapper.
func (d *DeadlineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	setup, ok := d.Inner.(interface{ SetupWithManager(ctrl.Manager) error })
	if !ok {
		return fmt.Errorf("inner reconciler %T does not implement SetupWithManager", d.Inner)
	}
	return setup.SetupWithManager(mgr)
}
