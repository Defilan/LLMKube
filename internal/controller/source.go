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
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	hfsource "github.com/defilantech/llmkube/pkg/hfsource"
)

// isUnrecoverableFetchError reports whether err is the kind of failure that
// will not heal by retrying without operator intervention. Used by the Model
// reconciler to short-circuit the rate-limited tight-retry path when the
// problem is a missing or unreachable source path rather than a transient
// cluster condition.
//
// The canonical example is the hot-spin from #405: a file:// source pointing
// at a path that exists on the metal-agent's host filesystem but not inside
// the controller pod. Returning an error from Reconcile in that case made
// controller-runtime spin on the rate-limited workqueue, pinning a CPU core
// for hours. Treating these errors as "stop returning err to the runtime, do
// periodic recheck on RequeueAfter instead" keeps the operator log honest
// and the CPU floor flat.
//
// Recognized terminal errors:
//
//   - fs.ErrNotExist: the path does not exist on the controller pod's
//     filesystem. Common in hybrid topologies (in-cluster controller + host
//     agent) where the user references a host path that is correct for the
//     agent but invisible to the controller.
//   - fs.ErrPermission: the controller cannot read the file. This is also
//     unrecoverable without operator action (chmod / chown / SELinux).
func isUnrecoverableFetchError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

// isPVCSource returns true if the source uses the pvc:// scheme.
func isPVCSource(source string) bool {
	return strings.HasPrefix(source, "pvc://")
}

// isS3Source reports whether source is an s3:// URL. Case-folded to agree
// with the other scheme classifiers (GHSA-jw3m-8q7m-f35r).
func isS3Source(source string) bool {
	return hasSchemeFold(source, "s3://")
}

// parseS3Source splits s3://bucket/key into bucket and key. Endpoint,
// region, and credentials are NOT in the URL; they come from the
// sourceSecretRef env (AWS_ENDPOINT_URL, AWS_REGION, AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY). Mirrors parsePVCSource error handling.
func parseS3Source(source string) (bucket, key string, err error) {
	if !isS3Source(source) {
		return "", "", fmt.Errorf("not an S3 source: %s", source)
	}

	// Strip the s3:// prefix
	rest := strings.TrimPrefix(source, "s3://")
	if rest == "" {
		return "", "", fmt.Errorf("empty S3 source: %s", source)
	}

	// Split into bucket and key
	slashIdx := strings.Index(rest, "/")
	if slashIdx < 0 {
		return "", "", fmt.Errorf("S3 source must include a key: %s (expected s3://bucket/key)", source)
	}

	bucket = rest[:slashIdx]
	key = rest[slashIdx+1:]

	if bucket == "" {
		return "", "", fmt.Errorf("S3 source has empty bucket: %s", source)
	}
	if key == "" {
		return "", "", fmt.Errorf("S3 source has empty key: %s", source)
	}

	return bucket, key, nil
}

// parsePVCSource extracts the PVC claim name and file path from a pvc:// source.
// Format: pvc://claim-name/path/to/model.gguf
func parsePVCSource(source string) (claimName, path string, err error) {
	if !isPVCSource(source) {
		return "", "", fmt.Errorf("not a PVC source: %s", source)
	}

	// Strip the pvc:// prefix
	rest := strings.TrimPrefix(source, "pvc://")
	if rest == "" {
		return "", "", fmt.Errorf("empty PVC source: %s", source)
	}

	// Split into claim name and path
	slashIdx := strings.Index(rest, "/")
	if slashIdx < 0 {
		return "", "", fmt.Errorf("PVC source must include a file path: %s (expected pvc://claim-name/path/to/model.gguf)", source)
	}

	claimName = rest[:slashIdx]
	path = rest[slashIdx+1:]

	if claimName == "" {
		return "", "", fmt.Errorf("PVC source has empty claim name: %s", source)
	}
	if path == "" {
		return "", "", fmt.Errorf("PVC source has empty file path: %s", source)
	}

	return claimName, path, nil
}

