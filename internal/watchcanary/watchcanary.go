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

// Package watchcanary detects a stalled watch path: an operator whose
// informers stop delivering events keeps its pod Running, its probes green
// and its webhooks answering while every controller goes silent (#2069).
// The canary renews a Lease through the API server and requires the
// manager's own informer to deliver the event back; a liveness check fails
// when a write has gone unobserved for too long, so the kubelet restarts an
// operator whose controllers can no longer see the cluster.
package watchcanary

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/defilantech/llmkube/internal/metrics"
)

const (
	// LeaseName is the name of the Lease the canary renews. It lives in the
	// operator's own namespace and the name is operator-reserved there: a
	// same-name lease is adopted by adding the canary label.
	LeaseName = "llmkube-controller-manager-watch-canary"

	// LabelKey / LabelValue mark the canary lease. The manager's Lease
	// informer is scoped to this label, so the watch covers exactly one
	// object and nothing else in the namespace is cached.
	LabelKey   = "llmkube.ai/watch-canary"
	LabelValue = "true"

	// canaryLeaseDurationSeconds is set on create only. Nothing expires the
	// lease on it; it exists so the object reads as a lease to kubectl.
	canaryLeaseDurationSeconds = 60

	// serviceAccountNamespaceFile is where the kubelet projects the pod's
	// namespace. Used as the last namespace fallback before disabling.
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// Options configures a Canary. Reader must read live (the manager's
// APIReader): a read through the manager's cache would hand back the stale
// object of a stalled informer and clobber the write with an old
// resourceVersion.
type Options struct {
	// Namespace is the operator's own namespace, where the canary lease lives.
	Namespace string
	// Interval is how often the canary writes the lease. Must be positive.
	Interval time.Duration
	// FailAfter is how long an unobserved write fails the liveness check.
	// Must be greater than Interval: one missed event must not fail the pod.
	FailAfter time.Duration
	// Reader reads the canary lease live, bypassing the manager's cache.
	Reader client.Reader
	// Writer creates and updates the canary lease. Writes always hit the API
	// server directly; the manager's client never caches writes.
	Writer client.Writer
	// Cache is the manager's cache. New registers the informer event handler
	// on it; the informer itself is scoped by CacheScope at manager setup.
	Cache cache.Cache
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Log receives the canary's own lifecycle and failure logs.
	Log logr.Logger
}

// Canary writes a Lease every Interval and marks the write observed when the
// manager's informer delivers the event back. It is a manager Runnable that
// needs leader election, so only the leader writes and the liveness check of
// a standby passes trivially.
type Canary struct {
	opts Options
	log  logr.Logger

	mu           sync.Mutex
	started      bool
	startedAt    time.Time
	haveObserved bool
	lastObserved time.Time
	sawWrite     bool
	up           bool
	healthy      bool
}

// CacheScope returns the cache.Options fragment that scopes the manager's
// Lease informer to exactly the canary's lease in namespace. ByObject keys by
// GVK, so this is safe only because no other controller watches Leases;
// scoping a GVK another controller watches (ConfigMaps, for example) would
// silently narrow that controller's watch too.
func CacheScope(namespace string) cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&coordinationv1.Lease{}: {
				Namespaces: map[string]cache.Config{namespace: {}},
				Label:      labels.SelectorFromSet(labels.Set{LabelKey: LabelValue}),
			},
		},
	}
}

// ResolveNamespace picks the namespace the canary lease lives in: the
// explicit flag wins, then the leader-election namespace, then the pod's
// namespace as the runtime reports it (POD_NAMESPACE, then the projected
// serviceaccount file). Empty means the caller should disable the canary and
// say why in its startup log.
func ResolveNamespace(flagNamespace, leaderElectionNamespace string) string {
	if flagNamespace != "" {
		return flagNamespace
	}
	if leaderElectionNamespace != "" {
		return leaderElectionNamespace
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return ""
}

// New validates the options and registers the informer event handler on the
// manager's cache. Call it before the manager starts: GetInformer on a
// not-yet-started cache registers the informer without waiting for sync, and
// the manager starts and syncs it as part of Start.
func New(o Options) (*Canary, error) {
	if o.Interval <= 0 {
		return nil, fmt.Errorf("watch canary interval must be positive, got %s", o.Interval)
	}
	if o.FailAfter <= o.Interval {
		return nil, fmt.Errorf("watch canary fail-after (%s) must be greater than the write interval (%s)", o.FailAfter, o.Interval)
	}
	if o.Namespace == "" {
		return nil, fmt.Errorf("watch canary namespace must not be empty")
	}
	if o.Reader == nil || o.Writer == nil {
		return nil, fmt.Errorf("watch canary needs a live reader and a writer client")
	}
	if o.Cache == nil {
		return nil, fmt.Errorf("watch canary needs the manager's cache to observe its own writes")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	log := o.Log
	if log.GetSink() == nil {
		log = logf.Log.WithName("watch-canary")
	}
	c := &Canary{opts: o, log: log}
	if err := c.watchInformer(); err != nil {
		return nil, err
	}
	return c, nil
}

// watchInformer registers the event handler that marks writes observed.
// lastObserved is advanced only by events for the canary lease itself, which
// is what makes the liveness check measure the same write -> watch -> cache
// pipeline the controllers depend on rather than the process being alive.
func (c *Canary) watchInformer() error {
	informer, err := c.opts.Cache.GetInformer(context.Background(), &coordinationv1.Lease{})
	if err != nil {
		return fmt.Errorf("get lease informer for the watch canary: %w", err)
	}
	_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    c.observed,
		UpdateFunc: func(_, new interface{}) { c.observed(new) },
		DeleteFunc: c.observed,
	})
	if err != nil {
		return fmt.Errorf("attach watch canary event handler: %w", err)
	}
	return nil
}

