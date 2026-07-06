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

// Package cachekey provides the single source of truth for deriving a model's
// cache key from its spec.source. Both the controller and the CLI consult this
// package so `serve`, `cache list`, and `delete --purge-cache` can never
// disagree about which directory on the cache PVC owns a given model.
//
// The controller's effectiveModelCacheKey() additionally scopes the fallback
// to non-metal multi-file models; that scoping lives in the controller package
// and delegates to Compute() for the unconditional SHA256 fingerprint.
package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
)

// Compute returns a stable, short fingerprint of source. It is the single
// function the controller and CLI agree on when they need to derive a cache
// key from a model source URL (or local path) without consulting status.
//
// The output is the first 16 hex characters of SHA-256(source), matching the
// historical convention used by both the controller and the CLI before this
// package existed. Keeping the prefix short keeps PVC directory names
// manageable while the full 64-char digest is still recoverable by callers
// that need it.
func Compute(source string) string {
	hash := sha256.Sum256([]byte(source))
	return hex.EncodeToString(hash[:])[:16]
}
