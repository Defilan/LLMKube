package controller

import (
	"strings"
	"testing"
)

// The operator's own downloads had no way to authenticate: only s3:// sources
// got credentials, so every gated or private Hugging Face repository failed
// with 401 for GGUF models, for spec.files staging, and for prefetch (#1750).
// These pin the two halves of the fix: the header is emitted for huggingface.co
// sources, and for nothing else.

func TestIsHFAuthSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   bool
	}{
		{"hf scheme", "hf://meta-llama/Llama-Guard-4-12B", true},
		{"hf scheme with revision", "hf://meta-llama/Llama-Guard-4-12B@abc123", true},
		{"hf scheme upper", "HF://meta-llama/Llama-Guard-4-12B", true},
		{"resolved https url", "https://huggingface.co/org/repo/resolve/main/model.gguf", true},
		{"host case folded", "https://HuggingFace.CO/org/repo/resolve/main/model.gguf", true},
		{"www prefix", "https://www.huggingface.co/org/repo/resolve/main/model.gguf", true},
		// The gate exists to keep the token off other hosts. A lookalike host is
		// the case that matters: a prefix match on "huggingface.co" without the
		// trailing slash would leak the token to an attacker-controlled domain.
		{"lookalike host", "https://huggingface.co.evil.example/org/repo/model.gguf", false},
		{"substring host", "https://nothuggingface.co/org/repo/model.gguf", false},
		{"other host", "https://cdn.example.com/model.gguf", false},
		{"s3 source", "s3://models/org/repo/model.gguf", false},
		{"local source", "/host-model/model.gguf", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHFAuthSource(tc.source); got != tc.want {
				t.Errorf("isHFAuthSource(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

// authHeader is the exact text a gated fetch must carry. Asserting the whole
// string rather than a substring keeps a malformed header (a missing space
// after "Bearer", say) from passing.
const authHeader = `-H "Authorization: Bearer ${HF_TOKEN}"`

func TestBuildModelInitCommand_HFAuth(t *testing.T) {
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		for _, useCache := range []bool{true, false} {
			t.Run(policy+"/cache="+boolStr(useCache), func(t *testing.T) {
				withAuth := buildModelInitCommand(false, false, useCache, true, policy)
				if !strings.Contains(withAuth, authHeader) {
					t.Errorf("HF source: no bearer header in:\n%s", withAuth)
				}
				if !strings.Contains(withAuth, "hf_curl()") {
					t.Error("HF source: the hf_curl definition is missing, so the script would fail with 'not found'")
				}
				// Defining the wrapper is not enough: the transfer has to call
				// it. A script that defines hf_curl and then runs plain curl
				// downloads unauthenticated and still contains every string a
				// laxer assertion would look for.
				if strings.Count(withAuth, "hf_curl ") == 0 {
					t.Errorf("HF source defines hf_curl but never invokes it:\n%s", withAuth)
				}
				for _, plainCall := range []string{"; curl -", "$(curl -", "if curl -"} {
					if strings.Contains(withAuth, plainCall) {
						t.Errorf("HF source still has an unauthenticated transfer (%q):\n%s", plainCall, withAuth)
					}
				}
				// --location-trusted would carry the token across the LFS redirect
				// to the CDN, which is exactly what must not happen.
				if strings.Contains(withAuth, "--location-trusted") {
					t.Error("--location-trusted sends the token to the redirect target")
				}

				plain := buildModelInitCommand(false, false, useCache, false, policy)
				if strings.Contains(plain, "HF_TOKEN") {
					t.Errorf("non-HF source leaks the token into:\n%s", plain)
				}
				if strings.Contains(plain, "hf_curl") {
					t.Error("non-HF source should call curl directly")
				}
			})
		}
	}
}

func TestBuildMultiFileInitCommand_HFAuth(t *testing.T) {
	for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
		t.Run(policy, func(t *testing.T) {
			withAuth := buildMultiFileInitCommand(true, false, true, policy)
			if !strings.Contains(withAuth, authHeader) {
				t.Errorf("HF multi-file: no bearer header in:\n%s", withAuth)
			}
			if !strings.Contains(withAuth, "hf_curl()") {
				t.Error("HF multi-file: hf_curl definition missing")
			}
			if strings.Count(withAuth, "hf_curl ") == 0 {
				t.Errorf("HF multi-file defines hf_curl but never invokes it:\n%s", withAuth)
			}
			for _, plainCall := range []string{"; curl -", "$(curl -", "if curl -"} {
				if strings.Contains(withAuth, plainCall) {
					t.Errorf("HF multi-file still has an unauthenticated transfer (%q):\n%s", plainCall, withAuth)
				}
			}

			plain := buildMultiFileInitCommand(true, false, false, policy)
			if strings.Contains(plain, "HF_TOKEN") {
				t.Errorf("non-HF multi-file leaks the token into:\n%s", plain)
			}

			// s3:// keeps signing with sigv4 and must not gain a bearer header
			// even when the source would otherwise qualify.
			s3 := buildMultiFileInitCommand(true, true, false, policy)
			if strings.Contains(s3, "HF_TOKEN") {
				t.Error("s3 multi-file should authenticate with sigv4 only")
			}
			if !strings.Contains(s3, "--aws-sigv4") {
				t.Error("s3 multi-file lost its sigv4 signing")
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
