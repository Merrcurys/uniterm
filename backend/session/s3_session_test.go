package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhnvrm/simples3"
)

func TestRunPoolRunsAllItems(t *testing.T) {
	var count atomic.Int64
	err := runPool(context.Background(), 23, 4, func(i int) error {
		count.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count.Load() != 23 {
		t.Fatalf("ran %d items, want 23", count.Load())
	}
}

func TestRunPoolReturnsFirstError(t *testing.T) {
	sentinel := errors.New("boom")
	err := runPool(context.Background(), 50, 3, func(i int) error {
		if i == 7 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want sentinel", err)
	}
}

func TestRunPoolStopsEarlyOnError(t *testing.T) {
	var ran atomic.Int64
	err := runPool(context.Background(), 1000, 2, func(i int) error {
		if i == 1 {
			return errors.New("boom")
		}
		time.Sleep(time.Millisecond)
		ran.Add(1)
		return nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ran.Load() >= 999 {
		t.Fatalf("pool kept running after error: %d items ran", ran.Load())
	}
}

func TestRunPoolHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int64
	err := runPool(ctx, 10000, 2, func(i int) error {
		ran.Add(1)
		if i == 5 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if ran.Load() >= 10000 {
		t.Fatal("pool did not stop after cancellation")
	}
}

func TestIsS3Retryable(t *testing.T) {
	cases := map[string]bool{
		"status code: 500 Internal Server Error": true,
		"status code: 503 Service Unavailable":   true,
		"net/http: request timeout":              true,
		"read: connection reset by peer":         true,
		"unexpected EOF":                         true,
		"status code: 404 Not Found":             false,
		"status code: 403 Forbidden":             false,
		"access denied":                          false,
	}
	for msg, want := range cases {
		if got := isS3Retryable(errors.New(msg)); got != want {
			t.Errorf("isS3Retryable(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestS3ContentType(t *testing.T) {
	if got := s3ContentType(".PNG"); got != "image/png" {
		t.Errorf("s3ContentType(.PNG) = %q", got)
	}
	if got := s3ContentType(".unknown"); got != "application/octet-stream" {
		t.Errorf("s3ContentType(.unknown) = %q", got)
	}
}

func TestS3PartSizeFor(t *testing.T) {
	const mb = 1024 * 1024
	cases := []struct {
		size int64
		want int64
	}{
		{8 * mb, 5 * mb},                   // tiny upload -> protocol floor
		{40 << 30, 5 * mb},                 // 40 GB still fits 9000 x 5 MB parts
		{90 << 30, (90<<30 + 8999) / 9000}, // 90 GB needs larger parts
	}
	for _, c := range cases {
		if got := s3PartSizeFor(c.size); got != c.want {
			t.Errorf("s3PartSizeFor(%d) = %d, want %d", c.size, got, c.want)
		}
	}
}

func TestS3DownloadChunkSizeFor(t *testing.T) {
	const mb = 1024 * 1024
	cases := []struct {
		size int64
		want int64
	}{
		{16 * mb, 16 * mb},                 // at the multipart threshold
		{100 * mb, 16 * mb},                // comfortably under the request budget
		{100 << 30, (100<<30 + 999) / 1000}, // 100 GB: grow chunks to stay under 1000
	}
	for _, c := range cases {
		if got := s3DownloadChunkSizeFor(c.size); got != c.want {
			t.Errorf("s3DownloadChunkSizeFor(%d) = %d, want %d", c.size, got, c.want)
		}
	}
}

// newS3RangeTestSession wires an S3Session at an httptest server so ranged
// download paths can be exercised without a real object store.
func newS3RangeTestSession(t *testing.T, ts *httptest.Server) (*S3Session, *TransferTask) {
	t.Helper()
	s := NewS3Session("test")
	s.s3 = simples3.New("us-east-1", "ak", "sk")
	s.s3.Endpoint = ts.URL
	s.client = ts.Client()
	s.bucket = "bkt"
	s.cwd = "/bkt"
	task := &TransferTask{ID: "t1", Type: "download", Status: "running"}
	task.start()
	t.Cleanup(task.done)
	return s, task
}

// serve206 answers Range requests with a proper 206 slice, like S3/MinIO.
func serve206(t *testing.T, data []byte, ranges *atomic.Int64) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
			t.Errorf("bad Range header %q: %v", rng, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ranges.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	})
}

func TestS3DownloadRangedAssemblesFile(t *testing.T) {
	const size = 17 << 20 // just above s3MultipartThreshold: 2 chunks
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31 % 251)
	}
	var ranges atomic.Int64
	ts := httptest.NewServer(serve206(t, data, &ranges))
	defer ts.Close()

	s, task := newS3RangeTestSession(t, ts)
	out := filepath.Join(t.TempDir(), "out.bin")
	if err := s.downloadObject(task, "obj", out, size); err != nil {
		t.Fatalf("downloadObject: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(got, data) {
		t.Fatalf("file mismatch: got %d bytes", len(got))
	}
	if ranges.Load() < 2 {
		t.Fatalf("expected >=2 ranged GETs, got %d", ranges.Load())
	}
}

func TestS3DownloadFallsBackWhenRangeIgnored(t *testing.T) {
	const size = 17 << 20
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 17 % 249)
	}
	// Server ignores Range entirely: always 200 with the full body.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer ts.Close()

	s, task := newS3RangeTestSession(t, ts)
	out := filepath.Join(t.TempDir(), "out.bin")
	if err := s.downloadObject(task, "obj", out, size); err != nil {
		t.Fatalf("downloadObject: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(got, data) {
		t.Fatalf("file mismatch after fallback: got %d bytes", len(got))
	}
}

func TestS3DownloadFallsBackOnPresignedReject(t *testing.T) {
	const size = 17 << 20
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 13 % 253)
	}
	// A provider that rejects path-style presigned URLs with 403 but serves
	// plain signed GETs (the simples3 client path) normally.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("SecondLevelDomainForbidden"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer ts.Close()

	s, task := newS3RangeTestSession(t, ts)
	out := filepath.Join(t.TempDir(), "out.bin")
	if err := s.downloadObject(task, "obj", out, size); err != nil {
		t.Fatalf("downloadObject: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(got, data) {
		t.Fatalf("file mismatch after fallback: got %d bytes", len(got))
	}
}

// bytesEqual avoids importing bytes just for Equal in a table of tests.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
