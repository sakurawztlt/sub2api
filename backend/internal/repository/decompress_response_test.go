package repository

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const decompressSamplePayload = `data: {"type":"message_start"}` + "\n\n" + `data: {"type":"message_stop"}` + "\n\n"

func newResponseWithBody(t *testing.T, encoded []byte, encoding string) *http.Response {
	t.Helper()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(encoded)),
	}
	if encoding != "" {
		resp.Header.Set("Content-Encoding", encoding)
	}
	resp.ContentLength = int64(len(encoded))
	resp.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	return resp
}

func readAllAndClose(t *testing.T, body io.ReadCloser) []byte {
	t.Helper()
	defer func() { _ = body.Close() }()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return out
}

func TestDecompressResponseBody_Zstd(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll([]byte(decompressSamplePayload), nil)
	_ = enc.Close()

	resp := newResponseWithBody(t, compressed, "zstd")
	decompressResponseBody(resp)

	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding should be cleared, got %q", resp.Header.Get("Content-Encoding"))
	}
	if resp.Header.Get("Content-Length") != "" {
		t.Fatalf("Content-Length should be cleared, got %q", resp.Header.Get("Content-Length"))
	}
	if resp.ContentLength != -1 {
		t.Fatalf("ContentLength should be -1 after decompression, got %d", resp.ContentLength)
	}

	got := readAllAndClose(t, resp.Body)
	if string(got) != decompressSamplePayload {
		t.Fatalf("body mismatch:\n  got=%q\n want=%q", got, decompressSamplePayload)
	}
}

func TestDecompressResponseBody_Gzip(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(decompressSamplePayload)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	resp := newResponseWithBody(t, buf.Bytes(), "gzip")
	decompressResponseBody(resp)

	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding should be cleared, got %q", resp.Header.Get("Content-Encoding"))
	}
	got := readAllAndClose(t, resp.Body)
	if string(got) != decompressSamplePayload {
		t.Fatalf("body mismatch: got %q", got)
	}
}

func TestDecompressResponseBody_Brotli(t *testing.T) {
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write([]byte(decompressSamplePayload)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}

	resp := newResponseWithBody(t, buf.Bytes(), "br")
	decompressResponseBody(resp)

	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding should be cleared, got %q", resp.Header.Get("Content-Encoding"))
	}
	got := readAllAndClose(t, resp.Body)
	if string(got) != decompressSamplePayload {
		t.Fatalf("body mismatch: got %q", got)
	}
}

func TestDecompressResponseBody_Deflate(t *testing.T) {
	var buf bytes.Buffer
	zw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate.NewWriter: %v", err)
	}
	if _, err := zw.Write([]byte(decompressSamplePayload)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}

	resp := newResponseWithBody(t, buf.Bytes(), "deflate")
	decompressResponseBody(resp)

	got := readAllAndClose(t, resp.Body)
	if string(got) != decompressSamplePayload {
		t.Fatalf("deflate body mismatch: got %q", got)
	}
}

func TestDecompressResponseBody_IdentityPassthrough(t *testing.T) {
	resp := newResponseWithBody(t, []byte(decompressSamplePayload), "")
	decompressResponseBody(resp)
	got := readAllAndClose(t, resp.Body)
	if string(got) != decompressSamplePayload {
		t.Fatalf("identity body mismatch: got %q", got)
	}
}

func TestDecompressResponseBody_NilSafe(t *testing.T) {
	decompressResponseBody(nil)

	resp := &http.Response{Header: http.Header{}, Body: nil}
	decompressResponseBody(resp) // must not panic
}
