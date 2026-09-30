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

package ingress

import (
	"bytes"
	"context"
	"crypto/subtle"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// SecretGetter is the subset of a controller-runtime client the TokenStore
// needs. The agent's client.Client satisfies it.
type SecretGetter interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
}

// TokenStore validates relay bearer tokens against the per-namespace
// llmkube-metal-relay Secret. Each namespace's token is cached for ttl; when
// a refresh observes a new value, the previous one stays valid until
// now()+grace so in-flight relays survive a rotation. Token values are never
// logged.
type TokenStore struct {
	c     SecretGetter
	ttl   time.Duration
	grace time.Duration
	now   func() time.Time
	// logger receives Warn logs for Secret read failures and unmanaged
	// Secrets; nil means no logs.
	logger *zap.SugaredLogger

	mu      sync.Mutex
	entries map[string]*tokenEntry
}

// tokenEntry is one namespace's cached token state. mu guards the fields
// but is never held across a Secret read: one caller at a time performs the
// read (refreshing), callers with cached state answer from it meanwhile, and
// only callers for a never-fetched entry wait on done.
type tokenEntry struct {
	mu          sync.Mutex
	fetched     bool          // a read has succeeded or returned NotFound
	refreshing  bool          // a read is in flight
	done        chan struct{} // closed when the in-flight read finishes
	nextRefresh time.Time     // zero until the first refresh attempt
	current     []byte
	previous    []byte
	prevUntil   time.Time
	// lastErr is the message of the last logged read failure, cleared by a
	// successful read, so a repeated identical failure is logged once.
	lastErr string
	// unmanaged is set while the last successful read found a Secret
	// without the controller's managed-by label, so the Warn is logged
	// once per change rather than on every refresh.
	unmanaged bool
}

const (
	// refreshTimeout bounds one Secret read. The read runs detached from
	// the caller's context so a client disconnect cannot poison the cache.
	refreshTimeout = 3 * time.Second
	// transientRetry is how soon, measured from the end of a failed read, a
	// refresh is retried after an error other than NotFound, instead of
	// waiting a full ttl.
	transientRetry = time.Second
)

// TokenStoreOption configures a TokenStore.
type TokenStoreOption func(*TokenStore)

// WithLogger makes the TokenStore log Secret read failures other than
// NotFound, and Secrets it ignores for lacking the controller's managed-by
// label, at Warn, once per namespace per transition (see apply). Token values
// are never logged.
func WithLogger(l *zap.SugaredLogger) TokenStoreOption {
	return func(s *TokenStore) { s.logger = l }
}

// NewTokenStore returns a TokenStore reading Secrets through c. now defaults
// to time.Now when nil.
func NewTokenStore(
	c SecretGetter, ttl, grace time.Duration, now func() time.Time, opts ...TokenStoreOption,
) *TokenStore {
	if now == nil {
		now = time.Now
	}
	s := &TokenStore{c: c, ttl: ttl, grace: grace, now: now, entries: map[string]*tokenEntry{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Valid reports whether presented is the current relay token for namespace,
// or the previous token within its rotation grace window. A missing Secret,
// an empty stored token, an empty presented token or a namespace that is not
// a valid DNS-1123 label all yield false.
//
// Callers must only pass namespaces the agent serves. An invalid namespace
// is rejected before the cache is touched (no entry, no Secret read), but
// every valid namespace gets a cache entry that is never evicted, so an
// unauthenticated caller must not be able to choose arbitrary valid names.
//
// While a refresh is in flight, callers for an entry that already has state
// answer from it immediately. Callers for a never-fetched entry wait for the
// in-flight read, or return false if their ctx ends first.
func (s *TokenStore) Valid(ctx context.Context, namespace, presented string) bool {
	if presented == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return false
	}
	e := s.entry(namespace)
	p := []byte(presented)

	e.mu.Lock()
	for {
		now := s.now()
		if now.Before(e.nextRefresh) || (e.refreshing && e.fetched) {
			ok := e.matches(p, now)
			e.mu.Unlock()
			return ok
		}
		if !e.refreshing {
			break
		}
		// Cold entry with a read in flight: wait for it, then re-evaluate.
		done := e.done
		e.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return false
		}
		e.mu.Lock()
	}

	e.refreshing = true
	e.done = make(chan struct{})
	e.mu.Unlock()

	res, err := s.read(ctx, namespace)

	e.mu.Lock()
	defer e.mu.Unlock()
	now := s.now() // after the read, so a slow read cannot leave nextRefresh already past
	s.apply(e, namespace, res, err, now)
	e.refreshing = false
	close(e.done)
	return e.matches(p, now)
}

// matches compares p against the current token and, within grace, the
// previous one. Caller holds e.mu.
func (e *tokenEntry) matches(p []byte, now time.Time) bool {
	if len(e.current) > 0 && subtle.ConstantTimeCompare(p, e.current) == 1 {
		return true
	}
	return len(e.previous) > 0 && now.Before(e.prevUntil) && subtle.ConstantTimeCompare(p, e.previous) == 1
}

func (s *TokenStore) entry(namespace string) *tokenEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[namespace]
	if !ok {
		e = &tokenEntry{}
		s.entries[namespace] = e
	}
	return e
}

