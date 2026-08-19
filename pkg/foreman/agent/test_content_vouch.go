package agent

import (
	"path"
	"regexp"
	"strings"
)

// scopeMatchedViaContentKey is the extra key carrying refs satisfied by the
// content-based vouch rather than by name-based folding. A content vouch is
// recorded separately from scopeMatched so a match earned by reading a file's
// imports is distinguishable in the task record from one earned by path
// folding (#1605, #1606).
const scopeMatchedViaContentKey = "scopeMatchedViaContent"

// contentReferencesModule reports whether testContent imports or references
// the module named by modulePath. It is the deterministic content signal the
// scope-overlap rail falls back to when name-based folding (testTargetsForPath
// / testTargetsWithLayout) fails to match a feature- or behaviour-named test
// file: a test named for what it exercises (test_dedup.py,
// test_platform_gh_api_forgejo.py) does not fold to the module it covers, but
// its body imports that module, and the import is a computable, model-free
// proof of coverage (#1610).
//
// The match is on the module's path stem (its last segment with the
// extension removed), tolerant of package prefixes, so a Python test that
// does `import pr_reviewer.platform` or `from pr_reviewer.platform import x`
// vouches for a ref naming `platform.py`, and a JS test that does
// `import x from './util'` or `require('./util')` vouches for `util.ts`.
//
// Per-language reference patterns, applied to the stem:
//   - Python: `import <stem>` or `from <pkg>.<stem> import ...`
//   - Go:     an import path whose last path element is the stem
//   - JS/TS:  `import ... from '<prefix>/<stem>'` or `require('<stem>')`
//   - Ruby:   `require '<stem>'` or `require_relative '<stem>'`
//
// The check is deliberately conservative: it returns false on any input that
// does not clearly reference the module, because a false vouch defeats the
// rail the same way a false name match would. A different module's stem that
// merely shares a prefix (foo vs foo_bar) must not vouch.
func contentReferencesModule(testContent, modulePath string) bool {
	base := path.Base(modulePath)
	stem := base
	if ext := path.Ext(base); ext != "" {
		stem = strings.TrimSuffix(base, ext)
	}
	if stem == "" {
		return false
	}
	return contentReferencesStem(testContent, stem)
}

// contentReferencesStem runs the per-language import checks for one stem.
// Kept separate from contentReferencesModule so the language rules stay
// readable and so a future language can be added without touching the
// modulePath-to-stem derivation.
func contentReferencesStem(content, stem string) bool {
	// Python: `import <stem>` / `import <pkg>.<stem>`, or
	// `from <stem> import` / `from <pkg>.<stem> import`.
	// The trailing \b keeps `import foo` from claiming `foo_bar`.
	for _, re := range []string{
		`(?m)^\s*import\s+` + regexp.QuoteMeta(stem) + `\b`,
		`(?m)^\s*import\s+[\w.]*` + regexp.QuoteMeta(stem) + `(\.|$|\s)`,
		`(?m)^\s*from\s+` + regexp.QuoteMeta(stem) + `\b`,
		`(?m)^\s*from\s+[\w.]*\.` + regexp.QuoteMeta(stem) + `(\.|$|\s)`,
	} {
		if matched(re, content) {
			return true
		}
	}

	// JS/TS: `import ... from '<stem>'`, `import ... from '<pkg>/<stem>'`,
	// or `require('<stem>')` / `require('<pkg>/<stem>')`.
	for _, re := range []string{
		`(?m)from\s+['"][^'"]*/` + regexp.QuoteMeta(stem) + `['"]`,
		`(?m)from\s+['"]` + regexp.QuoteMeta(stem) + `['"]`,
		`(?m)require\s*\(\s*['"][^'"]*/` + regexp.QuoteMeta(stem) + `['"]`,
		`(?m)require\s*\(\s*['"]` + regexp.QuoteMeta(stem) + `['"]`,
	} {
		if matched(re, content) {
			return true
		}
	}

	// Ruby: `require '<stem>'` / `require '<pkg>/<stem>'` and the
	// require_relative variants.
	for _, re := range []string{
		`(?m)^\s*require(_relative)?\s+['"][^'"]*/` + regexp.QuoteMeta(stem) + `['"]`,
		`(?m)^\s*require(_relative)?\s+['"]` + regexp.QuoteMeta(stem) + `['"]`,
	} {
		if matched(re, content) {
			return true
		}
	}

	// Go: an import path whose last element is the stem. Go's import paths
	// end in the package directory, so a test in the same package references
	// the module by its directory name (the stem of `foo.go`), not `foo.go`
	// itself.
	for _, re := range []string{
		`(?m)^\s*(?:[\w.]+ )?"[^"]*/` + regexp.QuoteMeta(stem) + `"`,
		`(?m)^\s*(?:[\w.]+ )?"` + regexp.QuoteMeta(stem) + `"`,
	} {
		if matched(re, content) {
			return true
		}
	}
	return false
}

// matched reports whether the (precompiled-once) regex pattern matches
// content. Compiling per call is acceptable: the scope rail reads at most a
// handful of small test files per run, and keeping the helper stateless keeps
// the vouch a pure function of (content, module) with no package state.
func matched(pattern, content string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(content)
}
