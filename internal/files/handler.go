package files

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"filestorage/internal/response"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (handler *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(32 << 20)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_multipart_form", "invalid multipart form")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		response.Error(w, http.StatusBadRequest, "file_required", "file field is required")
		return
	}
	defer file.Close()

	createdFile, err := handler.service.Upload(r.Context(), file, header)
	if err != nil {
		writeFileError(w, err)
		return
	}

	slog.Info("file uploaded", "id", createdFile.ID, "name", createdFile.OriginalName, "size", createdFile.Size)
	response.JSON(w, http.StatusCreated, createdFile)
}

func (handler *Handler) List(w http.ResponseWriter, r *http.Request) {
	files, err := handler.service.List(r.Context())
	if err != nil {
		slog.Error("get file list failed", "error", err)
		response.Error(w, http.StatusInternalServerError, "database_error", "could not get file list")
		return
	}

	response.JSON(w, http.StatusOK, files)
}

func (handler *Handler) Download(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	file, body, err := handler.service.Download(r.Context(), id)
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer body.Close()

	fileName := strings.ReplaceAll(file.OriginalName, `"`, "")

	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	w.WriteHeader(http.StatusOK)

	_, err = io.Copy(w, body)
	if err != nil {
		slog.Error("download file failed", "id", id, "error", err)
	}
}

func (handler *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	err := handler.service.Delete(r.Context(), id)
	if err != nil {
		writeFileError(w, err)
		return
	}

	slog.Info("file deleted", "id", id)
	response.JSON(w, http.StatusNoContent, nil)
}

func writeFileError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrFileNotFound) {
		response.Error(w, http.StatusNotFound, "file_not_found", "file not found")
		return
	}

	if errors.Is(err, ErrEmptyFile) {
		response.Error(w, http.StatusBadRequest, "empty_file", "file is empty")
		return
	}

	if errors.Is(err, ErrFileTooLarge) {
		response.Error(w, http.StatusRequestEntityTooLarge, "file_too_large", "file is too large")
		return
	}

	if errors.Is(err, ErrStorageUnavailable) {
		slog.Error("storage error", "error", err)
		response.Error(w, http.StatusServiceUnavailable, "storage_unavailable", "storage is unavailable")
		return
	}

	slog.Error("file operation failed", "error", err)
	response.Error(w, http.StatusInternalServerError, "internal_error", "internal server error")
}