// The HF source-resolution family (hasSchemeFold, isHuggingFaceURL,
// isHFAuthSource, isHuggingFaceFileURL, extractHFRepoFromURL, parseHFSource,
// normalizeHFSource) moved to
// pkg/hfsource in #1759 so the Metal agent could resolve hf:// the same way;
// these wrappers keep the call sites and tests unchanged.

func hasSchemeFold(source, prefix string) bool {
	return hfsource.HasSchemeFold(source, prefix)
}

func isHuggingFaceURL(source string) bool {
	return hfsource.IsHuggingFaceURL(source)
}

func isHFAuthSource(source string) bool {
	return hfsource.IsHFAuthSource(source)
}

func isHuggingFaceFileURL(source string) bool {
	return hfsource.IsHuggingFaceFileURL(source)
}

func extractHFRepoFromURL(source string) (repoID, revision string, ok bool) {
	return hfsource.ExtractHFRepoFromURL(source)
}

func parseHFSource(source string) (repoID, revision string, err error) {
	return hfsource.ParseHFSource(source)
}

func normalizeHFSource(source string) string {
	return hfsource.NormalizeHFSource(source)
}

// isLocalSource returns true if the source is a local file (file:// URL or
// absolute path). The file:// scheme matches case-insensitively so a
// case-variant local source cannot bypass the hostPath allowlist check in
// validateLocalSourceAllowed by dodging classification.
func isLocalSource(source string) bool {
	return hasSchemeFold(source, "file://") || strings.HasPrefix(source, "/")
}

// getLocalPath extracts the filesystem path from a local source. The scheme
// strip is case-insensitive to stay in agreement with isLocalSource.
func getLocalPath(source string) string {
	if hasSchemeFold(source, "file://") {
		return source[len("file://"):]
	}
	return source
}

