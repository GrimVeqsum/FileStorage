//go:build integration

// Package integration exercises the HTTP API and durable state transitions against
// real PostgreSQL and MinIO. Run it through scripts/test-integration.sh.
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"filestorage/internal/config"
	"filestorage/internal/files"
	"filestorage/internal/httptransport"
	"filestorage/internal/storage"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const testLimit = int64(1 << 20)

var (
	testDB          *sql.DB
	testStorage     *storage.Client
	testConfig      config.Config
	testDatabaseURL string
	legacyID        string
	legacyKey       string
	legacyBytes     = []byte("uploaded before the consistency migration\x00\xff")
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if err := setup(); err != nil {
		// Configuration values can contain secrets. Report only setup's fixed
		// diagnostic, rather than printing the PostgreSQL DSN or S3 credentials.
		fmt.Fprintln(os.Stderr, "integration setup:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func setup() error {
	for _, key := range []string{"TEST_DATABASE_URL", "TEST_MINIO_ENDPOINT", "TEST_MINIO_ACCESS_KEY", "TEST_MINIO_SECRET_KEY", "TEST_MINIO_BUCKET"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("%s is required; run scripts/test-integration.sh", key)
		}
	}
	databaseURL, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || strings.TrimPrefix(databaseURL.Path, "/") != "filestorage_test" {
		return errors.New("TEST_DATABASE_URL must select the dedicated filestorage_test database")
	}
	if !strings.HasPrefix(os.Getenv("TEST_MINIO_BUCKET"), "filestorage-integration") {
		return errors.New("TEST_MINIO_BUCKET must start with filestorage-integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminDB, err := sql.Open("pgx", databaseURL.String())
	if err != nil {
		return errors.New("open dedicated integration database failed")
	}
	defer adminDB.Close()
	// A unique schema permits independent/repeated runs without deleting any
	// database, table, volume, or artifact belonging to another run.
	schema := "integration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		return errors.New("create isolated integration schema failed")
	}
	query := databaseURL.Query()
	query.Set("search_path", schema+",public")
	databaseURL.RawQuery = query.Encode()
	testDatabaseURL = databaseURL.String()
	testDB, err = sql.Open("pgx", databaseURL.String())
	if err != nil {
		return errors.New("open isolated integration schema failed")
	}
	testDB.SetMaxOpenConns(12)
	if err := testDB.PingContext(ctx); err != nil {
		return errors.New("ping integration PostgreSQL failed")
	}
	testConfig = config.Config{
		MinIOEndpoint:  os.Getenv("TEST_MINIO_ENDPOINT"),
		MinIOAccessKey: os.Getenv("TEST_MINIO_ACCESS_KEY"),
		MinIOSecretKey: os.Getenv("TEST_MINIO_SECRET_KEY"),
		MinIOBucket:    os.Getenv("TEST_MINIO_BUCKET") + "-" + uuid.NewString()[:8],
	}
	testStorage, err = storage.NewClient(ctx, testConfig)
	if err != nil {
		return errors.New("initialize integration MinIO failed")
	}
	migrations, err := filepath.Glob("../migrations/*.up.sql")
	if err != nil || len(migrations) < 2 {
		return errors.New("consistency migrations are missing")
	}
	sort.Strings(migrations)
	for i, path := range migrations {
		script, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read migration %s failed", filepath.Base(path))
		}
		if _, err := testDB.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("apply migration %s failed: %w", filepath.Base(path), err)
		}
		if i == 0 {
			legacyID = uuid.NewString()
			// The old Linux filepath.Ext path could retain a backslash in
			// an extension. Preserve this existing S3 key through migration.
			legacyKey = uuid.NewString() + `.a\bar`
			if err := testStorage.Upload(ctx, legacyKey, bytes.NewReader(legacyBytes), int64(len(legacyBytes)), "text/plain"); err != nil {
				return errors.New("seed legacy object failed")
			}
			if _, err := testDB.ExecContext(ctx,
				`INSERT INTO files (id, original_name, storage_key, content_type, size) VALUES ($1,$2,$3,$4,$5)`,
				legacyID, "legacy file.txt", legacyKey, "text/plain", len(legacyBytes)); err != nil {
				return errors.New("seed legacy metadata failed")
			}
		}
	}
	return nil
}

func service(repository files.MetadataRepository, objectStorage files.FileStorage) *files.Service {
	if repository == nil {
		repository = files.NewRepository(testDB)
	}
	if objectStorage == nil {
		objectStorage = testStorage
	}
	return files.NewService(repository, objectStorage, testLimit)
}

func api(t *testing.T, svc *files.Service) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(httptransport.NewRouter(files.NewHandler(svc), svc))
	t.Cleanup(server.Close)
	return server
}

