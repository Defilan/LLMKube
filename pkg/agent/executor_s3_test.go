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

package agent

import (
	"fmt"
	"io"
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
)

// An s3:// source must be signed with AWS SigV4, never sent as a plain GET.
//
// Regression for #1449/#1667: before this fix the metal fetch path handed the
// raw s3:// source string to a plain net/http GET, which cannot sign the
// request, so a private MinIO returns 403. This test points AWS_ENDPOINT_URL at
// a real test server standing in for MinIO and asserts the request the executor
// makes carries a SigV4 Authorization header scoped to the s3 service. A plain
// GET (the buggy behavior) carries no Authorization header, so this fails
// without the fix.
func TestEnsureModel_S3UsesSigV4(t *testing.T) {
	tmpDir := t.TempDir()

	// Stand-in for the S3/MinIO endpoint. It rejects unsigned requests the way
	// a real bucket does, and answers signed ones with the object bytes.
	var gotAuth, gotDate, gotPayloadHash, gotURL string
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("x-amz-date")
		gotPayloadHash = r.Header.Get("x-amz-content-sha256")
		gotURL = r.URL.String()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			w.WriteHeader(http.StatusForbidden) // anonymous GET refused
			return
		}
		body := []byte("fake-gguf-bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(capture)
	defer srv.Close()

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}

	// The secret the executor must resolve. The endpoint points at the test
	// server, standing in for the MinIO endpoint.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-models", Namespace: "default"},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(srv.URL),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil))

	modelDir := filepath.Join(tmpDir, "s3-model")
	path, err := executor.ensureModel(
		t.Context(),
		"s3://models/org/repo/model-Q4_K_M.gguf",
		"s3-model",
		&corev1.LocalObjectReference{Name: "minio-models"},
	)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}

	// The downloaded file must land at the expected path.
	want := filepath.Join(modelDir, "model-Q4_K_M.gguf")
	if path != want {
		t.Errorf("ensureModel path = %q, want %q", path, want)
	}

	// The request must carry a SigV4 Authorization header scoped to s3.
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE0000000/") {
		t.Errorf("Authorization = %q, want an AWS4-HMAC-SHA256 header with the access key", gotAuth)
	}
	if !strings.Contains(gotAuth, "/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization = %q, want a us-east-1/s3 scope", gotAuth)
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

	// The object URL must be the path-style endpoint <endpoint>/<bucket>/<key>,
	// matching the controller and init-container shape.
	if !strings.HasSuffix(gotURL, "/models/org/repo/model-Q4_K_M.gguf") {
		t.Errorf("request URL = %q, want a path-style <endpoint>/models/org/repo/... object URL", gotURL)
	}

	// The body must have been written to disk.
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read downloaded model: %v", err)
	}
	if string(got) != "fake-gguf-bytes" {
		t.Errorf("downloaded body = %q, want %q", string(got), "fake-gguf-bytes")
	}
}

// A missing sourceSecretRef must fail clearly rather than fall through to an
// anonymous GET that 403s confusingly.
func TestEnsureModel_S3MissingSecretRefFails(t *testing.T) {
	tmpDir := t.TempDir()

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil))

	_, err := executor.ensureModel(
		t.Context(),
		"s3://models/org/repo/model.gguf",
		"s3-model",
		nil, // no sourceSecretRef
	)
	if err == nil {
		t.Fatal("ensureModel with an s3:// source and no sourceSecretRef should fail, not fall through to an anonymous GET")
	}
	if !strings.Contains(err.Error(), "sourceSecretRef") {
		t.Errorf("error = %q, want it to mention sourceSecretRef", err.Error())
	}
}

// An s3:// source whose secret is missing the access keys must fail clearly.
func TestEnsureModel_S3IncompleteSecretFails(t *testing.T) {
	tmpDir := t.TempDir()

	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "partial-creds", Namespace: "default"},
		Data: map[string][]byte{
			// Access key present, secret key missing.
			"AWS_ACCESS_KEY_ID": []byte("AKIAEXAMPLE0000000"),
			"AWS_REGION":        []byte("us-east-1"),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(),
		WithKubeClient("default", k8sClient, nil))

	_, err := executor.ensureModel(
		t.Context(),
		"s3://models/org/repo/model.gguf",
		"s3-model",
		&corev1.LocalObjectReference{Name: "partial-creds"},
	)
	if err == nil {
		t.Fatal("ensureModel with an s3:// source and an incomplete secret should fail")
	}
	if !strings.Contains(err.Error(), "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("error = %q, want it to name the missing AWS_SECRET_ACCESS_KEY", err.Error())
	}
}

// isS3Source / parseS3Source must classify and split correctly, matching the
// controller helpers they mirror.
func TestS3SourceClassification(t *testing.T) {
	cases := []struct {
		source       string
		wantIsS3     bool
		wantBucket   string
		wantKey      string
		wantParseErr bool
	}{
		{"s3://bucket/key/model.gguf", true, "bucket", "key/model.gguf", false},
		{"s3://models/org/repo/model-Q4_K_M.gguf", true, "models", "org/repo/model-Q4_K_M.gguf", false},
		{"S3://bucket/key", true, "bucket", "key", false}, // case-folded
		{"https://bucket/key", false, "", "", true},
		{"s3://bucket", true, "", "", true}, // missing key
		{"s3://", true, "", "", true},       // empty
	}
	for _, tc := range cases {
		if got := isS3Source(tc.source); got != tc.wantIsS3 {
			t.Errorf("isS3Source(%q) = %v, want %v", tc.source, got, tc.wantIsS3)
		}
		bucket, key, err := parseS3Source(tc.source)
		if tc.wantParseErr {
			if err == nil {
				t.Errorf("parseS3Source(%q) = nil error, want error", tc.source)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseS3Source(%q) error: %v", tc.source, err)
			continue
		}
		if bucket != tc.wantBucket || key != tc.wantKey {
			t.Errorf("parseS3Source(%q) = (%q,%q), want (%q,%q)", tc.source, bucket, key, tc.wantBucket, tc.wantKey)
		}
	}
}

// The sigv4 round-tripper must attach a valid AWS SigV4 Authorization header to
// every request (the in-process equivalent of the init container's signed
// curl). This exercises sigv4RoundTripper.RoundTrip directly.
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
