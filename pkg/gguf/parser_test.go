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

package gguf

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test data builders
// ---------------------------------------------------------------------------

// writeLE wraps binary.Write for test helpers; panics on error
// (bytes.Buffer.Write never fails, but errcheck requires handling).
func writeLE(buf *bytes.Buffer, data interface{}) {
	if err := binary.Write(buf, binary.LittleEndian, data); err != nil {
		panic(fmt.Sprintf("binary.Write failed: %v", err))
	}
}

type testValue interface {
	writeWithTag(buf *bytes.Buffer)
	writeData(buf *bytes.Buffer)
	typeTag() uint32
}

type testString struct{ s string }
type testUint32 struct{ v uint32 }
type testBool struct{ v bool }
type testArray struct{ elements []testValue }

func (t testString) typeTag() uint32 { return 8 }
func (t testUint32) typeTag() uint32 { return 4 }
func (t testBool) typeTag() uint32   { return 7 }
func (t testArray) typeTag() uint32  { return 9 }

func (t testString) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(8))
	writeGGUFString(buf, t.s)
}
func (t testString) writeData(buf *bytes.Buffer) {
	writeGGUFString(buf, t.s)
}

func (t testUint32) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(4))
	writeLE(buf, t.v)
}
func (t testUint32) writeData(buf *bytes.Buffer) {
	writeLE(buf, t.v)
}

func (t testBool) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(7))
	if t.v {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
}
func (t testBool) writeData(buf *bytes.Buffer) {
	if t.v {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
}

func (t testArray) writeWithTag(buf *bytes.Buffer) {
	writeLE(buf, uint32(9)) // ARRAY type tag
	var elemType uint32 = 4 // default uint32
	if len(t.elements) > 0 {
		elemType = t.elements[0].typeTag()
	}
	writeLE(buf, elemType)
	writeLE(buf, uint64(len(t.elements)))
	for _, elem := range t.elements {
		elem.writeData(buf) // no per-element type tag
	}
}
func (t testArray) writeData(buf *bytes.Buffer) {
	// Arrays don't appear as nested elements in our tests
}

func writeGGUFString(buf *bytes.Buffer, s string) {
	writeLE(buf, uint64(len(s)))
	buf.WriteString(s)
}

type metadataEntry struct {
	key   string
	value testValue
}

// buildGGUF constructs a minimal valid GGUF byte buffer.
func buildGGUF(metadata []metadataEntry, tensorCount uint64) []byte {
	buf := &bytes.Buffer{}

	// Header
	writeLE(buf, uint32(0x46554747)) // magic
	writeLE(buf, uint32(3))          // version
	writeLE(buf, tensorCount)
	writeLE(buf, uint64(len(metadata)))

	// Metadata KV pairs
	for _, kv := range metadata {
		writeGGUFString(buf, kv.key)
		kv.value.writeWithTag(buf)
	}

	// Tensor info entries (minimal: 1D tensors of type F32)
	for i := uint64(0); i < tensorCount; i++ {
		name := fmt.Sprintf("tensor.%d", i)
		writeGGUFString(buf, name)
		writeLE(buf, uint32(1))   // n_dimensions = 1
		writeLE(buf, uint64(128)) // dimension[0] = 128
		writeLE(buf, uint32(0))   // type = F32
		writeLE(buf, i*512)       // offset
	}

	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestParseValidHeader(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(0x46554747)) // magic
	writeLE(buf, uint32(3))          // version
	writeLE(buf, uint64(10))         // tensor_count
	writeLE(buf, uint64(5))          // metadata_kv_count

	header, err := parseHeader(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if header.Version != 3 {
		t.Errorf("version = %d, want 3", header.Version)
	}
	if header.TensorCount != 10 {
		t.Errorf("tensor_count = %d, want 10", header.TensorCount)
	}
	if header.MetadataKVCount != 5 {
		t.Errorf("metadata_kv_count = %d, want 5", header.MetadataKVCount)
	}
}

func TestInvalidMagic(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(0xDEADBEEF))
	writeLE(buf, uint32(3))
	writeLE(buf, uint64(0))
	writeLE(buf, uint64(0))

	_, err := parseHeader(buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidMagic) {
		t.Errorf("expected ErrInvalidMagic, got: %v", err)
	}
}

func TestUnsupportedVersion(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(0x46554747))
	writeLE(buf, uint32(99))
	writeLE(buf, uint64(0))
	writeLE(buf, uint64(0))

	_, err := parseHeader(buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("expected ErrUnsupportedVersion, got: %v", err)
	}
}