func uploadRequest(t *testing.T, endpoint, filename string, payload []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint+"/files", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func do(t *testing.T, request *http.Request) *http.Response {
	t.Helper()
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func httpUpload(t *testing.T, server *httptest.Server, filename string, payload []byte) files.File {
	t.Helper()
	response := do(t, uploadRequest(t, server.URL, filename, payload))
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("upload status %d, body %s", response.StatusCode, body)
	}
	var result files.File
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.ID == "" || result.Size != int64(len(payload)) {
		t.Fatalf("incorrect upload metadata: %+v", result)
	}
	return result
}

func getRequest(t *testing.T, method, endpoint string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func directUpload(t *testing.T, svc *files.Service, payload []byte) (files.File, error) {
	t.Helper()
	return svc.Upload(context.Background(), bytesFile{bytes.NewReader(payload)}, &multipart.FileHeader{
		Filename: "integration.bin", Size: int64(len(payload)), Header: textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}},
	})
}

type bytesFile struct{ *bytes.Reader }

func (bytesFile) Close() error { return nil }

func state(t *testing.T, id string) string {
	t.Helper()
	var result string
	if err := testDB.QueryRow(`SELECT state FROM files WHERE id=$1`, id).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func objectKey(t *testing.T, id string) string {
	t.Helper()
	var key string
	if err := testDB.QueryRow(`SELECT storage_key FROM files WHERE id=$1`, id).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

func assertInvisible(t *testing.T, svc *files.Service, id string) {
	t.Helper()
	_, body, err := svc.Download(context.Background(), id)
	if body != nil {
		body.Close()
	}
	if !errors.Is(err, files.ErrFileNotFound) {
		t.Fatalf("non-ready row is downloadable: %v", err)
	}
	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range list {
		if file.ID == id {
			t.Fatal("non-ready row leaked into file list")
		}
	}
}

func assertObjectMissing(t *testing.T, key string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := testStorage.Download(ctx, key)
	if body != nil {
		body.Close()
	}
	if err == nil {
		t.Fatal("object still exists after cleanup")
	}
}

func forceCleanup(t *testing.T, id string) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE files SET updated_at=now()-interval '1 hour', next_retry_at=now()-interval '1 hour' WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
}

func TestHTTPUploadDownload(t *testing.T) {
	server := api(t, service(nil, nil))
	for _, size := range []int{1, 8193, int(testLimit)} {
		t.Run(fmt.Sprintf("bytes_%d", size), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0, 1, '\n', '\r', 0xff, 0xfe, 'a'}, size/7+1)[:size]
			file := httpUpload(t, server, "roundtrip.bin", payload)
			response := do(t, getRequest(t, http.MethodGet, server.URL+"/files/"+file.ID))
			if response.StatusCode != http.StatusOK {
				t.Fatalf("download status %d", response.StatusCode)
			}
			got, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("downloaded bytes differ from uploaded bytes")
			}
			if response.ContentLength != int64(size) {
				t.Fatalf("Content-Length = %d, want %d", response.ContentLength, size)
			}
			if response.Header.Get("Content-Type") == "" {
				t.Fatal("download has no Content-Type")
			}
			if file.StorageKey != "" {
				t.Fatal("internal object key exposed in JSON")
			}
		})
	}
}

func TestHTTPDefaultTwentyMiBRoundTrip(t *testing.T) {
	// This exercises the default file limit, beyond the SDK's former multipart
	// threshold, through the real spool/MD5/single-PUT path and both stores.
	const size = 20 << 20
	svc := files.NewService(files.NewRepository(testDB), testStorage, size)
	server := api(t, svc)
	payload := bytes.Repeat([]byte{0, 0xff, '\r', '\n', 'a', 'b', 'c', 0x80}, size/8)
	want := sha256.Sum256(payload)
	file := httpUpload(t, server, "default-limit.bin", payload)
	response := do(t, getRequest(t, http.MethodGet, server.URL+"/files/"+file.ID))
	if response.StatusCode != http.StatusOK || response.ContentLength != size {
		t.Fatalf("20 MiB download status=%d length=%d", response.StatusCode, response.ContentLength)
	}
	digest := sha256.New()
	n, err := io.Copy(digest, response.Body)
	if err != nil || n != size || !bytes.Equal(digest.Sum(nil), want[:]) {
		t.Fatalf("20 MiB roundtrip changed bytes: downloaded=%d error=%v", n, err)
	}
	deleted := do(t, getRequest(t, http.MethodDelete, server.URL+"/files/"+file.ID))
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("20 MiB file delete status %d", deleted.StatusCode)
	}
	assertObjectMissing(t, objectKey(t, file.ID))
}

