package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"go.uber.org/zap"
)

func hfTestExecutor(t *testing.T, store string) *MetalExecutor {
	t.Helper()
	lg, _ := zap.NewDevelopment()
	return &MetalExecutor{modelStorePath: store, logger: lg.Sugar()}
}

// A gated repository 401s without a bearer token. This asserts the token
// reaches the first hop.
func TestDownloadFile_SendsBearerToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("weights"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.gguf")
	if err := hfTestExecutor(t, dir).downloadFile(t.Context(), srv.URL+"/model.gguf", dst, "hf_secret"); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got != "Bearer hf_secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer hf_secret")
	}
	if b, _ := os.ReadFile(dst); string(b) != "weights" {
		t.Errorf("body = %q", string(b))
	}
}

// No token means no header at all, rather than an empty one, so an ungated
// repository behaves exactly as it did before this change.
func TestDownloadFile_NoTokenSendsNoHeader(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		_, _ = w.Write([]byte("weights"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "m.gguf")
	if err := hfTestExecutor(t, dir).downloadFile(t.Context(), srv.URL+"/m.gguf", dst, ""); err != nil {
		t.Fatalf("download: %v", err)
	}
	if present {
		t.Error("Authorization header sent with no token")
	}
}

// The one that matters. Hugging Face answers a weights request with a redirect
// to its CDN on a different host; the token must not follow, or a credential
// for huggingface.co is handed to an unrelated origin. net/http strips it on a
// cross-host redirect, and this pins that behaviour so a future switch to a
// custom client or a CheckRedirect override cannot silently remove it.
func TestDownloadFile_TokenNotForwardedAcrossHosts(t *testing.T) {
	var cdnAuth string
	var cdnHit bool
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHit = true
		cdnAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("weights-from-cdn"))
	}))
	defer cdn.Close()

	var originAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, cdn.URL+"/blob", http.StatusFound)
	}))
	defer origin.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.gguf")
	if err := hfTestExecutor(t, dir).downloadFile(t.Context(), origin.URL+"/model.gguf", dst, "hf_secret"); err != nil {
		t.Fatalf("download: %v", err)
	}
	if originAuth != "Bearer hf_secret" {
		t.Errorf("origin Authorization = %q, want the token on the first hop", originAuth)
	}
	if !cdnHit {
		t.Fatal("redirect was not followed, so the test proves nothing")
	}
	if cdnAuth != "" {
		t.Errorf("token leaked across hosts: CDN saw %q", cdnAuth)
	}
	if b, _ := os.ReadFile(dst); string(b) != "weights-from-cdn" {
		t.Errorf("body = %q, want the redirected content", string(b))
	}
}

// Accepts both spellings the fetch path can actually download: a
// huggingface.co URL, and hf:// which downloadFile resolves to one before
// building the request (#1759).
func TestIsHFAuthHost(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"https://huggingface.co/org/repo/resolve/main/m.gguf", true},
		{"https://HuggingFace.CO/org/repo/resolve/main/m.gguf", true},
		{"https://www.huggingface.co/org/repo/resolve/main/m.gguf", true},
		{"https://WWW.HuggingFace.co/org/repo/resolve/main/m.gguf", true},
		{"http://huggingface.co/org/repo/resolve/main/m.gguf", true},
		{"hf://meta-llama/Llama-Guard-4-12B", true},
		{"hf://meta-llama/Llama-Guard-4-12B@abc123", true},
		{"https://huggingface.co.evil.example/org/repo/m.gguf", false},
		{"https://nothuggingface.co/org/repo/m.gguf", false},
		{"https://cdn.example.com/m.gguf", false},
		{"s3://models/org/repo/m.gguf", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isHFAuthHost(tc.source); got != tc.want {
			t.Errorf("isHFAuthHost(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// hf:// must reach the wire as its huggingface.co resolve URL, the same
// rewrite the init container's normalize_hf_source applies on the
// in-cluster path (#1759). Before that fix the metal downloader handed the
// raw hf:// source to net/http and every hf:// Model failed with
// `unsupported protocol scheme "hf"`. The resolver itself is a pure
// function pinned in pkg/hfsource; a unit test cannot dial huggingface.co,
// so the resolved host is swapped for a local server here and the asserted
// behavior is the request that actually goes out: path, token, and bytes.
// pinHFHost points the hf:// resolver at srvURL for the duration of the test,
// so the huggingface.co request the resolved source produces can be observed
// on a local server. A unit test cannot dial huggingface.co.
func pinHFHost(t *testing.T, srvURL string) {
	t.Helper()
	orig := hfNormalize
	hfNormalize = func(source string) string {
		return strings.Replace(orig(source), "https://huggingface.co/", srvURL+"/", 1)
	}
	t.Cleanup(func() { hfNormalize = orig })
}

func TestDownloadFile_HFSchemeResolvedBeforeRequest(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("weights-from-hf"))
	}))
	defer srv.Close()

	pinHFHost(t, srv.URL)

	dir := t.TempDir()
	dst := filepath.Join(dir, "m.gguf")
	if err := hfTestExecutor(t, dir).downloadFile(t.Context(), "hf://org/repo", dst, ""); err != nil {
		t.Fatalf("downloadFile(hf://org/repo): %v", err)
	}
	if gotPath != "/org/repo/resolve/main/" {
		t.Errorf("request path = %q, want /org/repo/resolve/main/", gotPath)
	}
	if b, _ := os.ReadFile(dst); string(b) != "weights-from-hf" {
		t.Errorf("body = %q, want the resolved download", string(b))
	}
}

// End to end through ensureModel: an hf:// Model with a sourceSecretRef must
// both resolve and authenticate. The token gate runs on the raw source, so
// isHFAuthHost has to accept hf:// here, not only the resolved URL.
func TestEnsureModel_HFSchemeSendsToken(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("weights-from-hf"))
	}))
	defer srv.Close()

	pinHFHost(t, srv.URL)

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hf-token", Namespace: "default"},
		Data:       map[string][]byte{"HF_TOKEN": []byte("hf_secret")},
	}

	tests := []struct {
		name      string
		secretRef *corev1.LocalObjectReference
		wantAuth  string
	}{
		{"with source secret", &corev1.LocalObjectReference{Name: "hf-token"}, "Bearer hf_secret"},
		{"without source secret", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPath, gotAuth = "", ""
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
			executor := NewMetalExecutor("/bin/llama-server", t.TempDir(), newNopLogger(),
				WithKubeClient("default", k8sClient, nil))

			dst, err := executor.ensureModel(t.Context(), "hf://org/repo", "hf-model", tc.secretRef)
			if err != nil {
				t.Fatalf("ensureModel(hf://org/repo): %v", err)
			}
			if gotPath != "/org/repo/resolve/main/" {
				t.Errorf("request path = %q, want /org/repo/resolve/main/", gotPath)
			}
			if tc.wantAuth == "" {
				if gotAuth != "" {
					t.Errorf("Authorization = %q, want no header without a secret", gotAuth)
				}
			} else if gotAuth != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", gotAuth, tc.wantAuth)
			}
			if b, _ := os.ReadFile(dst); string(b) != "weights-from-hf" {
				t.Errorf("body = %q, want the resolved download", string(b))
			}
		})
	}
}