// validateLocalSourceAllowed enforces the host-path allowlist for local model
// sources. Non-local sources (https, pvc, hf) are validated elsewhere and pass
// through as nil here. A local source (absolute path or file:// URI) is allowed
// only when its cleaned absolute path lies within one of allowedRoots. An empty
// allowedRoots disables local/hostPath sources entirely, which is the secure
// default (see GHSA-jw3m-8q7m-f35r).
//
// The check is lexical (filepath.Clean), so ".." escapes are rejected; it does
// NOT resolve symlinks, so an operator who allowlists a root is trusting the
// contents of that root not to symlink elsewhere. Roots that are empty or not
// absolute are ignored.
func validateLocalSourceAllowed(source string, allowedRoots []string) error {
	if !isLocalSource(source) {
		return nil
	}
	p := getLocalPath(source)
	if !filepath.IsAbs(p) {
		return fmt.Errorf("local model source must be an absolute path: %q", source)
	}
	clean := filepath.Clean(p)
	allowed := false
	for _, root := range allowedRoots {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		r := filepath.Clean(root)
		if clean == r || strings.HasPrefix(clean, r+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		if hasNoUsableRoot(allowedRoots) {
			return fmt.Errorf("local/hostPath model sources are disabled: no allowed roots configured "+
				"(set modelSource.allowedHostPathRoots); refusing source %q (GHSA-jw3m-8q7m-f35r)", source)
		}
		return fmt.Errorf("local model source %q is not within any allowed root %v (GHSA-jw3m-8q7m-f35r)", source, allowedRoots)
	}
	return nil
}

// hasNoUsableRoot reports whether allowedRoots contains no usable (non-empty,
// absolute) entry, so the error message can distinguish "feature disabled" from
// "path outside configured roots".
func hasNoUsableRoot(allowedRoots []string) bool {
	for _, root := range allowedRoots {
		if root != "" && filepath.IsAbs(root) {
			return false
		}
	}
	return true
}

// isRemoteHTTPSource reports whether source is an http:// or https:// URL.
// These sources are downloaded by the inference Pod's init container into the
// per-namespace model cache PVC, not by the Model controller. Downloading in
// the controller's pod writes to the operator-namespace PVC, which is not
// visible to Pods in user namespaces (PVCs cannot be cross-namespace mounted),
// so the controller defers the actual fetch to the workload.
//
// The scheme matches case-insensitively ("HTTP://..." is remote): url.Parse
// lowercases schemes, so http.Client would happily fetch a case-variant URL
// that a case-sensitive classifier had failed to route to the guarded
// remote-source path (GHSA-jw3m-8q7m-f35r).
func isRemoteHTTPSource(source string) bool {
	// A huggingface.co URL that is NOT a single-file download (a landing page,
	// /tree/<rev>, a revision-pinned repo root, or a datasets/spaces page) is a
	// runtime-resolved or non-model source, not a remote HTTP file. Only a
	// huggingface.co URL that names a specific FILE
	// (/resolve|blob/<rev>/<file>) is a single-file HTTP download and stays
	// classified here.
	if isHuggingFaceURL(source) && !isHuggingFaceFileURL(source) {
		return false
	}
	return hasSchemeFold(source, "https://") || hasSchemeFold(source, "http://")
}

// hfServeArg returns the model argument a runtime (vLLM/TGI/SGLang) should
// receive for an HF-repo source: the bare "org/name" repo id, with "@rev"
// appended when a revision is pinned. This is identical to what the bare
// "org/name" source form already yields, so hf:// sources and huggingface.co
// repo URLs serve the same way the bare form does.
//
// Why not normalizeHFSource here: that returns a
// "https://huggingface.co/org/repo/resolve/<rev>/" download URL, which is
// correct for the init-container download path but is rejected by
// vLLM/TGI/SGLang ("Repo id must be in the form 'namespace/repo_name'"). The
// serve path needs the repo id, the download path needs the URL, so they use
// different helpers. Non-HF-repo sources (local paths, s3://, direct file URLs)
// pass through unchanged.
func hfServeArg(source string) string {
	if !isHFRepoSource(source) {
		return source
	}
	repoID, revision, err := parseHFSource(source)
	if err != nil || repoID == "" {
		return source
	}
	if revision != "" {
		return repoID + "@" + revision
	}
	return repoID
}

// validateHFRepoSource checks for common HF source mistakes and returns an
// error if the source is malformed. Now accepts @rev syntax and validates
// the revision is well-formed. Also validates huggingface.co URLs.
func validateHFRepoSource(source string) error {
	if !strings.HasPrefix(strings.ToLower(source), "hf://") && !isHuggingFaceURL(source) {
		return nil
	}
	_, _, err := parseHFSource(source)
	return err
}

// isHFRepoSource reports whether source looks like a HuggingFace repo ID
// (e.g., "TinyLlama/TinyLlama-1.1B-Chat-v1.0", "Qwen/Qwen3.6-35B-A3B")
// or an hf://-prefixed repo ID (e.g., "hf://org/repo").
// These sources are downloaded by the runtime (vLLM) at startup, not by
// the Model controller.
//
// Criteria:
//
//	Not a URL (no "://" scheme other than hf://)
//	Not an absolute path (doesn't start with "/")
//	Not a PVC source (handled separately)
//	Contains at least one "/" separator (HF convention: owner/repo)
//	Matches Hugging Face's permitted character set
func isHFRepoSource(source string) bool {
	if source == "" {
		return false
	}
	if isPVCSource(source) {
		return false
	}
	if isS3Source(source) {
		return false
	}
	if isLocalSource(source) {
		return false
	}
	if isHuggingFaceURL(source) {
		_, _, ok := extractHFRepoFromURL(source)
		return ok
	}
	if isRemoteHTTPSource(source) {
		return false
	}
	// Strip hf:// prefix if present for validation
	checkSource := strings.TrimPrefix(strings.ToLower(source), "hf://")
	if !strings.Contains(checkSource, "/") {
		return false
	}
	// Match HF's permitted character set: alphanumeric, hyphens, underscores,
	// dots, forward slashes, and @ (for revision). Must start with alphanumeric.
	for i, c := range checkSource {
		if i == 0 {
			if !isAlphaNum(c) {
				return false
			}
			continue
		}
		if !isAlphaNum(c) && c != '-' && c != '_' && c != '.' && c != '/' && c != '@' {
			return false
		}
	}
	return true
}

func isAlphaNum(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	}
	return false
}