func TestHTTPRejectedUploads(t *testing.T) {
	server := api(t, service(nil, nil))
	t.Run("empty", func(t *testing.T) {
		response := do(t, uploadRequest(t, server.URL, "empty", nil))
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("empty upload status %d", response.StatusCode)
		}
	})
	t.Run("file_limit", func(t *testing.T) {
		response := do(t, uploadRequest(t, server.URL, "too-big", bytes.Repeat([]byte{'a'}, int(testLimit)+1)))
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized file status %d", response.StatusCode)
		}
	})
	t.Run("request_body_limit", func(t *testing.T) {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/files", bytes.NewReader(bytes.Repeat([]byte{'a'}, int(testLimit)+(2<<20))))
		request.Header.Set("Content-Type", "multipart/form-data; boundary=not-there")
		response := do(t, request)
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized request body status %d", response.StatusCode)
		}
	})
	t.Run("malformed_multipart", func(t *testing.T) {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/files", strings.NewReader("--broken\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x\"\r\n\r\ntruncated"))
		request.Header.Set("Content-Type", "multipart/form-data; boundary=broken")
		response := do(t, request)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("malformed multipart status %d", response.StatusCode)
		}
	})
	t.Run("missing_file", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("description", "no file"); err != nil {
			t.Fatal(err)
		}
		writer.Close()
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/files", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response := do(t, request)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("missing file status %d", response.StatusCode)
		}
	})
}

func TestHTTPFilenameSafety(t *testing.T) {
	server := api(t, service(nil, nil))
	for _, name := range []string{"отчёт 世界.txt", `quoted "file"; 'sample'.txt`, "../../unsafe.txt", `..\..\unsafe.txt`} {
		t.Run(name, func(t *testing.T) {
			file := httpUpload(t, server, name, []byte("safe bytes"))
			response := do(t, getRequest(t, http.MethodGet, server.URL+"/files/"+file.ID))
			if response.StatusCode != http.StatusOK {
				t.Fatalf("download status %d", response.StatusCode)
			}
			kind, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
			if err != nil || kind != "attachment" || params["filename"] == "" {
				t.Fatalf("invalid Content-Disposition: %q (%v)", response.Header.Get("Content-Disposition"), err)
			}
			if strings.ContainsAny(params["filename"], "\r\n/\\") {
				t.Fatalf("unsafe download name: %q", params["filename"])
			}
			if strings.Contains(name, "世界") && !strings.Contains(params["filename"], "世界") {
				t.Fatalf("Unicode filename lost: %q", params["filename"])
			}
			key := objectKey(t, file.ID)
			if _, err := uuid.Parse(key); err != nil {
				t.Fatalf("object key uses user filename: %q", key)
			}
		})
	}
	t.Run("encoded_CRLF", func(t *testing.T) {
		response := do(t, uploadRequest(t, server.URL, "evil\r\nX-Injected: yes.txt", []byte("safe bytes")))
		if response.StatusCode == http.StatusBadRequest {
			return // Explicit rejection of control characters is safe.
		}
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("control-character filename status %d", response.StatusCode)
		}
		var file files.File
		if err := json.NewDecoder(response.Body).Decode(&file); err != nil {
			t.Fatal(err)
		}
		download := do(t, getRequest(t, http.MethodGet, server.URL+"/files/"+file.ID))
		if download.StatusCode != http.StatusOK || download.Header.Get("X-Injected") != "" {
			t.Fatal("CRLF filename affected HTTP response")
		}
		_, params, err := mime.ParseMediaType(download.Header.Get("Content-Disposition"))
		if err != nil || strings.ContainsAny(params["filename"], "\r\n") {
			t.Fatal("CRLF survived in Content-Disposition")
		}
	})
}

func TestHTTPDeleteIdempotentAndConcurrent(t *testing.T) {
	server := api(t, service(nil, nil))
	file := httpUpload(t, server, "delete.bin", []byte("delete me"))
	key := objectKey(t, file.ID)
	var wait sync.WaitGroup
	statuses := make(chan int, 2)
	errors := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request, _ := http.NewRequest(http.MethodDelete, server.URL+"/files/"+file.ID, nil)
			response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
			if err != nil {
				errors <- err
				return
			}
			response.Body.Close()
			statuses <- response.StatusCode
		}()
	}
	wait.Wait()
	close(statuses)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	for status := range statuses {
		if status != http.StatusNoContent {
			t.Fatalf("concurrent delete status %d", status)
		}
	}
	for _, id := range []string{file.ID, uuid.NewString()} {
		response := do(t, getRequest(t, http.MethodDelete, server.URL+"/files/"+id))
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("repeated/absent delete status %d", response.StatusCode)
		}
		body, _ := io.ReadAll(response.Body)
		if len(body) != 0 {
			t.Fatal("204 delete response has a body")
		}
	}
	assertInvisible(t, service(nil, nil), file.ID)
	assertObjectMissing(t, key)
}

// Wrappers only replace the precise failed boundary. Every unaffected operation
// still runs against the real PostgreSQL and MinIO containers.
type repositoryFault struct {
	files.MetadataRepository
	create      func(context.Context, files.CreateFileParams) (files.File, error)
	activate    func(context.Context, string) (files.File, error)
	beginDelete func(context.Context, string) (files.File, error)
	markDeleted func(context.Context, string) error
}