func TestReadString(t *testing.T) {
	buf := &bytes.Buffer{}
	s := "hello, gguf!"
	writeLE(buf, uint64(len(s)))
	buf.WriteString(s)

	result, err := readString(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello, gguf!" {
		t.Errorf("string = %q, want %q", result, "hello, gguf!")
	}
}

func TestParseStringValue(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(8)) // STRING type tag
	writeLE(buf, uint64(5)) // length
	buf.WriteString("llama")

	value, err := readValue(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := AsStr(value)
	if !ok {
		t.Fatalf("expected StringVal, got %T", value)
	}
	if s != "llama" {
		t.Errorf("value = %q, want %q", s, "llama")
	}
}

func TestParseUint32Value(t *testing.T) {
	buf := &bytes.Buffer{}
	writeLE(buf, uint32(4)) // UINT32 type tag
	writeLE(buf, uint32(4096))

	value, err := readValue(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, ok := AsU32(value)
	if !ok {
		t.Fatalf("expected Uint32Val, got %T", value)
	}
	if n != 4096 {
		t.Errorf("value = %d, want 4096", n)
	}
}

func TestTruncatedInput(t *testing.T) {
	// Only 2 bytes — not enough for a u32 magic number
	buf := bytes.NewReader([]byte{0x47, 0x47})

	_, err := parseHeader(buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Should be an io error (EOF or unexpected EOF)
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.EOF or io.ErrUnexpectedEOF, got: %v", err)
	}
}

func TestParseArrayValue(t *testing.T) {
	data := buildGGUF([]metadataEntry{
		{
			key: "tokenizer.ggml.tokens",
			value: testArray{elements: []testValue{
				testString{s: "hello"},
				testString{s: "world"},
				testString{s: "test"},
			}},
		},
	}, 0)

	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, ok := gguf.GetMetadata("tokenizer.ggml.tokens")
	if !ok {
		t.Fatal("metadata key not found")
	}
	arr, ok := AsArray(v)
	if !ok {
		t.Fatalf("expected ArrayVal, got %T", v)
	}
	if len(arr) != 3 {
		t.Fatalf("array len = %d, want 3", len(arr))
	}

	for i, expected := range []string{"hello", "world", "test"} {
		s, ok := AsStr(arr[i])
		if !ok {
			t.Errorf("element %d: expected StringVal, got %T", i, arr[i])
			continue
		}
		if s != expected {
			t.Errorf("element %d = %q, want %q", i, s, expected)
		}
	}
}

func TestParseBoolValue(t *testing.T) {
	data := buildGGUF([]metadataEntry{
		{key: "general.little_endian", value: testBool{v: true}},
	}, 0)

	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, ok := gguf.GetMetadata("general.little_endian")
	if !ok {
		t.Fatal("metadata key not found")
	}
	b, ok := AsBool(v)
	if !ok {
		t.Fatalf("expected BoolVal, got %T", v)
	}
	if !b {
		t.Error("expected true, got false")
	}
}

func TestParseFullFile(t *testing.T) {
	data := buildGGUF([]metadataEntry{
		{key: "general.architecture", value: testString{s: "llama"}},
		{key: "general.name", value: testString{s: "Llama 3.1 8B Instruct"}},
		{key: "general.file_type", value: testUint32{v: 17}},
		{key: "llama.context_length", value: testUint32{v: 131072}},
		{key: "llama.embedding_length", value: testUint32{v: 4096}},
		{key: "llama.block_count", value: testUint32{v: 32}},
		{key: "llama.attention.head_count", value: testUint32{v: 32}},
	}, 5)

	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gguf.Architecture() != "llama" {
		t.Errorf("architecture = %q, want %q", gguf.Architecture(), "llama")
	}
	if gguf.Name() != "Llama 3.1 8B Instruct" {
		t.Errorf("name = %q, want %q", gguf.Name(), "Llama 3.1 8B Instruct")
	}
	if gguf.Quantization() != "Q5_K_M" {
		t.Errorf("quantization = %q, want %q", gguf.Quantization(), "Q5_K_M")
	}
	if gguf.ContextLength() != 131072 {
		t.Errorf("context_length = %d, want 131072", gguf.ContextLength())
	}
	if gguf.EmbeddingLength() != 4096 {
		t.Errorf("embedding_length = %d, want 4096", gguf.EmbeddingLength())
	}
	if gguf.BlockCount() != 32 {
		t.Errorf("block_count = %d, want 32", gguf.BlockCount())
	}
	if gguf.HeadCount() != 32 {
		t.Errorf("head_count = %d, want 32", gguf.HeadCount())
	}
	if len(gguf.TensorInfo) != 5 {
		t.Errorf("tensor count = %d, want 5", len(gguf.TensorInfo))
	}
}

func TestFileTypeName(t *testing.T) {
	tests := []struct {
		fileType uint32
		want     string
	}{
		{0, "F32"},
		{1, "F16"},
		{2, "Q4_0"},
		{3, "Q4_1"},
		{7, "Q8_0"},
		{8, "Q5_0"},
		{9, "Q5_1"},
		{10, "Q2_K"},
		{11, "Q3_K_S"},
		{12, "Q3_K_M"},
		{13, "Q3_K_L"},
		{14, "Q4_K_S"},
		{15, "Q4_K_M"},
		{16, "Q5_K_S"},
		{17, "Q5_K_M"},
		{18, "Q6_K"},
		{19, "IQ2_XXS"},
		{20, "IQ2_XS"},
		{21, "IQ3_XXS"},
		{22, "IQ1_S"},
		{23, "IQ4_NL"},
		{24, "IQ3_S"},
		{25, "IQ2_S"},
		{26, "IQ4_XS"},
		{27, "IQ3_M"},
		{28, "IQ1_M"},
		{29, "BF16"},
		{30, "Q4_0_4_4"},
		{31, "Q4_0_4_8"},
		{32, "Q4_0_8_8"},
		{999, "Unknown"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("type_%d", tt.fileType), func(t *testing.T) {
			got := FileTypeName(tt.fileType)
			if got != tt.want {
				t.Errorf("FileTypeName(%d) = %q, want %q", tt.fileType, got, tt.want)
			}
		})
	}
}

func TestConvenienceMethods(t *testing.T) {
	data := buildGGUF([]metadataEntry{
		{key: "general.architecture", value: testString{s: "phi"}},
		{key: "general.file_type", value: testUint32{v: 15}},
		{key: "general.license", value: testString{s: "Apache-2.0"}},
		{key: "phi.context_length", value: testUint32{v: 2048}},
		{key: "phi.embedding_length", value: testUint32{v: 2560}},
		{key: "phi.block_count", value: testUint32{v: 24}},
		{key: "phi.attention.head_count", value: testUint32{v: 32}},
	}, 0)

	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gguf.Architecture() != "phi" {
		t.Errorf("architecture = %q, want %q", gguf.Architecture(), "phi")
	}
	if gguf.Quantization() != "Q4_K_M" {
		t.Errorf("quantization = %q, want %q", gguf.Quantization(), "Q4_K_M")
	}
	if gguf.ContextLength() != 2048 {
		t.Errorf("context_length = %d, want 2048", gguf.ContextLength())
	}
	if gguf.EmbeddingLength() != 2560 {
		t.Errorf("embedding_length = %d, want 2560", gguf.EmbeddingLength())
	}
	if gguf.BlockCount() != 24 {
		t.Errorf("block_count = %d, want 24", gguf.BlockCount())
	}
	if gguf.HeadCount() != 32 {
		t.Errorf("head_count = %d, want 32", gguf.HeadCount())
	}
	if gguf.License() != "Apache-2.0" {
		t.Errorf("license = %q, want %q", gguf.License(), "Apache-2.0")
	}
}

func TestParseEmptyGGUF(t *testing.T) {
	data := buildGGUF(nil, 0)
	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gguf.Header.Version != 3 {
		t.Errorf("version = %d, want 3", gguf.Header.Version)
	}
	if gguf.Header.TensorCount != 0 {
		t.Errorf("tensor_count = %d, want 0", gguf.Header.TensorCount)
	}
	if len(gguf.Metadata) != 0 {
		t.Errorf("metadata len = %d, want 0", len(gguf.Metadata))
	}
	if len(gguf.TensorInfo) != 0 {
		t.Errorf("tensor_info len = %d, want 0", len(gguf.TensorInfo))
	}
}

func TestMissingMetadataReturnsZero(t *testing.T) {
	data := buildGGUF(nil, 0)
	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gguf.Architecture() != "" {
		t.Errorf("architecture = %q, want empty", gguf.Architecture())
	}
	if gguf.Name() != "" {
		t.Errorf("name = %q, want empty", gguf.Name())
	}
	if gguf.Quantization() != "" {
		t.Errorf("quantization = %q, want empty", gguf.Quantization())
	}
	if gguf.ContextLength() != 0 {
		t.Errorf("context_length = %d, want 0", gguf.ContextLength())
	}
	if gguf.License() != "" {
		t.Errorf("license = %q, want empty", gguf.License())
	}
}

func TestParseTensorInfo(t *testing.T) {
	data := buildGGUF(nil, 3)
	gguf, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gguf.TensorInfo) != 3 {
		t.Fatalf("tensor count = %d, want 3", len(gguf.TensorInfo))
	}

	if gguf.TensorInfo[0].Name != "tensor.0" {
		t.Errorf("tensor[0].name = %q, want %q", gguf.TensorInfo[0].Name, "tensor.0")
	}
	if len(gguf.TensorInfo[0].Dimensions) != 1 || gguf.TensorInfo[0].Dimensions[0] != 128 {
		t.Errorf("tensor[0].dimensions = %v, want [128]", gguf.TensorInfo[0].Dimensions)
	}
	if gguf.TensorInfo[0].Type != GGMLTypeF32 {
		t.Errorf("tensor[0].type = %v, want F32", gguf.TensorInfo[0].Type)
	}
	if gguf.TensorInfo[0].Offset != 0 {
		t.Errorf("tensor[0].offset = %d, want 0", gguf.TensorInfo[0].Offset)
	}

	if gguf.TensorInfo[1].Name != "tensor.1" {
		t.Errorf("tensor[1].name = %q, want %q", gguf.TensorInfo[1].Name, "tensor.1")
	}
	if gguf.TensorInfo[1].Offset != 512 {
		t.Errorf("tensor[1].offset = %d, want 512", gguf.TensorInfo[1].Offset)
	}

	if gguf.TensorInfo[2].Name != "tensor.2" {
		t.Errorf("tensor[2].name = %q, want %q", gguf.TensorInfo[2].Name, "tensor.2")
	}
	if gguf.TensorInfo[2].Offset != 1024 {
		t.Errorf("tensor[2].offset = %d, want 1024", gguf.TensorInfo[2].Offset)
	}
}

