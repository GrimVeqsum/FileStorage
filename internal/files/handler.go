package files

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"filestorage/internal/response"
)

// MaxMultipartOverhead bounds headers, boundaries and any trailing request data.
const MaxMultipartOverhead int64 = 1 << 20

// Spooling is bounded per request and per process, without file-sized Go buffers.
const maxConcurrentUploads = 8

type handlerService interface {
	Upload(context.Context, multipart.File, *multipart.FileHeader) (File, error)
	List(context.Context) ([]File, error)
	Download(context.Context, string) (File, io.ReadCloser, error)
	Delete(context.Context, string) error
}

type Handler struct {
	service          handlerService
	maxFileSizeBytes int64
	uploadSlots      chan struct{}
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service, maxFileSizeBytes: service.maxFileSizeBytes, uploadSlots: make(chan struct{}, maxConcurrentUploads)}
}

func (handler *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	select {
	case handler.uploadSlots <- struct{}{}:
		defer func() { <-handler.uploadSlots }()
	default:
		response.Error(w, http.StatusServiceUnavailable, "upload_busy", "too many concurrent uploads")
		return
	}

	maxBody := handler.maxFileSizeBytes + MaxMultipartOverhead
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	defer r.Body.Close()
	if r.ContentLength > maxBody {
		writeFileError(w, ErrFileTooLarge)
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		writeMultipartError(w, err)
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		writeMultipartError(w, err)
		return
	}
	if part.FormName() != "file" || part.FileName() == "" {
		response.Error(w, http.StatusBadRequest, "file_required", "a single file field is required")
		return
	}
	fileName, ok := safeFileName(part.FileName())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_file_name", "invalid file name")
		return
	}

	file, err := os.CreateTemp("", "filestorage-upload-*")
	if err != nil {
		writeFileError(w, err)
		return
	}
	// On Unix, unlink immediately: even SIGKILL cannot leave a named spool file.
	// Closing the descriptor releases its storage. Deferred removal is a fallback.
	_ = os.Remove(file.Name())
	defer os.Remove(file.Name())
	defer file.Close()

	size, err := io.Copy(file, io.LimitReader(part, handler.maxFileSizeBytes+1))
	if size > handler.maxFileSizeBytes {
		writeFileError(w, ErrFileTooLarge)
		return
	}
	if err != nil {
		writeMultipartError(w, err)
		return
	}
	if _, err = reader.NextPart(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("exactly one multipart part is required")
		}
		writeMultipartError(w, err)
		return
	}
	// MultipartReader may stop before an epilogue. Still enforce the whole-body limit.
	if _, err = io.Copy(io.Discard, r.Body); err != nil {
		writeMultipartError(w, err)
		return
	}
	if size == 0 {
		writeFileError(w, ErrEmptyFile)
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		writeFileError(w, err)
		return
	}
	var prefix [512]byte
	n, err := file.Read(prefix[:])
	if err != nil && !errors.Is(err, io.EOF) {
		writeFileError(w, err)
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		writeFileError(w, err)
		return
	}
	header := &multipart.FileHeader{Filename: fileName, Size: size, Header: make(textproto.MIMEHeader)}
	header.Header.Set("Content-Type", http.DetectContentType(prefix[:n]))
	createdFile, err := handler.service.Upload(r.Context(), file, header)
	if err != nil {
		writeFileError(w, err)
		return
	}
	w.Header().Set("Location", "/files/"+createdFile.ID)
	response.JSON(w, http.StatusCreated, createdFile)
}

func writeMultipartError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeFileError(w, ErrFileTooLarge)
		return
	}
	response.Error(w, http.StatusBadRequest, "invalid_multipart_form", "invalid multipart form; one file field is required")
}

func safeFileName(name string) (string, bool) {
	if !utf8.ValidString(name) || len(name) > 1024 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", false
	}
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	if name == "" || name == "." || name == ".." || name == "/" {
		return "", false
	}
	return name, true
}

func (handler *Handler) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), CleanupTimeout)
	defer cancel()
	files, err := handler.service.List(ctx)
	if err != nil {
		writeFileError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, files)
}

func (handler *Handler) Download(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), OperationTimeout)
	defer cancel()
	file, body, err := handler.service.Download(ctx, r.PathValue("id"))
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer body.Close()
	if file.Size < 0 {
		writeFileError(w, ErrStorageUnavailable)
		return
	}
	if sized, ok := body.(interface{ Size() int64 }); ok && sized.Size() != file.Size {
		writeFileError(w, ErrStorageUnavailable)
		return
	}
	name, ok := safeFileName(file.OriginalName)
	if !ok {
		// Legacy metadata can contain names that the new upload path rejects.
		name = "download"
	}
	contentType := file.ContentType
	if _, _, err := mime.ParseMediaType(contentType); err != nil || strings.ContainsAny(contentType, "\r\n") {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err = io.CopyN(w, body, file.Size); err != nil {
		slog.Error("download interrupted", "id", file.ID)
		// Headers are already committed. Close the transport so clients see truncation.
		panic(http.ErrAbortHandler)
	}
}

func (handler *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := handler.service.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidID):
		response.Error(w, http.StatusBadRequest, "invalid_file_id", "invalid file id")
	case errors.Is(err, ErrFileNotFound):
		response.Error(w, http.StatusNotFound, "file_not_found", "file not found")
	case errors.Is(err, ErrEmptyFile):
		response.Error(w, http.StatusBadRequest, "empty_file", "file is empty")
	case errors.Is(err, ErrFileTooLarge):
		response.Error(w, http.StatusRequestEntityTooLarge, "file_too_large", "file is too large")
	case errors.Is(err, ErrStorageUnavailable):
		response.Error(w, http.StatusServiceUnavailable, "storage_unavailable", "storage is unavailable")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		response.Error(w, http.StatusServiceUnavailable, "operation_interrupted", "operation interrupted; retry or check file list")
	default:
		// Drivers can include a DSN or credentials in their errors; do not echo them.
		slog.Error("file operation failed")
		response.Error(w, http.StatusServiceUnavailable, "service_unavailable", "file service is unavailable")
	}
}