func (r repositoryFault) Create(ctx context.Context, params files.CreateFileParams) (files.File, error) {
	if r.create != nil {
		return r.create(ctx, params)
	}
	return r.MetadataRepository.Create(ctx, params)
}

func (r repositoryFault) Activate(ctx context.Context, id string) (files.File, error) {
	if r.activate != nil {
		return r.activate(ctx, id)
	}
	return r.MetadataRepository.Activate(ctx, id)
}

func (r repositoryFault) BeginDelete(ctx context.Context, id string) (files.File, error) {
	if r.beginDelete != nil {
		return r.beginDelete(ctx, id)
	}
	return r.MetadataRepository.BeginDelete(ctx, id)
}

func (r repositoryFault) MarkDeleted(ctx context.Context, id string) error {
	if r.markDeleted != nil {
		return r.markDeleted(ctx, id)
	}
	return r.MetadataRepository.MarkDeleted(ctx, id)
}

type storageFault struct {
	files.FileStorage
	upload func(context.Context, string, io.Reader, int64, string) error
	delete func(context.Context, string) error
}

func (s storageFault) Upload(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	if s.upload != nil {
		return s.upload(ctx, key, reader, size, contentType)
	}
	return s.FileStorage.Upload(ctx, key, reader, size, contentType)
}

func (s storageFault) Delete(ctx context.Context, key string) error {
	if s.delete != nil {
		return s.delete(ctx, key)
	}
	return s.FileStorage.Delete(ctx, key)
}

var injectedFailure = errors.New("injected boundary failure")

func TestUploadPostgresFailureAfterMinIOSuccess(t *testing.T) {
	repository := files.NewRepository(testDB)
	var id string
	fault := repositoryFault{MetadataRepository: repository, activate: func(_ context.Context, candidate string) (files.File, error) {
		id = candidate
		return files.File{}, injectedFailure
	}}
	_, err := directUpload(t, service(fault, nil), []byte("S3 PUT succeeded but PostgreSQL activation failed"))
	if err == nil || id == "" {
		t.Fatal("expected activation failure after successful S3 PUT")
	}
	assertInvisible(t, service(nil, nil), id)
	forceCleanup(t, id)
	if err := service(nil, nil).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertObjectMissing(t, objectKey(t, id))
	if state(t, id) != "deleted" {
		t.Fatal("failed upload was not durably cleaned")
	}
}

func TestUploadAmbiguousPostgresCommitPreservesReadyObject(t *testing.T) {
	repository := files.NewRepository(testDB)
	var id string
	fault := repositoryFault{MetadataRepository: repository, activate: func(ctx context.Context, candidate string) (files.File, error) {
		id = candidate
		if _, err := repository.Activate(ctx, candidate); err != nil {
			return files.File{}, err
		}
		return files.File{}, injectedFailure // COMMIT succeeded; acknowledgement was lost.
	}}
	payload := []byte("a committed ready file must not be compensated away")
	_, err := directUpload(t, service(fault, nil), payload)
	if err == nil || id == "" {
		t.Fatal("expected ambiguous acknowledgement failure")
	}
	if state(t, id) != "ready" {
		t.Fatal("ambiguous commit removed a ready file")
	}
	_, body, err := service(nil, nil).Download(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("committed object unavailable after ambiguous result: %v", err)
	}
}

func TestUploadAmbiguousIntentDoesNotCreateUntrackedObject(t *testing.T) {
	repository := files.NewRepository(testDB)
	var id string
	var uploads atomic.Int32
	repositoryWrapper := repositoryFault{MetadataRepository: repository, create: func(ctx context.Context, params files.CreateFileParams) (files.File, error) {
		id = params.ID
		if _, err := repository.Create(ctx, params); err != nil {
			return files.File{}, err
		}
		return files.File{}, injectedFailure
	}}
	storageWrapper := storageFault{FileStorage: testStorage, upload: func(context.Context, string, io.Reader, int64, string) error {
		uploads.Add(1)
		return nil
	}}
	_, err := directUpload(t, service(repositoryWrapper, storageWrapper), []byte("no PUT without durable intent acknowledgement"))
	if err == nil || id == "" || uploads.Load() != 0 {
		t.Fatal("ambiguous intent unexpectedly sent an S3 PUT")
	}
	assertInvisible(t, service(nil, nil), id)
	forceCleanup(t, id)
	if err := service(nil, nil).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state(t, id) != "deleted" {
		t.Fatal("unacknowledged intent was not recovered")
	}
}