func TestRejectEmptyFile(t *testing.T) {
	_, err := Parse(bytes.NewReader([]byte{}))
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}

func TestRejectOversizedString(t *testing.T) {
	buf := &bytes.Buffer{}
	// Write a string length that exceeds the safety limit
	writeLE(buf, uint64(maxStringLength+1))

	_, err := readString(buf)
	if err == nil {
		t.Fatal("expected error for oversized string, got nil")
	}
	if !errors.Is(err, ErrSizeLimitExceeded) {
		t.Errorf("expected ErrSizeLimitExceeded, got: %v", err)
	}
}

func TestRejectOversizedArray(t *testing.T) {
	buf := &bytes.Buffer{}
	// Type tag: ARRAY
	writeLE(buf, uint32(9))
	// Element type: UINT32
	writeLE(buf, uint32(4))
	// Count: exceeds limit
	writeLE(buf, uint64(maxArrayCount+1))

	_, err := readValue(buf)
	if err == nil {
		t.Fatal("expected error for oversized array, got nil")
	}
	if !errors.Is(err, ErrSizeLimitExceeded) {
		t.Errorf("expected ErrSizeLimitExceeded, got: %v", err)
	}
}

func TestRejectOversizedDimensions(t *testing.T) {
	buf := &bytes.Buffer{}
	// Tensor name
	writeGGUFString(buf, "bad_tensor")
	// n_dimensions: exceeds limit
	writeLE(buf, uint32(maxDimensions+1))

	_, err := parseTensorInfo(buf)
	if err == nil {
		t.Fatal("expected error for oversized dimensions, got nil")
	}
	if !errors.Is(err, ErrSizeLimitExceeded) {
		t.Errorf("expected ErrSizeLimitExceeded, got: %v", err)
	}
}

