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
}

type CreateFileParams struct {
	OriginalName string
	StorageKey   string
	ContentType  string
	Size         int64
}

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (repository *Repository) Create(ctx context.Context, params CreateFileParams) (File, error) {
	query := `
		INSERT INTO files (original_name, storage_key, content_type, size)
		VALUES ($1, $2, $3, $4)
		RETURNING id, original_name, storage_key, content_type, size, created_at
	`

	var file File

	err := repository.db.QueryRowContext(
		ctx,
		query,
		params.OriginalName,
		params.StorageKey,
		params.ContentType,
		params.Size,
	).Scan(
		&file.ID,
		&file.OriginalName,
		&file.StorageKey,
		&file.ContentType,
		&file.Size,
		&file.CreatedAt,
	)

	if err != nil {
		return File{}, fmt.Errorf("create file metadata: %w", err)
	}

	return file, nil
}

func (repository *Repository) List(ctx context.Context) ([]File, error) {
	query := `
		SELECT id, original_name, storage_key, content_type, size, created_at
		FROM files
		ORDER BY created_at DESC
	`

	rows, err := repository.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("get file list: %w", err)
	}
	defer rows.Close()

	files := make([]File, 0)

	for rows.Next() {
		var file File

		err = rows.Scan(
			&file.ID,
			&file.OriginalName,
			&file.StorageKey,
			&file.ContentType,
			&file.Size,
			&file.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}

		files = append(files, file)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read file rows: %w", err)
	}

	return files, nil
}

func (repository *Repository) GetByID(ctx context.Context, id string) (File, error) {
	query := `
		SELECT id, original_name, storage_key, content_type, size, created_at
		FROM files
		WHERE id = $1
	`

	var file File

	err := repository.db.QueryRowContext(ctx, query, id).Scan(
		&file.ID,
		&file.OriginalName,
		&file.StorageKey,
		&file.ContentType,
		&file.Size,
		&file.CreatedAt,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrFileNotFound
	}

	if err != nil {
		return File{}, fmt.Errorf("get file by id: %w", err)
	}

	return file, nil
}

func (repository *Repository) Delete(ctx context.Context, id string) error {
	query := `DELETE FROM files WHERE id = $1`

	result, err := repository.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete file metadata: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get deleted rows count: %w", err)
	}

	if rowsAffected == 0 {
		return ErrFileNotFound
	}

	return nil
}

func (repository *Repository) Ready(ctx context.Context) error {
	return repository.db.PingContext(ctx)
}