func TestMinIOUploadFailureAndLostAcknowledgement(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("object_written_%v", committed), func(t *testing.T) {
			var id, key string
			repository := files.NewRepository(testDB)
			repositoryWrapper := repositoryFault{MetadataRepository: repository, create: func(ctx context.Context, params files.CreateFileParams) (files.File, error) {
				id = params.ID
				key = params.StorageKey
				return repository.Create(ctx, params)
			}}
			storageWrapper := storageFault{FileStorage: testStorage, upload: func(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error {
				if committed {
					if err := testStorage.Upload(ctx, objectKey, reader, size, contentType); err != nil {
						return err
					}
				}
				return injectedFailure
			}}
			_, err := directUpload(t, service(repositoryWrapper, storageWrapper), []byte("test upload error"))
			if !errors.Is(err, files.ErrStorageUnavailable) || id == "" {
				t.Fatalf("expected storage failure, got %v", err)
			}
			assertInvisible(t, service(nil, nil), id)
			forceCleanup(t, id)
			if err := service(nil, nil).Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertObjectMissing(t, key)
		})
	}
}

func TestUploadCancellationUsesIndependentCleanupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var id, key string
	var cleanupObserved atomic.Bool
	repository := files.NewRepository(testDB)
	repositoryWrapper := repositoryFault{MetadataRepository: repository, create: func(ctx context.Context, params files.CreateFileParams) (files.File, error) {
		id, key = params.ID, params.StorageKey
		return repository.Create(ctx, params)
	}}
	storageWrapper := storageFault{FileStorage: testStorage,
		upload: func(uploadCtx context.Context, key string, reader io.Reader, size int64, contentType string) error {
			if err := testStorage.Upload(uploadCtx, key, reader, size, contentType); err != nil {
				return err
			}
			cancel()
			return uploadCtx.Err()
		},
		delete: func(cleanupCtx context.Context, key string) error {
			if cleanupCtx.Err() != nil {
				return errors.New("cleanup inherited canceled request")
			}
			if _, ok := cleanupCtx.Deadline(); !ok {
				return errors.New("cleanup context has no deadline")
			}
			cleanupObserved.Store(true)
			return testStorage.Delete(cleanupCtx, key)
		},
	}
	payload := []byte("the PUT completed before request cancellation")
	_, err := service(repositoryWrapper, storageWrapper).Upload(ctx, bytesFile{bytes.NewReader(payload)}, &multipart.FileHeader{Filename: "cancel.bin", Size: int64(len(payload))})
	if err == nil || id == "" || !cleanupObserved.Load() {
		t.Fatalf("request cancellation did not run bounded independent compensation: %v", err)
	}
	assertInvisible(t, service(nil, nil), id)
	assertObjectMissing(t, key)
}

func TestDeletePostgresFailures(t *testing.T) {
	t.Run("before_intent", func(t *testing.T) {
		file, err := directUpload(t, service(nil, nil), []byte("keep until delete intent persisted"))
		if err != nil {
			t.Fatal(err)
		}
		fault := repositoryFault{MetadataRepository: files.NewRepository(testDB), beginDelete: func(context.Context, string) (files.File, error) {
			return files.File{}, injectedFailure
		}}
		if err := service(fault, nil).Delete(context.Background(), file.ID); err == nil {
			t.Fatal("expected PostgreSQL failure")
		}
		if state(t, file.ID) != "ready" {
			t.Fatal("failed delete intent changed visible file")
		}
		_, body, err := service(nil, nil).Download(context.Background(), file.ID)
		if err != nil {
			t.Fatal("object removed before delete intent was persisted")
		}
		body.Close()
	})
	t.Run("after_object_removed", func(t *testing.T) {
		file, err := directUpload(t, service(nil, nil), []byte("delete recovery after metadata error"))
		if err != nil {
			t.Fatal(err)
		}
		fault := repositoryFault{MetadataRepository: files.NewRepository(testDB), markDeleted: func(context.Context, string) error {
			return injectedFailure
		}}
		if err := service(fault, nil).Delete(context.Background(), file.ID); err == nil {
			t.Fatal("expected metadata finalization failure")
		}
		assertInvisible(t, service(nil, nil), file.ID)
		assertObjectMissing(t, file.StorageKey)
		forceCleanup(t, file.ID)
		if err := service(nil, nil).Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if state(t, file.ID) != "deleted" {
			t.Fatal("delete metadata failure was not recovered")
		}
	})
	t.Run("ambiguous_intent", func(t *testing.T) {
		file, err := directUpload(t, service(nil, nil), []byte("delete intent committed but unacknowledged"))
		if err != nil {
			t.Fatal(err)
		}
		repository := files.NewRepository(testDB)
		fault := repositoryFault{MetadataRepository: repository, beginDelete: func(ctx context.Context, id string) (files.File, error) {
			if _, err := repository.BeginDelete(ctx, id); err != nil {
				return files.File{}, err
			}
			return files.File{}, injectedFailure
		}}
		if err := service(fault, nil).Delete(context.Background(), file.ID); err == nil {
			t.Fatal("expected ambiguous intent error")
		}
		assertInvisible(t, service(nil, nil), file.ID)
		forceCleanup(t, file.ID)
		if err := service(nil, nil).Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertObjectMissing(t, file.StorageKey)
	})
}

