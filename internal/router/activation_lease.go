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

package router

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SwapCoordinator decides which router-proxy replica drives a ModelPool swap.
// With spec.proxy.replicas > 1 the in-process Activator mutex only serializes
// swaps inside one replica, so a per-pool lease makes activation a real
// single-writer: the owner scales the member, every other replica defers and
// waits for it (#1477).
//
// It is an interface so the swap policy stays unit-testable without a live API
// server; production injects LeaseCoordinator.
type SwapCoordinator interface {
	// Acquire reports whether this replica owns swap decisions for poolKey. A
	// non-owner must not write the member. Ownership is per call: the caller
	// releases when its swap completes.
	Acquire(ctx context.Context, poolKey string) (bool, error)

	// Release gives up ownership of poolKey if this replica holds it. Safe to
	// call when ownership was never acquired, or was lost to a takeover.
	Release(ctx context.Context, poolKey string)
}

const (
	// defaultLeaseDuration is how long a swap lease survives without renewal.
	// Long enough to cover a slow cold load, short enough that a crashed owner
	// frees the pool quickly.
	defaultLeaseDuration = 60 * time.Second
	leaseNamePrefix      = "llmkube-pool-"
)

// LeaseCoordinator implements SwapCoordinator over coordination.k8s.io Leases.
// A lease held by another live replica means a swap is already being driven, so
// this replica defers rather than issuing a competing member write.
type LeaseCoordinator struct {
	client    client.Client
	namespace string
	identity  string
	duration  time.Duration

	// nowFn is overridable in tests.
	nowFn func() time.Time
}

// NewLeaseCoordinator builds a LeaseCoordinator whose holder identity is the
// process hostname, which is the pod name under Kubernetes.
func NewLeaseCoordinator(cl client.Client, namespace string) *LeaseCoordinator {
	identity, err := os.Hostname()
	if err != nil || identity == "" {
		identity = "unknown"
	}
	return &LeaseCoordinator{
		client:    cl,
		namespace: namespace,
		identity:  identity,
		duration:  defaultLeaseDuration,
		nowFn:     time.Now,
	}
}

// Acquire claims the pool's swap lease for this replica, or reports that
// another live replica holds it. A lease whose RenewTime plus duration has
// lapsed is taken over, so a crashed owner does not wedge the pool forever. A
// write that loses the resourceVersion race reports not-owned rather than
// retrying: the winner owns the swap.
func (c *LeaseCoordinator) Acquire(ctx context.Context, poolKey string) (bool, error) {
	name := leaseNamePrefix + sanitizeLeaseName(poolKey)
	lease := &coordinationv1.Lease{}
	err := c.client.Get(ctx, types.NamespacedName{Namespace: c.namespace, Name: name}, lease)
	if apierrors.IsNotFound(err) {
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(c.identity),
				LeaseDurationSeconds: ptr.To(int32(c.duration.Seconds())),
				RenewTime:            &metav1.MicroTime{Time: c.nowFn()},
			},
		}
		if cerr := c.client.Create(ctx, lease); cerr != nil {
			if apierrors.IsAlreadyExists(cerr) {
				return false, nil
			}
			return false, fmt.Errorf("create activation lease %s: %w", name, cerr)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get activation lease %s: %w", name, err)
	}
	if holder := ptr.Deref(lease.Spec.HolderIdentity, ""); holder != "" && holder != c.identity {
		if !c.expired(lease) {
			return false, nil
		}
	}

	// We hold it, nobody holds it, or the previous holder lapsed: take or renew.
	lease.Spec.HolderIdentity = ptr.To(c.identity)
	lease.Spec.LeaseDurationSeconds = ptr.To(int32(c.duration.Seconds()))
	lease.Spec.RenewTime = &metav1.MicroTime{Time: c.nowFn()}
	if uerr := c.client.Update(ctx, lease); uerr != nil {
		if apierrors.IsConflict(uerr) {
			return false, nil
		}
		return false, fmt.Errorf("renew activation lease %s: %w", name, uerr)
	}
	return true, nil
}

// Release clears this replica's hold so the next swap can be driven without
// waiting out the duration. A lease now held by another replica is left alone.
func (c *LeaseCoordinator) Release(ctx context.Context, poolKey string) {
	name := leaseNamePrefix + sanitizeLeaseName(poolKey)
	lease := &coordinationv1.Lease{}
	if err := c.client.Get(ctx, types.NamespacedName{Namespace: c.namespace, Name: name}, lease); err != nil {
		return
	}
	if ptr.Deref(lease.Spec.HolderIdentity, "") != c.identity {
		return
	}
	lease.Spec.HolderIdentity = ptr.To("")
	lease.Spec.RenewTime = &metav1.MicroTime{Time: c.nowFn().Add(-2 * c.duration)}
	_ = c.client.Update(ctx, lease)
}

// expired reports whether the lease's hold has lapsed by its RenewTime and
// duration.
func (c *LeaseCoordinator) expired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil {
		return true
	}
	d := time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 0)) * time.Second
	if d <= 0 {
		d = defaultLeaseDuration
	}
	return c.nowFn().After(lease.Spec.RenewTime.Add(d))
}

// sanitizeLeaseName folds an arbitrary pool key into a DNS-1123 subdomain so it
// can name a Lease.
func sanitizeLeaseName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "pool"
	}
	return out
}