func TestGGMLTypeString(t *testing.T) {
	if GGMLTypeF32.String() != "F32" {
		t.Errorf("F32.String() = %q", GGMLTypeF32.String())
	}
	if GGMLTypeQ4K.String() != "Q4_K" {
		t.Errorf("Q4K.String() = %q", GGMLTypeQ4K.String())
	}
	unknown := GGMLType(999)
	if unknown.String() != "Unknown(999)" {
		t.Errorf("Unknown.String() = %q", unknown.String())
	}
}

// ---------------------------------------------------------------------------
// Remote header-only read
// ---------------------------------------------------------------------------

// fixtureMetadata is the metadata used by the remote-read fixtures, shared so
// the local-vs-remote comparison asserts on the same expected values.
func fixtureMetadata() []metadataEntry {
	return []metadataEntry{
		{key: "general.architecture", value: testString{s: "llama"}},
		{key: "general.name", value: testString{s: "Llama 3.1 8B Instruct"}},
		{key: "general.file_type", value: testUint32{v: 17}},
		{key: "llama.context_length", value: testUint32{v: 131072}},
		{key: "llama.embedding_length", value: testUint32{v: 4096}},
		{key: "llama.block_count", value: testUint32{v: 32}},
		{key: "llama.attention.head_count", value: testUint32{v: 32}},
	}
}