func TestDeleteMinIOFailureAndLostAcknowledgement(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("object_removed_%v", committed), func(t *testing.T) {
			file, err := directUpload(t, service(nil, nil), []byte("S3 delete retry"))
			if err != nil {
				t.Fatal(err)
			}
			fault := storageFault{FileStorage: testStorage, delete: func(ctx context.Context, key string) error {
				if committed {
					if err := testStorage.Delete(ctx, key); err != nil {
						return err
					}
				}
				return injectedFailure
			}}
			if err := service(nil, fault).Delete(context.Background(), file.ID); !errors.Is(err, files.ErrStorageUnavailable) {
				t.Fatalf("expected storage delete failure, got %v", err)
			}
			assertInvisible(t, service(nil, nil), file.ID)
			if err := service(nil, nil).Delete(context.Background(), file.ID); err != nil {
				t.Fatal("idempotent delete retry failed:", err)
			}
			assertObjectMissing(t, file.StorageKey)
			if state(t, file.ID) != "deleted" {
				t.Fatal("delete retry did not finalize tombstone")
			}
		})
	}
}

func TestRecoveryAfterProcessStopsAtIntermediateStates(t *testing.T) {
	for _, intermediate := range []string{"uploading_without_object", "uploading_with_object", "deleting", "deleted_with_late_put"} {
		t.Run(intermediate, func(t *testing.T) {
			id, key := uuid.NewString(), uuid.NewString()
			status := "uploading"
			if intermediate == "deleting" {
				status = "deleting"
			}
			if intermediate == "deleted_with_late_put" {
				status = "deleted"
			}
			payload := []byte("interrupted process object")
			if intermediate != "uploading_without_object" {
				if err := testStorage.Upload(context.Background(), key, bytes.NewReader(payload), int64(len(payload)), "application/octet-stream"); err != nil {
					t.Fatal(err)
				}
			}
			_, err := testDB.Exec(`INSERT INTO files (id,original_name,storage_key,content_type,size,state,updated_at,next_retry_at)
				VALUES ($1,'interrupted.bin',$2,'application/octet-stream',$3,$4,now()-interval '1 hour',now()-interval '1 hour')`, id, key, len(payload), status)
			if err != nil {
				t.Fatal(err)
			}
			freshProcess := service(nil, nil)
			assertInvisible(t, freshProcess, id)
			if err := freshProcess.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertObjectMissing(t, key)
			if state(t, id) != "deleted" {
				t.Fatal("intermediate state not recovered to durable tombstone")
			}
			// A remote PUT may finish after the process/connection disappeared.
			// A retained tombstone must therefore also clean a later retry.
			if err := testStorage.Upload(context.Background(), key, bytes.NewReader(payload), int64(len(payload)), "application/octet-stream"); err != nil {
				t.Fatal(err)
			}
			forceCleanup(t, id)
			if err := freshProcess.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertObjectMissing(t, key)
		})
	}
}

