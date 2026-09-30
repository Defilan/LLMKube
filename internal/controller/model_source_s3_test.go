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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// newModelReconcilerWithStatus builds a reconciler whose fake client supports
// the status subresource, which reconcileBySourceType needs because every
// terminal path there writes status via r.Status().Update.
func newModelReconcilerWithStatus(t *testing.T, objects ...client.Object) *ModelReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&inferencev1alpha1.Model{}).
		Build()
	return &ModelReconciler{Client: c, Scheme: scheme}
}

// An s3:// Model must be routed to the runtime-resolved path, exactly like a
// remote http(s) source: the InferenceService Pod's init container does the
// fetch with a sigv4-signed curl, using credentials from sourceSecretRef that
// the controller never resolves.
//
// Before this was wired, s3:// matched no case in reconcileBySourceType and
// fell through to the controller-side eager fetch, where net/http rejected the
// scheme outright:
//
//	failed to download: Get "s3://bucket/key": unsupported protocol scheme "s3"
//
// The model never left Downloading, so no Pod was ever created and the
// perfectly good init-container S3 support in model_storage.go never ran.
func TestReconcileBySourceType_S3IsRuntimeResolved(t *testing.T) {
	const source = "s3://models/org/repo/model-Q4_K_M.gguf"

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          source,
			SourceSecretRef: &corev1.LocalObjectReference{Name: "minio-models"},
		},
	}
	r := newModelReconcilerWithStatus(t, model)

	handled, _, err := r.reconcileBySourceType(context.Background(), model)
	if err != nil {
		t.Fatalf("reconcileBySourceType: %v", err)
	}
	if !handled {
		t.Fatal("s3:// source fell through to the controller-side download path; " +
			"net/http cannot fetch an s3:// URL, so the Model can only fail there")
	}
	if model.Status.Phase != PhaseReady {
		t.Errorf("phase = %q, want %q", model.Status.Phase, PhaseReady)
	}
	// A cache key is what points the storage builder at the shared cache PVC
	// rather than an emptyDir; without it every Pod re-downloads the weights,
	// which is the whole thing an object store exists to avoid.
	if want := computeCacheKey(source); model.Status.CacheKey != want {
		t.Errorf("cacheKey = %q, want %q", model.Status.CacheKey, want)
	}
}

// The controller must not try to fetch an s3:// source itself. This pins the
// classification the dispatch depends on, independent of the switch above.
func TestS3SourceIsNotClassifiedAsFetchableByController(t *testing.T) {
	const source = "s3://models/org/repo/model.gguf"

	if !isS3Source(source) {
		t.Fatalf("isS3Source(%q) = false", source)
	}
	if isLocalSource(source) {
		t.Errorf("isLocalSource(%q) = true; fetchModel would try a filesystem copy", source)
	}
	if isRemoteHTTPSource(source) {
		t.Errorf("isRemoteHTTPSource(%q) = true; the controller would attempt an HTTP GET", source)
	}
}

// The sigv4 signer must attach a valid AWS Signature Version 4 Authorization
// header to every request it forwards, so an s3:// object store accepts the
// controller's metadata read. This exercises sigv4RoundTripper.RoundTrip
// directly (the in-process equivalent of the init container's signed curl) and
// fails if the signer stops producing a signed request.
func TestSigV4RoundTripperSignsRequest(t *testing.T) {
	var gotAuth, gotDate, gotPayloadHash string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		gotDate = req.Header.Get("x-amz-date")
		gotPayloadHash = req.Header.Get("x-amz-content-sha256")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	signer := &sigv4RoundTripper{
		base:      base,
		endpoint:  &url.URL{Scheme: "http", Host: "minio.local"},
		accessKey: "AKIAEXAMPLE",
		secretKey: "secret",
		region:    "us-east-1",
	}

	req, err := http.NewRequest(http.MethodGet, "http://minio.local/models/org/repo/model.gguf", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := signer.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256 with access key", gotAuth)
	}
	if !strings.Contains(gotAuth, "/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization = %q, want region/service scope us-east-1/s3", gotAuth)
	}
	if !strings.Contains(gotAuth, "Signature=") {
		t.Errorf("Authorization = %q, want a Signature", gotAuth)
	}
	if gotDate == "" {
		t.Error("x-amz-date header not set")
	}
	if gotPayloadHash == "" {
		t.Error("x-amz-content-sha256 header not set")
	}
}

// roundTripFunc adapts a func to http.RoundTripper for tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// sigv4HeadersIn lists the headers the signer adds that hdr carries.
func sigv4HeadersIn(hdr http.Header) []string {
	var found []string
	for k := range hdr {
		if strings.EqualFold(k, "Authorization") || strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			found = append(found, k)
		}
	}
	return found
}