// buildGGUFWithTensorData builds a GGUF whose header+metadata+tensor-info is
// small but whose total file size is large (tensor data padding). This lets a
// header-only remote read prove it transfers far fewer bytes than the file.
func buildGGUFWithTensorData(metadata []metadataEntry, tensorCount uint64, tensorDataBytes int) []byte {
	header := buildGGUF(metadata, tensorCount)
	out := make([]byte, len(header)+tensorDataBytes)
	copy(out, header)
	// Padding stays zero — it is never read by a header-only parse.
	return out
}

func TestParseFromURL_HeaderOnly(t *testing.T) {
	// Small header section, ~4 MB of trailing tensor data.
	const tensorDataBytes = 4 << 20
	data := buildGGUFWithTensorData(fixtureMetadata(), 5, tensorDataBytes)

	// Local parse is the ground truth.
	local, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("local parse failed: %v", err)
	}

	srv, served, supportsRange := newRangeFileServer(t, data)
	defer srv.Close()
	_ = supportsRange

	remote, err := ParseFromURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("ParseFromURL failed: %v", err)
	}

	assertSameMetadata(t, local, remote)

	got := served()
	if got >= int64(len(data)) {
		t.Fatalf("served %d bytes, expected materially fewer than file size %d", got, len(data))
	}
	if got >= int64(len(data))/2 {
		t.Fatalf("served %d bytes, expected < half of file size %d (not header-only)", got, len(data))
	}
}

