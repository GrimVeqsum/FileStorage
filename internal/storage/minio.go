package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"filestorage/internal/config"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Client struct {
	minioClient *minio.Client
	bucket      string
}

func NewClient(ctx context.Context, cfg config.Config) (*Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 10 * time.Second
	transport.MaxConnsPerHost = 32
	minioClient, err := minio.New(cfg.MinIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOUseSSL, Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}
	client := &Client{minioClient: minioClient, bucket: cfg.MinIOBucket}
	if err = client.createBucketIfNotExists(ctx); err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return client, nil
}

func (client *Client) createBucketIfNotExists(ctx context.Context) error {
	exists, err := client.minioClient.BucketExists(ctx, client.bucket)
	if err != nil {
		return fmt.Errorf("check minio bucket: %w", err)
	}
	if exists {
		return nil
	}
	if err = client.minioClient.MakeBucket(ctx, client.bucket, minio.MakeBucketOptions{}); err != nil {
		code := minio.ToErrorResponse(err).Code
		if code != "BucketAlreadyOwnedByYou" && code != "BucketAlreadyExists" {
			return fmt.Errorf("create minio bucket: %w", err)
		}
		return client.Ready(ctx)
	}
	return nil
}

func validStorageKey(key string) bool {
	if key == "" || key == "." || key == ".." || len(key) > 1024 || strings.ContainsAny(key, "/\r\n\x00") {
		return false
	}
	if strings.ContainsRune(key, '\\') {
		// The old Linux UUID + filepath.Ext generator could retain a literal
		// backslash in an extension. Accept only that identifiable legacy shape;
		// the SDK encodes it as an S3 key, never a filesystem path.
		if len(key) < 38 || key[36] != '.' {
			return false
		}
		if _, err := uuid.Parse(key[:36]); err != nil {
			return false
		}
	}
	return true
}

func (client *Client) Upload(ctx context.Context, storageKey string, file io.Reader, size int64, contentType string) error {
	if !validStorageKey(storageKey) || size < 0 || size > 5*1024*1024*1024 {
		return errors.New("invalid object key or size")
	}
	// A single streaming PUT avoids abandoned multipart-upload parts on process
	// death. Config deliberately caps uploads at S3's 5 GiB single-PUT limit.
	_, readerAt := file.(io.ReaderAt)
	_, seeker := file.(io.Seeker)
	info, err := client.minioClient.PutObject(ctx, client.bucket, storageKey, file, size, minio.PutObjectOptions{
		ContentType: contentType, DisableMultipart: true,
		// The HTTP spool implements both. Avoid SDK file-sized buffering for
		// generic non-seekable readers while verifying HTTP upload integrity.
		SendContentMd5: readerAt && seeker,
	})
	if err != nil {
		return fmt.Errorf("upload file to minio: %w", err)
	}
	if info.Size != size {
		return errors.New("uploaded object size mismatch")
	}
	return nil
}

type prefetchedObject struct {
	io.Reader
	object *minio.Object
	size   int64
}

func (body *prefetchedObject) Close() error { return body.object.Close() }
func (body *prefetchedObject) Size() int64  { return body.size }

func (client *Client) Download(ctx context.Context, storageKey string) (io.ReadCloser, error) {
	if !validStorageKey(storageKey) {
		return nil, errors.New("invalid object key")
	}
	object, err := client.minioClient.GetObject(ctx, client.bucket, storageKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("download file from minio: %w", err)
	}
	info, err := object.Stat()
	if err != nil {
		object.Close()
		return nil, fmt.Errorf("stat object: %w", err)
	}
	// GetObject is lazy: a successful HEAD alone is insufficient. Force the first
	// GET/read before HTTP headers are committed. Later network failures can only
	// be reported through a truncated response, not a different status code.
	if info.Size < 0 {
		object.Close()
		return nil, errors.New("invalid object size")
	}
	prefix := make([]byte, min(info.Size, 512))
	if len(prefix) != 0 {
		if _, err = io.ReadFull(object, prefix); err != nil {
			object.Close()
			return nil, fmt.Errorf("read object prefix: %w", err)
		}
	}
	return &prefetchedObject{Reader: io.MultiReader(bytes.NewReader(prefix), object), object: object, size: info.Size}, nil
}

func (client *Client) Delete(ctx context.Context, storageKey string) error {
	if !validStorageKey(storageKey) {
		return errors.New("invalid object key")
	}
	if err := client.minioClient.RemoveObject(ctx, client.bucket, storageKey, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete file from minio: %w", err)
	}
	return nil
}

func (client *Client) Ready(ctx context.Context) error {
	exists, err := client.minioClient.BucketExists(ctx, client.bucket)
	if err != nil {
		return fmt.Errorf("minio unavailable: %w", err)
	}
	if !exists {
		return errors.New("minio bucket is missing")
	}
	return nil
}
