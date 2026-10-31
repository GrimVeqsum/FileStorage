package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type File struct {
	ID           string    `json:"id"`
	OriginalName string    `json:"original_name"`
	StorageKey   string    `json:"-"`
	ContentType  string    `json:"content_type"`
	Size         int64     `json:"size"`
	CreatedAt    time.Time `json:"created_at"`
	State        string    `json:"-"`
}

type CreateFileParams struct {
	ID           string
	OriginalName string
	StorageKey   string
	ContentType  string
	Size         int64
}

// MetadataRepository keeps durable operation intent before touching object storage.
// State transitions must be conditional: an uncertain activation must never cause
// a compensation to delete an object whose metadata is already ready.
type MetadataRepository interface {
	Create(context.Context, CreateFileParams) (File, error)
	Activate(context.Context, string) (File, error)
	AbandonUpload(context.Context, string) (bool, error)
	List(context.Context) ([]File, error)
	GetByID(context.Context, string) (File, error)
	BeginDelete(context.Context, string) (File, error)
	MarkDeleted(context.Context, string) error
	ClaimCleanup(context.Context, time.Duration, int) ([]File, error)
	Ready(context.Context) error
}

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

const fileColumns = "id, original_name, storage_key, content_type, size, created_at, state"

type scanner interface{ Scan(...any) error }

func scanFile(row scanner) (File, error) {
	var file File
	err := row.Scan(&file.ID, &file.OriginalName, &file.StorageKey, &file.ContentType, &file.Size, &file.CreatedAt, &file.State)
	return file, err
}

func (repository *Repository) Create(ctx context.Context, params CreateFileParams) (File, error) {
	file, err := scanFile(repository.db.QueryRowContext(ctx, `
		INSERT INTO files (id, original_name, storage_key, content_type, size, state)
		VALUES ($1, $2, $3, $4, $5, 'uploading') RETURNING `+fileColumns,
		params.ID, params.OriginalName, params.StorageKey, params.ContentType, params.Size))
	if err != nil {
		return File{}, fmt.Errorf("create file metadata: %w", err)
	}
	return file, nil
}

func (repository *Repository) Activate(ctx context.Context, id string) (File, error) {
	file, err := scanFile(repository.db.QueryRowContext(ctx, `
		UPDATE files SET state = 'ready', updated_at = now()
		WHERE id = $1 AND state = 'uploading' RETURNING `+fileColumns, id))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrOperationConflict
	}
	if err != nil {
		return File{}, fmt.Errorf("activate file metadata: %w", err)
	}
	return file, nil
}

// AbandonUpload may only claim an uncommitted upload. A false result means the
// object must be left alone: activation may already have committed.
func (repository *Repository) AbandonUpload(ctx context.Context, id string) (bool, error) {
	result, err := repository.db.ExecContext(ctx, `
		UPDATE files SET state = 'deleting', updated_at = now(), next_retry_at = now()
		WHERE id = $1 AND state = 'uploading'`, id)
	if err != nil {
		return false, fmt.Errorf("abandon upload: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (repository *Repository) List(ctx context.Context) ([]File, error) {
	rows, err := repository.db.QueryContext(ctx, `SELECT `+fileColumns+`
		FROM files WHERE state = 'ready' ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("get file list: %w", err)
	}
	defer rows.Close()
	return scanFiles(rows)
}

func scanFiles(rows *sql.Rows) ([]File, error) {
	files := make([]File, 0)
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read file rows: %w", err)
	}
	return files, nil
}

func (repository *Repository) GetByID(ctx context.Context, id string) (File, error) {
	file, err := scanFile(repository.db.QueryRowContext(ctx, `SELECT `+fileColumns+`
		FROM files WHERE id = $1 AND state = 'ready'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrFileNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("get file by id: %w", err)
	}
	return file, nil
}

func (repository *Repository) BeginDelete(ctx context.Context, id string) (File, error) {
	// Lock and update in one statement. Concurrent deletes all retain the same key,
	// including tombstones, so repeating S3 DELETE is safe.
	file, err := scanFile(repository.db.QueryRowContext(ctx, `
		UPDATE files SET
			state = CASE WHEN state = 'deleted' THEN 'deleted' ELSE 'deleting' END,
			updated_at = now(), next_retry_at = now()
		WHERE id = $1 RETURNING `+fileColumns, id))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrFileNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("begin file deletion: %w", err)
	}
	return file, nil
}

func (repository *Repository) MarkDeleted(ctx context.Context, id string) error {
	result, err := repository.db.ExecContext(ctx, `
		UPDATE files SET state = 'deleted', updated_at = now(), next_retry_at = now() + interval '5 minutes'
		WHERE id = $1 AND state IN ('deleting', 'deleted')`, id)
	if err != nil {
		return fmt.Errorf("finish file deletion: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrOperationConflict
	}
	return nil
}

func (repository *Repository) ClaimCleanup(ctx context.Context, staleAfter time.Duration, limit int) ([]File, error) {
	// A short atomic lease distributes work between replicas. Tombstones are never
	// purged: a PUT whose outcome was unknown may finish after an earlier DELETE.
	// Repeating cleanup eventually removes even that late object.
	rows, err := repository.db.QueryContext(ctx, `
		WITH candidates AS (
			SELECT id FROM files
			WHERE (state = 'uploading' AND updated_at < now() - $1 * interval '1 second')
			   OR (state IN ('deleting', 'deleted') AND next_retry_at <= now())
			ORDER BY next_retry_at, id LIMIT $2 FOR UPDATE SKIP LOCKED
		)
		UPDATE files AS f SET state = 'deleting', updated_at = now(),
			next_retry_at = now() + $1 * interval '1 second'
		FROM candidates AS c WHERE f.id = c.id
		RETURNING f.id, f.original_name, f.storage_key, f.content_type, f.size, f.created_at, f.state`, staleAfter.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim file cleanup: %w", err)
	}
	defer rows.Close()
	return scanFiles(rows)
}

func (repository *Repository) Ready(ctx context.Context) error {
	// A successful ping alone would report ready before migration 002 has run.
	var count int
	return repository.db.QueryRowContext(ctx, `SELECT count(*) FROM (
		SELECT state, updated_at, next_retry_at FROM files LIMIT 0
	) AS schema_check`).Scan(&count)
}
