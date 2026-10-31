package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"time"

	"github.com/google/uuid"
)

var (
	ErrFileNotFound       = errors.New("file not found")
	ErrEmptyFile          = errors.New("empty file")
	ErrFileTooLarge       = errors.New("file too large")
	ErrStorageUnavailable = errors.New("storage unavailable")
	ErrInvalidID          = errors.New("invalid file id")
	ErrOperationConflict  = errors.New("file operation conflict")
)

const (
	OperationTimeout = 2 * time.Minute
	CleanupTimeout   = 5 * time.Second
	// Recovery waits beyond the maximum foreground upload lifetime. Conditional
	// transitions and recurring tombstone cleanup also cover arbitrarily late PUTs.
	UploadStaleAfter = OperationTimeout + time.Minute
)

type FileStorage interface {
	Upload(ctx context.Context, storageKey string, file io.Reader, size int64, contentType string) error
	Download(ctx context.Context, storageKey string) (io.ReadCloser, error)
	Delete(ctx context.Context, storageKey string) error
	Ready(ctx context.Context) error
}

type Service struct {
	repository       MetadataRepository
	storage          FileStorage
	maxFileSizeBytes int64
}

func NewService(repository MetadataRepository, storage FileStorage, maxFileSizeBytes int64) *Service {
	return &Service{repository: repository, storage: storage, maxFileSizeBytes: maxFileSizeBytes}
}

func (service *Service) Upload(ctx context.Context, file multipart.File, header *multipart.FileHeader) (File, error) {
	if header.Size == 0 {
		return File{}, ErrEmptyFile
	}
	if header.Size < 0 || header.Size > service.maxFileSizeBytes {
		return File{}, ErrFileTooLarge
	}
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	id := uuid.NewString()
	// Commit the intent first. If its acknowledgement is lost, do not PUT: the
	// durable uploading record is harmless and will be recovered as stale.
	createdFile, err := service.repository.Create(ctx, CreateFileParams{
		ID: id, OriginalName: header.Filename, StorageKey: id,
		ContentType: contentType, Size: header.Size,
	})
	if err != nil {
		return File{}, err
	}
	if err = service.storage.Upload(ctx, id, file, header.Size, contentType); err != nil {
		service.compensateUpload(id)
		return File{}, fmt.Errorf("%w: upload: %v", ErrStorageUnavailable, err)
	}
	createdFile, err = service.repository.Activate(ctx, createdFile.ID)
	if err != nil {
		// Activate may actually have committed. Only an atomic uploading ->
		// deleting claim authorizes compensation, never the error alone.
		service.compensateUpload(id)
		return File{}, err
	}
	return createdFile, nil
}

func (service *Service) compensateUpload(id string) {
	// Request cancellation must not cancel compensation. Any failed step leaves
	// durable intent for reconciliation after this bounded attempt or a restart.
	ctx, cancel := context.WithTimeout(context.Background(), CleanupTimeout)
	defer cancel()
	claimed, err := service.repository.AbandonUpload(ctx, id)
	if err != nil {
		slog.Error("upload compensation claim failed", "id", id)
		return
	}
	if !claimed {
		return
	}
	if err = service.storage.Delete(ctx, id); err != nil {
		slog.Error("upload compensation delete failed", "id", id)
		return
	}
	if err = service.repository.MarkDeleted(ctx, id); err != nil {
		slog.Error("upload compensation completion failed", "id", id)
	}
}

func (service *Service) List(ctx context.Context) ([]File, error) {
	return service.repository.List(ctx)
}

func (service *Service) Download(ctx context.Context, id string) (File, io.ReadCloser, error) {
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return File{}, nil, ErrInvalidID
	}
	file, err := service.repository.GetByID(ctx, parsedID.String())
	if err != nil {
		return File{}, nil, err
	}
	body, err := service.storage.Download(ctx, file.StorageKey)
	if err != nil {
		return File{}, nil, fmt.Errorf("%w: download: %v", ErrStorageUnavailable, err)
	}
	return file, body, nil
}

func (service *Service) Delete(ctx context.Context, id string) error {
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return ErrInvalidID
	}
	id = parsedID.String()
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	file, err := service.repository.BeginDelete(ctx, id)
	if errors.Is(err, ErrFileNotFound) {
		return nil // DELETE of any absent UUID is idempotent.
	}
	if err != nil {
		return err
	}
	if err = service.storage.Delete(ctx, file.StorageKey); err != nil {
		return fmt.Errorf("%w: delete: %v", ErrStorageUnavailable, err)
	}
	return service.repository.MarkDeleted(ctx, id)
}

// Reconcile retries durable deletes and abandons expired uploads. It is safe to
// run in multiple processes; claims and upload activation serialize in PostgreSQL.
func (service *Service) Reconcile(ctx context.Context) error {
	claimCtx, cancelClaim := context.WithTimeout(ctx, CleanupTimeout)
	files, err := service.repository.ClaimCleanup(claimCtx, UploadStaleAfter, 32)
	cancelClaim()
	if err != nil {
		return err
	}
	var failures []error
	for _, file := range files {
		cleanupCtx, cancel := context.WithTimeout(ctx, CleanupTimeout)
		err = service.storage.Delete(cleanupCtx, file.StorageKey)
		if err == nil {
			err = service.repository.MarkDeleted(cleanupCtx, file.ID)
		}
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("reconcile file %s: %w", file.ID, err))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(failures...)
}

func (service *Service) Ready(ctx context.Context) error {
	if err := service.repository.Ready(ctx); err != nil {
		return err
	}
	return service.storage.Ready(ctx)
}
