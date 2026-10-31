package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
)

type httpTestService struct {
	file File
	data []byte
	err  error
}

func (service *httpTestService) Upload(_ context.Context, body multipart.File, header *multipart.FileHeader) (File, error) {
	service.data, _ = io.ReadAll(body)
	service.file = File{ID: "56e05abc-398a-4466-842e-af5d8deaf290", OriginalName: header.Filename, Size: header.Size, ContentType: header.Header.Get("Content-Type")}
	return service.file, service.err
}
func (service *httpTestService) List(context.Context) ([]File, error) {
	return []File{service.file}, service.err
}
func (service *httpTestService) Download(context.Context, string) (File, io.ReadCloser, error) {
	return service.file, io.NopCloser(bytes.NewReader(service.data)), service.err
}
func (service *httpTestService) Delete(context.Context, string) error { return service.err }

func httpTestHandler(service *httpTestService, limit int64) *Handler {
	return &Handler{service: service, maxFileSizeBytes: limit, uploadSlots: make(chan struct{}, maxConcurrentUploads)}
}

func httpTestMultipart(t *testing.T, name, contents string, extra bool) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": name}))
	header.Set("Content-Type", "text/html; malicious=claimed")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(part, contents)
	if extra {
		writer.WriteField("extra", "ignored fields are rejected")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}

func TestHTTPUploadLimitsAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, contents string
		extra          bool
		status         int
		serviceError   error
	}{
		{"exact limit", strings.Repeat("x", 16), false, 201, nil},
		{"over limit", strings.Repeat("x", 17), false, 413, nil},
		{"empty", "", false, 400, nil},
		{"extra part", "x", true, 400, nil},
		{"database error", "x", false, 503, errors.New("postgres://secret:password@internal")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("TMPDIR", directory)
			service := &httpTestService{err: tc.serviceError}
			body, contentType := httpTestMultipart(t, "file.txt", tc.contents, tc.extra)
			r := httptest.NewRequest(http.MethodPost, "/files", body)
			r.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			httpTestHandler(service, 16).Upload(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			if tc.status == 201 && (string(service.data) != tc.contents || service.file.ContentType != "text/plain; charset=utf-8") {
				t.Fatalf("bad captured upload: %+v %q", service.file, service.data)
			}
			if strings.Contains(w.Body.String(), "password") {
				t.Fatal("internal error leaked")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files leaked: %v %v", entries, err)
			}
		})
	}
}

func TestHTTPBodyLimitIncludesEpilogue(t *testing.T) {
	body, contentType := httpTestMultipart(t, "ok.txt", "hello", false)
	body.WriteString(strings.Repeat("x", int(MaxMultipartOverhead)))
	request := httptest.NewRequest(http.MethodPost, "/files", body)
	request.ContentLength = -1 // Enforce streamed requests as well as declared lengths.
	request.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	httpTestHandler(&httpTestService{}, 16).Upload(w, request)
	if w.Code != 413 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func TestHTTPMalformedAndHeaderLimits(t *testing.T) {
	for _, tc := range []struct{ name, body, contentType string }{
		{"wrong media type", "content", "text/plain"},
		{"missing boundary", "content", "multipart/form-data"},
		{"truncated", "--test\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.txt\"\r\n\r\nhello", "multipart/form-data; boundary=test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("TMPDIR", directory)
			r := httptest.NewRequest(http.MethodPost, "/files", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			httpTestHandler(&httpTestService{}, 64).Upload(w, r)
			if w.Code != 400 {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			entries, _ := os.ReadDir(directory)
			if len(entries) != 0 {
				t.Fatal("malformed multipart leaked tempfile")
			}
		})
	}
}

func TestHTTPFilenameSafety(t *testing.T) {
	for _, name := range []string{"файл \"quote\";%.txt", "../../folder/name.txt", `C:\folder\name.txt`, "newline\r\nX-Injected: yes", "..", "bad\x00name"} {
		t.Run(name, func(t *testing.T) {
			body, contentType := httpTestMultipart(t, name, "hello", false)
			r := httptest.NewRequest(http.MethodPost, "/files", body)
			r.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			service := &httpTestService{}
			handler := httpTestHandler(service, 32)
			handler.Upload(w, r)
			if strings.ContainsAny(name, "\r\n\x00") || name == ".." {
				if w.Code != 400 {
					t.Fatalf("unsafe name accepted: %d", w.Code)
				}
				return
			}
			if w.Code != 201 {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if strings.ContainsAny(service.file.OriginalName, "/\\") {
				t.Fatal("path not stripped")
			}
			w = httptest.NewRecorder()
			handler.Download(w, httptest.NewRequest(http.MethodGet, "/files/id", nil))
			mediaType, parameters, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			if err != nil || mediaType != "attachment" || parameters["filename"] != service.file.OriginalName {
				t.Fatalf("bad disposition %q: %v", w.Header().Get("Content-Disposition"), err)
			}
			if w.Body.String() != "hello" || w.Header().Get("Content-Length") != "5" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("invalid download representation")
			}
		})
	}
}

func TestHTTPBusyUploadAndLateReadFailure(t *testing.T) {
	handler := httpTestHandler(&httpTestService{file: File{Size: 10, OriginalName: "x"}, data: []byte("short")}, 32)
	for range maxConcurrentUploads {
		handler.uploadSlots <- struct{}{}
	}
	w := httptest.NewRecorder()
	handler.Upload(w, httptest.NewRequest(http.MethodPost, "/files", nil))
	if w.Code != 503 {
		t.Fatalf("saturated uploads status %d", w.Code)
	}
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("expected transport abort, got %v", recovered)
		}
	}()
	handler.Download(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/files/id", nil))
}
