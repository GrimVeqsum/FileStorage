package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func s3TestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4("test", "test-secret", ""), Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Client{minioClient: client, bucket: "files"}
}

func TestDownloadForcesGETBeforeReturning(t *testing.T) {
	var getCalled atomic.Bool
	client := s3TestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "10")
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("ETag", `"test-etag"`)
			return
		}
		getCalled.Store(true)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>GET not allowed</Message></Error>`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	body, err := client.Download(ctx, "a-file.txt")
	if err == nil || body != nil || !getCalled.Load() {
		t.Fatalf("lazy GET failure not detected: body=%v error=%v GET=%v", body, err, getCalled.Load())
	}
}

func TestDownloadPrefetchPreservesAllBytesAndSize(t *testing.T) {
	payload := strings.Repeat("a", 1025)
	client := s3TestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1025")
		w.Header().Set("ETag", `"test-etag"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		if r.Method == http.MethodGet {
			io.WriteString(w, payload)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	body, err := client.Download(ctx, "a-file.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	actual, err := io.ReadAll(body)
	if err != nil || string(actual) != payload || body.(interface{ Size() int64 }).Size() != 1025 {
		t.Fatalf("prefetch damaged response: size=%d error=%v", len(actual), err)
	}
}

func TestMissingBucketIsNotReady(t *testing.T) {
	client := s3TestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Ready(ctx); err == nil {
		t.Fatal("missing bucket incorrectly reported ready")
	}
}

func TestObjectKeyTraversalRejectedBeforeNetwork(t *testing.T) {
	client := &Client{} // A network operation would panic; invalid keys must fail first.
	for _, key := range []string{"", "../escape", "/absolute", `folder\name`, "a\r\nb", "..", "."} {
		if err := client.Upload(context.Background(), key, strings.NewReader("x"), 1, "text/plain"); err == nil {
			t.Fatalf("unsafe upload key %q accepted", key)
		}
		if _, err := client.Download(context.Background(), key); err == nil {
			t.Fatalf("unsafe download key %q accepted", key)
		}
		if err := client.Delete(context.Background(), key); err == nil {
			t.Fatalf("unsafe delete key %q accepted", key)
		}
	}
	if !validStorageKey("cd75915d-c134-4c43-a71f-8c2d77716503.txt") || !validStorageKey(`cd75915d-c134-4c43-a71f-8c2d77716503.a\bar`) {
		t.Fatal("legacy key rejected")
	}
}