// testS3Client builds the controller's s3 client for endpoint through the
// SSRF-guarded metadata transport, with loopback allowlisted for httptest.
func testS3Client(t *testing.T, endpoint string) *http.Client {
	t.Helper()
	r := &ModelReconciler{AllowedRemoteHosts: []string{"127.0.0.1"}}
	c, err := r.s3Client(s3Creds{
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "secret",
		Region:          "us-east-1",
		Endpoint:        endpoint,
	})
	if err != nil {
		t.Fatalf("s3Client: %v", err)
	}
	return c
}

// An s3 endpoint that 307-redirects GET and HEAD to another host must not
// hand that host a SigV4 signature: the redirected requests carry neither
// Authorization nor any x-amz-* header, and the object is still read from
// the second server (#1955).
func TestS3ClientDoesNotSignCrossHostRedirect(t *testing.T) {
	var secondHeaders []http.Header
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHeaders = append(secondHeaders, r.Header.Clone())
		w.Header().Set("Content-Length", "15")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("presigned-bytes"))
		}
	}))
	defer other.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.Redirect(w, r, other.URL+"/presigned/model.gguf?X-Amz-Signature=abc", http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()

	c := testS3Client(t, endpoint.URL)
	objectURL := endpoint.URL + "/models/org/model.gguf"
	resp, err := c.Get(objectURL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "presigned-bytes" {
		t.Errorf("GET = %d %q, want 200 from the second server", resp.StatusCode, body)
	}
	if size := (&ModelReconciler{}).s3ContentLength(context.Background(), c, objectURL); size != 15 {
		t.Errorf("HEAD size = %d, want 15 from the second server", size)
	}

	if len(secondHeaders) != 2 {
		t.Fatalf("second server saw %d requests, want a GET and a HEAD", len(secondHeaders))
	}
	for i, hdr := range secondHeaders {
		if found := sigv4HeadersIn(hdr); len(found) > 0 {
			t.Errorf("cross-host redirect hop %d carried signing headers %v", i, found)
		}
	}
}

// A redirect to another path on the endpoint is still signed, as is a plain
// request.
func TestS3ClientSignsSameHostRedirect(t *testing.T) {
	var signedPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") ||
			r.Header.Get("x-amz-date") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		signedPaths = append(signedPaths, r.URL.Path)
		if r.URL.Path == "/models/old.gguf" {
			http.Redirect(w, r, "/models/new.gguf", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := testS3Client(t, srv.URL)
	for _, path := range []string{"/models/plain.gguf", "/models/old.gguf"} {
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (every hop signed)", path, resp.StatusCode)
		}
	}
	want := []string{"/models/plain.gguf", "/models/old.gguf", "/models/new.gguf"}
	if strings.Join(signedPaths, ",") != strings.Join(want, ",") {
		t.Errorf("signed paths = %v, want %v", signedPaths, want)
	}
}

// The signer passes a request to any other origin through untouched, and does
// not write its headers into the caller's request.
func TestSigV4RoundTripperSignsOnlyItsEndpoint(t *testing.T) {
	var got http.Header
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	signer := &sigv4RoundTripper{
		base:      base,
		endpoint:  &url.URL{Scheme: "https", Host: "MinIO.lan"},
		accessKey: "AKIAEXAMPLE",
		secretKey: "secret",
		region:    "us-east-1",
	}
	for _, tc := range []struct {
		url  string
		sign bool
	}{
		{"https://minio.lan:443/models/m.gguf", true},
		{"https://minio.lan:9000/models/m.gguf", false},
		{"http://minio.lan/models/m.gguf", false},
		{"https://elsewhere.example/models/m.gguf?X-Amz-Signature=x", false},
	} {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := signer.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip(%s): %v", tc.url, err)
		}
		_ = resp.Body.Close()
		if signed := got.Get("Authorization") != ""; signed != tc.sign {
			t.Errorf("%s: signed = %v, want %v", tc.url, signed, tc.sign)
		}
		if !tc.sign && len(sigv4HeadersIn(got)) > 0 {
			t.Errorf("%s: unsigned request carried %v", tc.url, sigv4HeadersIn(got))
		}
		if len(sigv4HeadersIn(req.Header)) > 0 {
			t.Errorf("%s: signer wrote %v into the caller's request", tc.url, sigv4HeadersIn(req.Header))
		}
	}
}

// A malformed or relative endpoint is refused instead of producing a signer
// that matches nothing.
func TestS3ClientRejectsRelativeEndpoint(t *testing.T) {
	if _, err := (&ModelReconciler{}).s3Client(s3Creds{Endpoint: "minio.lan:9000"}); err == nil {
		t.Error("s3Client accepted an endpoint without a scheme and host")
	}
}