// NeedLeaderElection keeps the write loop on the leader only: with more than
// one replica only one pod renews the lease, and a standby that never writes
// must not fail its own liveness check.
func (c *Canary) NeedLeaderElection() bool { return true }

// Start runs the write loop until the manager's context ends.
func (c *Canary) Start(ctx context.Context) error {
	c.mu.Lock()
	c.started = true
	c.startedAt = c.now()
	c.mu.Unlock()
	c.log.Info("watch canary started",
		"namespace", c.opts.Namespace, "lease", LeaseName,
		"interval", c.opts.Interval.String(), "failAfter", c.opts.FailAfter.String())

	if err := c.write(ctx); err != nil {
		c.log.Error(err, "watch canary write failed")
	}
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.write(ctx); err != nil {
				c.log.Error(err, "watch canary write failed")
			}
		}
	}
}

// LivenessCheck fails when a canary write has gone unobserved for FailAfter,
// or when no event for the lease has ever arrived within FailAfter of start.
// Both mean the manager's watch pipeline is not delivering; the kubelet
// restarts the pod when the probe starts failing.
func (c *Canary) LivenessCheck() healthz.Checker {
	return func(_ *http.Request) error {
		now := c.now()
		err := c.check(now)
		c.mu.Lock()
		wasHealthy := c.healthy
		c.healthy = err == nil
		c.mu.Unlock()
		if err != nil && wasHealthy {
			c.log.Error(err, "watch canary failed the liveness check; the kubelet restarts this pod once the probe starts failing")
		}
		return err
	}
}

func (c *Canary) check(now time.Time) error {
	c.mu.Lock()
	started, startedAt, haveObserved, last := c.started, c.startedAt, c.haveObserved, c.lastObserved
	c.mu.Unlock()

	// Not leading (or not started yet): there is nothing to verify, and a
	// standby that never writes must stay live.
	if !started {
		return nil
	}
	if !haveObserved {
		if elapsed := now.Sub(startedAt); elapsed > c.opts.FailAfter {
			return fmt.Errorf(
				"watch canary has not observed a single event for its lease in %s (started %s ago): the manager's watch pipeline is not delivering",
				c.opts.FailAfter, elapsed.Truncate(time.Second))
		}
		return nil
	}
	if age := now.Sub(last); age > c.opts.FailAfter {
		return fmt.Errorf(
			"watch canary write not observed for %s (fail-after %s): the manager's watch pipeline is stalled",
			age.Truncate(time.Second), c.opts.FailAfter)
	}
	return nil
}

// write renews the canary lease. The read is live and the write direct, so
// the write path never depends on the very cache it is measuring; a lost
// race or a transient API-server error is one missed interval, repaired by
// the next one because every write starts from a fresh read.
func (c *Canary) write(ctx context.Context) error {
	now := c.now()
	lease := &coordinationv1.Lease{}
	err := c.opts.Reader.Get(ctx, client.ObjectKey{Namespace: c.opts.Namespace, Name: LeaseName}, lease)
	switch {
	case apierrors.IsNotFound(err):
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: c.opts.Namespace,
				Name:      LeaseName,
				Labels:    map[string]string{LabelKey: LabelValue},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(holderIdentity()),
				LeaseDurationSeconds: ptr.To(int32(canaryLeaseDurationSeconds)),
				RenewTime:            &metav1.MicroTime{Time: now},
			},
		}
		err = c.opts.Writer.Create(ctx, lease)
	case err != nil:
		// The read itself failed; report it below.
	default:
		// Adopt a same-name lease that lacks the label (one left by an
		// earlier operator or created by hand): the name is operator-reserved
		// in the operator's own namespace.
		if lease.Labels == nil {
			lease.Labels = map[string]string{}
		}
		lease.Labels[LabelKey] = LabelValue
		lease.Spec.HolderIdentity = ptr.To(holderIdentity())
		lease.Spec.RenewTime = &metav1.MicroTime{Time: now}
		err = c.opts.Writer.Update(ctx, lease)
	}
	if err != nil {
		metrics.RecordWatchCanaryWrite("error")
		return err
	}
	metrics.RecordWatchCanaryWrite("ok")
	c.mu.Lock()
	c.sawWrite = true
	c.mu.Unlock()
	return nil
}

// observed records one delivered event for the canary lease. Deletions count
// too: a delete event is also proof the pipeline moved, and the next write
// recreates the lease.
func (c *Canary) observed(obj interface{}) {
	object, ok := obj.(client.Object)
	if !ok {
		tombstone, tombOK := obj.(toolscache.DeletedFinalStateUnknown)
		if !tombOK {
			return
		}
		object, ok = tombstone.Obj.(client.Object)
		if !ok {
			return
		}
	}
	if object.GetName() != LeaseName || object.GetNamespace() != c.opts.Namespace {
		return
	}

	now := c.now()
	c.mu.Lock()
	c.haveObserved = true
	c.lastObserved = now
	wasUp := c.up
	c.up = c.up || c.sawWrite
	c.mu.Unlock()

	metrics.RecordWatchCanaryObservation(now)
	if c.up && !wasUp {
		metrics.SetWatchCanaryUp()
	}
}

// Started reports whether the write loop has begun (the process is the
// leader). Tests and the healthz check read it.
func (c *Canary) Started() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// Observed reports whether an event for the canary lease has been observed,
// and when the last one arrived. Tests read it.
func (c *Canary) Observed() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastObserved, c.haveObserved
}

func (c *Canary) now() time.Time { return c.opts.Now() }

func holderIdentity() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}