func TestParseFromURL_NoRangeSupport(t *testing.T) {
	const tensorDataBytes = 4 << 20
	data := buildGGUFWithTensorData(fixtureMetadata(), 5, tensorDataBytes)

	local, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("local parse failed: %v", err)
	}

	srv, _, _ := newNoRangeFileServer(t, data)
	defer srv.Close()

	// Count bytes the client actually pulls through the body, independent of
	// how aggressively the server (or the loopback socket buffer) pushes. This
	// is what the parser controls: it stops reading once the metadata and
	// tensor-info section is consumed and never touches the tensor data.
	var consumed atomic.Int64
	client := &http.Client{Transport: &countingTransport{rt: http.DefaultTransport, n: &consumed}}

	remote, err := parseFromURL(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("ParseFromURL (no range) failed: %v", err)
	}

	assertSameMetadata(t, local, remote)

	got := consumed.Load()
	if got >= int64(len(data)) {
		t.Fatalf("client consumed %d bytes, expected to stop before reading the full file %d", got, len(data))
	}
	if got >= int64(len(data))/2 {
		t.Fatalf("client consumed %d bytes, expected < half of file size %d (not header-only)", got, len(data))
	}
}

// countingTransport wraps an http.RoundTripper and counts the bytes read from
// every response body it returns.
type countingTransport struct {
	rt http.RoundTripper
	n  *atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	resp.Body = &countingReadCloser{rc: resp.Body, n: c.n}
	return resp, nil
}

type countingReadCloser struct {
	rc io.ReadCloser
	n  *atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

func assertSameMetadata(t *testing.T, want, got *GGUFFile) {
	t.Helper()
	if got.Architecture() != want.Architecture() {
		t.Errorf("architecture = %q, want %q", got.Architecture(), want.Architecture())
	}
	if got.Name() != want.Name() {
		t.Errorf("name = %q, want %q", got.Name(), want.Name())
	}
	if got.Quantization() != want.Quantization() {
		t.Errorf("quantization = %q, want %q", got.Quantization(), want.Quantization())
	}
	if got.ContextLength() != want.ContextLength() {
		t.Errorf("context_length = %d, want %d", got.ContextLength(), want.ContextLength())
	}
	if got.EmbeddingLength() != want.EmbeddingLength() {
		t.Errorf("embedding_length = %d, want %d", got.EmbeddingLength(), want.EmbeddingLength())
	}
	if got.BlockCount() != want.BlockCount() {
		t.Errorf("block_count = %d, want %d", got.BlockCount(), want.BlockCount())
	}
	if got.HeadCount() != want.HeadCount() {
		t.Errorf("head_count = %d, want %d", got.HeadCount(), want.HeadCount())
	}
	if len(got.TensorInfo) != len(want.TensorInfo) {
		t.Errorf("tensor info len = %d, want %d", len(got.TensorInfo), len(want.TensorInfo))
	}
}

// newRangeFileServer serves data with full HTTP Range support and counts body
// bytes written. Returns the server, a func reporting bytes served, and whether
// the server advertised range support.
func newRangeFileServer(t *testing.T, data []byte) (*httptest.Server, func() int64, bool) {
	t.Helper()
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countingResponseWriter{ResponseWriter: w, n: &served}
		http.ServeContent(cw, r, "model.gguf", time.Time{}, bytes.NewReader(data))
	}))
	return srv, served.Load, true
}

// newNoRangeFileServer serves data WITHOUT range support: it ignores the Range
// header and streams the full body, counting bytes the client actually reads
// (the handler stops writing once the client closes the connection).
func newNoRangeFileServer(t *testing.T, data []byte) (*httptest.Server, func() int64, bool) {
	t.Helper()
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		// Deliberately no Accept-Ranges; write in chunks so a client that stops
		// reading early causes us to stop writing (broken pipe).
		const chunk = 64 * 1024
		for off := 0; off < len(data); off += chunk {
			end := off + chunk
			if end > len(data) {
				end = len(data)
			}
			n, err := w.Write(data[off:end])
			served.Add(int64(n))
			if err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	return srv, served.Load, false
}

type countingResponseWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}
