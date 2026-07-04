package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"path/filepath"

	"github.com/google/uuid"
)

var (
	ErrFileNotFound       = errors.New("file not found")
	ErrEmptyFile          = errors.New("empty file")
	ErrFileTooLarge       = errors.New("file too large")
	ErrStorageUnavailable = errors.New("storage unavailable")
)

type FileStorage interface {
	Upload(ctx context.Context, storageKey string, file io.Reader, size int64, contentType string) error
	Download(ctx context.Context, storageKey string) (io.ReadCloser, error)
	Delete(ctx context.Context, storageKey string) error
	Ready(ctx context.Context) error
}

type Service struct {
	repository       *Repository
	storage          FileStorage
	maxFileSizeBytes int64
}

func NewService(repository *Repository, storage FileStorage, maxFileSizeBytes int64) *Service {
	return &Service{
		repository:       repository,
		storage:          storage,
		maxFileSizeBytes: maxFileSizeBytes,
	}
}

func (service *Service) Upload(ctx context.Context, file multipart.File, header *multipart.FileHeader) (File, error) {
	if header.Size == 0 {
		return File{}, ErrEmptyFile
	}

	if header.Size > service.maxFileSizeBytes {
		return File{}, ErrFileTooLarge
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	storageKey := uuid.NewString() + filepath.Ext(header.Filename)

	err := service.storage.Upload(ctx, storageKey, file, header.Size, contentType)
	if err != nil {
		return File{}, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}

	createdFile, err := service.repository.Create(ctx, CreateFileParams{
		OriginalName: header.Filename,
		StorageKey:   storageKey,
		ContentType:  contentType,
		Size:         header.Size,
	})
	if err != nil {
		_ = service.storage.Delete(ctx, storageKey)
		return File{}, err
	}

	return createdFile, nil
}

func (service *Service) List(ctx context.Context) ([]File, error) {
	return service.repository.List(ctx)
}

func (service *Service) Download(ctx context.Context, id string) (File, io.ReadCloser, error) {
	file, err := service.repository.GetByID(ctx, id)
	if err != nil {
		return File{}, nil, err
	}

	body, err := service.storage.Download(ctx, file.StorageKey)
	if err != nil {
		return File{}, nil, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}

	return file, body, nil
}

func (service *Service) Delete(ctx context.Context, id string) error {
	file, err := service.repository.GetByID(ctx, id)
	if err != nil {
		return err
	}

	err = service.storage.Delete(ctx, file.StorageKey)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}

	return service.repository.Delete(ctx, id)
}

func (service *Service) Ready(ctx context.Context) error {
	if err := service.repository.Ready(ctx); err != nil {
		return err
	}

	if err := service.storage.Ready(ctx); err != nil {
		return err
	}

	return nil
}
