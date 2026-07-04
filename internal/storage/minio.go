package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"filestorage/internal/config"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Client struct {
	minioClient *minio.Client
	bucket      string
}

func NewClient(ctx context.Context, cfg config.Config) (*Client, error) {
	minioClient, err := minio.New(cfg.MinIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOUseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}

	client := &Client{
		minioClient: minioClient,
		bucket:      cfg.MinIOBucket,
	}

	err = client.createBucketIfNotExists(ctx)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func (client *Client) createBucketIfNotExists(ctx context.Context) error {
	exists, err := client.minioClient.BucketExists(ctx, client.bucket)
	if err != nil {
		return fmt.Errorf("minio unavailable: %w", err)
	}

	if exists {
		slog.Info("minio bucket already exists", "bucket", client.bucket)
		return nil
	}

	err = client.minioClient.MakeBucket(ctx, client.bucket, minio.MakeBucketOptions{})
	if err != nil {
		return fmt.Errorf("create minio bucket: %w", err)
	}

	slog.Info("minio bucket created", "bucket", client.bucket)
	return nil
}

func (client *Client) Upload(ctx context.Context, storageKey string, file io.Reader, size int64, contentType string) error {
	_, err := client.minioClient.PutObject(ctx, client.bucket, storageKey, file, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("upload file to minio: %w", err)
	}

	return nil
}

func (client *Client) Download(ctx context.Context, storageKey string) (io.ReadCloser, error) {
	object, err := client.minioClient.GetObject(ctx, client.bucket, storageKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("download file from minio: %w", err)
	}

	_, err = object.Stat()
	if err != nil {
		object.Close()
		return nil, fmt.Errorf("get file info from minio: %w", err)
	}

	return object, nil
}

func (client *Client) Delete(ctx context.Context, storageKey string) error {
	err := client.minioClient.RemoveObject(ctx, client.bucket, storageKey, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("delete file from minio: %w", err)
	}

	return nil
}

func (client *Client) Ready(ctx context.Context) error {
	_, err := client.minioClient.BucketExists(ctx, client.bucket)
	if err != nil {
		return fmt.Errorf("minio unavailable: %w", err)
	}

	return nil
}
