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

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DeadlineReconciler bounds the context-aware parts of every wrapped
// reconcile with a context deadline: API requests and HTTP requests built
// with the reconcile context fail at the deadline, so the workqueue requeues
// the object with backoff and the controller's error metric moves (#2069).
// It cannot interrupt code that ignores its context (a local file copy), so
// such a reconcile still completes; a success that lands after the deadline
// is kept and logged as an overrun, not relabeled as a failure. A timeout of
// zero or less disables the bound and passes the request through.
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
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()

	res, err := d.Inner.Reconcile(ctx, req)
	// Only a deadline counts here, and only a failed reconcile is relabeled:
	// an inner that ignored its context and still succeeded keeps its
	// success, with the overrun logged. A plain cancellation means the
	// manager is shutting down, where relabeling the error buys nothing, and
	// an inner error that already reports the deadline is left alone.
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return res, err
	}
	if err == nil {
		d.Log.Info("reconcile finished after its deadline",
			"elapsed", time.Since(start).String(), "timeout", d.Timeout.String(),
			"namespace", req.Namespace, "name", req.Name)
		return res, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return res, err
	}
	d.Log.Error(ctx.Err(), "reconcile exceeded its deadline and was cancelled",
		"timeout", d.Timeout.String(), "namespace", req.Namespace, "name", req.Name)
	return res, fmt.Errorf("reconcile exceeded deadline %s: %w", d.Timeout, ctx.Err())
}

// WithReconcileDeadline returns the decorator a reconciler applies to itself
// inside its own SetupWithManager: the wrapper has to be the argument the
// reconciler passes to its builder's Complete, because that argument is what
// the controller stores as its Reconciler. cmd/main.go sets the field only
// when --reconcile-timeout is positive.
func WithReconcileDeadline(timeout time.Duration, log logr.Logger) func(reconcile.Reconciler) reconcile.Reconciler {
	return func(inner reconcile.Reconciler) reconcile.Reconciler {
		return &DeadlineReconciler{Inner: inner, Timeout: timeout, Log: log}
	}
}