// readResult is one Secret read: the trimmed token, and whether the Secret
// exists but lacks the controller's managed-by label (in which case token is
// nil).
type readResult struct {
	token     []byte
	unmanaged bool
}

// read fetches namespace's relay token under context.WithoutCancel(ctx)
// with its own refreshTimeout, so the answer does not depend on whether this
// particular caller has gone away. Surrounding whitespace is trimmed, as the
// relay trims its token file, so a Secret written with a trailing newline
// still matches. It returns a nil token for a missing Secret or an empty (or
// whitespace-only) key, and a non-nil error only for failures other than
// NotFound.
//
// A Secret without the controller's managed-by label counts as having no
// token (unmanaged is set). This is a safety net, not the control against a
// planted token (#1957): whoever creates the Secret can set the label too.
// It catches a Secret created before the chart's admission policy existed,
// or created by hand by mistake. The control is that policy, which lets only
// the controller create the Secret; without it (Kubernetes older than 1.30,
// or the policy disabled) the protection is RBAC on Secret create.
func (s *TokenStore) read(ctx context.Context, namespace string) (readResult, error) {
	getCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: inferencev1alpha1.MetalRelaySecretName}
	err := s.c.Get(getCtx, key, &secret)
	if apierrors.IsNotFound(err) {
		return readResult{}, nil
	}
	if err != nil {
		return readResult{}, err
	}
	if secret.Labels[inferencev1alpha1.LabelManagedBy] != inferencev1alpha1.ManagedByController {
		return readResult{unmanaged: true}, nil
	}
	return readResult{token: bytes.TrimSpace(secret.Data[inferencev1alpha1.MetalRelaySecretKey])}, nil
}

// apply folds a read result into e. Caller holds e.mu; now is read after the
// read returned.
//
// A successful read or NotFound is cached for ttl, so a flood of requests
// costs at most one Get per ttl per namespace. A missing Secret, an empty
// token or an unmanaged Secret (no controller managed-by label) revokes both
// current and previous; an unmanaged Secret is logged at Warn once each time
// it is first seen, without its token. Any other read error (API server
// unreachable, timeout, RBAC) keeps the last-known tokens until a later read
// succeeds, and retries transientRetry after the failed read ended rather
// than a full ttl. With no last-known tokens this is fail-closed. Such an
// error is logged at Warn when it differs from the last one logged for the
// namespace (the first failure after a success, or a changed message); the
// token values are never logged.
func (s *TokenStore) apply(e *tokenEntry, namespace string, res readResult, err error, now time.Time) {
	if err != nil {
		e.nextRefresh = now.Add(transientRetry)
		if msg := err.Error(); msg != e.lastErr {
			e.lastErr = msg
			if s.logger != nil {
				s.logger.Warnw("failed to read the relay token Secret; keeping the last known tokens",
					"namespace", namespace, "secret", inferencev1alpha1.MetalRelaySecretName, "error", msg)
			}
		}
		return
	}
	e.lastErr = ""
	e.fetched = true
	e.nextRefresh = now.Add(s.ttl)
	if res.unmanaged && !e.unmanaged && s.logger != nil {
		s.logger.Warnw("ignoring the relay token Secret: it lacks the controller's managed-by label, "+
			"so the controller did not create it; relays in this namespace get 401 until it is deleted "+
			"and the controller recreates it",
			"namespace", namespace, "secret", inferencev1alpha1.MetalRelaySecretName,
			"label", inferencev1alpha1.LabelManagedBy+"="+inferencev1alpha1.ManagedByController)
	}
	e.unmanaged = res.unmanaged
	fresh := res.token
	if len(fresh) == 0 {
		e.current, e.previous, e.prevUntil = nil, nil, time.Time{}
		return
	}
	if len(e.current) > 0 && subtle.ConstantTimeCompare(fresh, e.current) != 1 {
		e.previous = e.current
		e.prevUntil = now.Add(s.grace)
	}
	e.current = append([]byte(nil), fresh...)
}