func TestRecoveryDoesNotClaimActiveUpload(t *testing.T) {
	repository := files.NewRepository(testDB)
	id, key := uuid.NewString(), uuid.NewString()
	_, err := repository.Create(context.Background(), files.CreateFileParams{ID: id, StorageKey: key, OriginalName: "active.bin", ContentType: "application/octet-stream", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := service(nil, nil).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state(t, id) != "uploading" {
		t.Fatal("reconciler claimed a recent active upload")
	}
}

func TestMigrationAndFreshServicePreserveAvailableFiles(t *testing.T) {
	if state(t, legacyID) != "ready" {
		t.Fatal("migration did not preserve old files as ready")
	}
	for range 2 {
		// Fresh service/repository/storage instances simulate application restart;
		// the containers and their persisted data are left running.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		freshStorage, err := storage.NewClient(ctx, testConfig)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		freshService := service(files.NewRepository(testDB), freshStorage)
		if err := freshService.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, body, err := freshService.Download(context.Background(), legacyID)
		if err != nil {
			t.Fatal("legacy file unavailable after migration/restart:", err)
		}
		got, err := io.ReadAll(body)
		body.Close()
		if err != nil || !bytes.Equal(got, legacyBytes) {
			t.Fatalf("restart changed persisted legacy file: %v", err)
		}
	}
}

func TestUnavailableDependencies(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		unavailable, err := sql.Open("pgx", "postgres://test:test@127.0.0.1:1/filestorage_test?sslmode=disable&connect_timeout=1")
		if err != nil {
			t.Fatal(err)
		}
		defer unavailable.Close()
		svc := service(files.NewRepository(unavailable), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := svc.Ready(ctx); err == nil {
			t.Fatal("readiness accepted unavailable PostgreSQL")
		}
		server := api(t, svc)
		response := do(t, getRequest(t, http.MethodGet, server.URL+"/ready"))
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("readiness status %d", response.StatusCode)
		}
		body, _ := io.ReadAll(response.Body)
		if bytes.Contains(body, []byte("127.0.0.1:1")) || bytes.Contains(body, []byte("postgres://")) {
			t.Fatal("internal database error leaked to HTTP response")
		}
	})
	t.Run("minio", func(t *testing.T) {
		badConfig := testConfig
		badConfig.MinIOEndpoint = "127.0.0.1:1"
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := storage.NewClient(ctx, badConfig); err == nil {
			t.Fatal("startup accepted unavailable MinIO")
		}
		// A real MinIO bucket removed from the configured path is unavailable to
		// readiness even when the endpoint itself still answers requests.
		bucketMissing := testConfig
		bucketMissing.MinIOBucket = "filestorage-integration-missing-" + uuid.NewString()[:8]
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		// Authentication failure exercises real S3 error handling without
		// stopping or changing any container belonging to another test.
		bucketMissing.MinIOSecretKey = "deliberately-invalid-integration-secret"
		if _, err := storage.NewClient(ctx2, bucketMissing); err == nil {
			t.Fatal("startup accepted invalid MinIO credentials")
		}
	})
	t.Run("minio_becomes_unavailable", func(t *testing.T) {
		target, err := url.Parse("http://" + testConfig.MinIOEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		var offline atomic.Bool
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if offline.Load() {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `<Error><Code>ServiceUnavailable</Code><Message>injected outage</Message></Error>`)
				return
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(gateway.Close)
		cfg := testConfig
		cfg.MinIOEndpoint = strings.TrimPrefix(gateway.URL, "http://")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		proxiedStorage, err := storage.NewClient(ctx, cfg)
		cancel()
		if err != nil {
			t.Fatal("initialize real MinIO through fault gateway:", err)
		}
		svc := service(nil, proxiedStorage)
		offline.Store(true)
		ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
		defer cancel2()
		if err := svc.Ready(ctx2); err == nil {
			t.Fatal("readiness accepted a MinIO outage after startup")
		}
		server := api(t, svc)
		response := do(t, getRequest(t, http.MethodGet, server.URL+"/ready"))
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("readiness during MinIO outage status %d", response.StatusCode)
		}
	})
}

func TestMissingOrChangedS3ObjectFailsBeforeHTTP200(t *testing.T) {
	for _, change := range []string{"missing", "different_size"} {
		t.Run(change, func(t *testing.T) {
			server := api(t, service(nil, nil))
			file := httpUpload(t, server, "object.bin", []byte("initial persisted object bytes"))
			key := objectKey(t, file.ID)
			if change == "missing" {
				if err := testStorage.Delete(context.Background(), key); err != nil {
					t.Fatal(err)
				}
			} else {
				// Simulate storage changed outside this API. A valid S3 response
				// with the wrong length must not produce a misleading HTTP 200.
				if err := testStorage.Upload(context.Background(), key, strings.NewReader("short"), 5, "text/plain"); err != nil {
					t.Fatal(err)
				}
			}
			response := do(t, getRequest(t, http.MethodGet, server.URL+"/files/"+file.ID))
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("corrupt/missing S3 object download status %d", response.StatusCode)
			}
		})
	}
}

func TestConcurrentRecoveryClaimsBatchesWithoutLosingWork(t *testing.T) {
	marker := "batch-" + uuid.NewString()
	for range 40 {
		_, err := testDB.Exec(`INSERT INTO files (id,original_name,storage_key,content_type,size,state,updated_at,next_retry_at)
			VALUES ($1,$2,$3,'application/octet-stream',1,'deleting',now()-interval '1 hour',now()-interval '1 hour')`, uuid.NewString(), marker, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- service(nil, nil).Reconcile(context.Background())
		}()
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var remaining int
	if err := testDB.QueryRow(`SELECT count(*) FROM files WHERE original_name=$1 AND state<>'deleted'`, marker).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d records were skipped by concurrent recovery batches", remaining)
	}
}

func TestConsistencyDowngradeRefusesNonReadyRows(t *testing.T) {
	id := uuid.NewString()
	_, err := files.NewRepository(testDB).Create(context.Background(), files.CreateFileParams{ID: id, StorageKey: id, OriginalName: "pending.bin", ContentType: "application/octet-stream", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../migrations/002_file_lifecycle.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Execute the actual downgrade in its own connection. Its deliberate error
	// leaves a PostgreSQL transaction aborted, so always roll it back explicitly.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := testDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	defer connection.ExecContext(context.Background(), "ROLLBACK")
	if _, err := connection.ExecContext(ctx, string(script)); err == nil {
		t.Fatal("downgrade accepted pending/tombstone rows and could expose unavailable files")
	}
	if state(t, id) != "uploading" || state(t, legacyID) != "ready" {
		t.Fatal("refused downgrade changed existing file states")
	}
}

func TestConcurrentCleanupClaimsAreDisjoint(t *testing.T) {
	marker := "claim-" + uuid.NewString()
	want := make(map[string]bool)
	for range 64 {
		id := uuid.NewString()
		want[id] = true
		_, err := testDB.Exec(`INSERT INTO files (id,original_name,storage_key,content_type,size,state,updated_at,next_retry_at)
			VALUES ($1,$2,$3,'application/octet-stream',1,'deleting',now()-interval '7 days',now()-interval '7 days')`, id, marker, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		claimed []files.File
		err     error
	}
	claims := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			claimed, err := files.NewRepository(testDB).ClaimCleanup(context.Background(), files.UploadStaleAfter, 32)
			claims <- result{claimed: claimed, err: err}
		}()
	}
	close(start)
	seen := make(map[string]bool)
	for range 2 {
		batch := <-claims
		if batch.err != nil {
			t.Fatal(batch.err)
		}
		if len(batch.claimed) != 32 {
			t.Fatalf("claim batch length %d, want 32", len(batch.claimed))
		}
		for _, file := range batch.claimed {
			if !want[file.ID] {
				t.Fatal("claim skipped an older eligible record")
			}
			if seen[file.ID] {
				t.Fatal("concurrent workers claimed the same row")
			}
			seen[file.ID] = true
		}
	}
	if len(seen) != len(want) {
		t.Fatal("concurrent claims lost eligible work")
	}
	for id := range seen {
		if err := files.NewRepository(testDB).MarkDeleted(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

type appProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error // Written before done closes; read only after receiving done.
	logPath string
}

func startApp(t *testing.T, binary string) (*appProcess, string, []string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HTTP_PORT=" + port,
		"DATABASE_URL=" + testDatabaseURL,
		"MINIO_ENDPOINT=" + testConfig.MinIOEndpoint,
		"MINIO_ACCESS_KEY=" + testConfig.MinIOAccessKey,
		"MINIO_SECRET_KEY=" + testConfig.MinIOSecretKey,
		"MINIO_BUCKET=" + testConfig.MinIOBucket,
		"MINIO_USE_SSL=false",
		"MAX_FILE_SIZE_MB=1",
		"TMPDIR=" + t.TempDir(),
	}
	logPath := filepath.Join(t.TempDir(), "app.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary)
	command.Env = environment
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		logFile.Close()
		t.Fatal("start compiled application:", err)
	}
	process := &appProcess{command: command, done: make(chan struct{}), logPath: logPath}
	go func() {
		process.err = command.Wait()
		logFile.Close()
		close(process.done)
	}()
	t.Cleanup(func() {
		select {
		case <-process.done:
		default:
			command.Process.Kill()
			<-process.done
		}
	})
	endpoint := "http://127.0.0.1:" + port
	deadline := time.Now().Add(15 * time.Second)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint + "/ready")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process, endpoint, environment
			}
		}
		select {
		case <-process.done:
			logs, _ := os.ReadFile(logPath)
			t.Fatalf("application exited before readiness: %v\n%s", process.err, logs)
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("application never became ready")
	return nil, "", nil
}

func stopApp(t *testing.T, process *appProcess) {
	t.Helper()
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal("signal application:", err)
	}
	select {
	case <-process.done:
		if process.err != nil {
			logs, _ := os.ReadFile(process.logPath)
			t.Fatalf("SIGTERM did not exit cleanly: %v\n%s", process.err, logs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("graceful shutdown did not finish within 10s")
	}
}

func TestRealApplicationRestartAndGracefulSIGTERM(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "filestorage")
	build := exec.Command("go", "build", "-o", binary, "./cmd/app")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real application: %v\n%s", err, output)
	}
	first, endpoint, environment := startApp(t, binary)
	healthcheck := exec.Command(binary, "healthcheck")
	healthcheck.Env = environment
	if output, err := healthcheck.CombinedOutput(); err != nil {
		t.Fatalf("container healthcheck command failed: %v\n%s", err, output)
	}
	payload := []byte("bytes survive a real process restart\x00\xff\r\n")
	file := httpUpload(t, &httptest.Server{URL: endpoint}, "persistent.bin", payload)
	stopApp(t, first)
	second, restartedEndpoint, _ := startApp(t, binary)
	response := do(t, getRequest(t, http.MethodGet, restartedEndpoint+"/files/"+file.ID))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download after process restart status %d", response.StatusCode)
	}
	got, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("file changed across real process restart: %v", err)
	}
	deleted := do(t, getRequest(t, http.MethodDelete, restartedEndpoint+"/files/"+file.ID))
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after restart status %d", deleted.StatusCode)
	}
	stopApp(t, second)
}
