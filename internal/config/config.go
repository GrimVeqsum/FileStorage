package config

import (
	"log/slog"
	"os"
	"strconv"
)

type Config struct {
	HTTPPort         string
	DatabaseURL      string
	MinIOEndpoint    string
	MinIOAccessKey   string
	MinIOSecretKey   string
	MinIOBucket      string
	MinIOUseSSL      bool
	MaxFileSizeBytes int64
}

func Load() Config {
	minIOUseSSL, err := strconv.ParseBool(getEnv("MINIO_USE_SSL", "false"))
	if err != nil {
		minIOUseSSL = false
	}

	maxFileSizeMB, err := strconv.ParseInt(getEnv("MAX_FILE_SIZE_MB", "20"), 10, 64)
	if err != nil || maxFileSizeMB <= 0 {
		maxFileSizeMB = 20
	}

	return Config{
		HTTPPort:         getEnv("HTTP_PORT", "8080"),
		DatabaseURL:      requireEnv("DATABASE_URL"),
		MinIOEndpoint:    requireEnv("MINIO_ENDPOINT"),
		MinIOAccessKey:   requireEnv("MINIO_ACCESS_KEY"),
		MinIOSecretKey:   requireEnv("MINIO_SECRET_KEY"),
		MinIOBucket:      getEnv("MINIO_BUCKET", "files"),
		MinIOUseSSL:      minIOUseSSL,
		MaxFileSizeBytes: maxFileSizeMB * 1024 * 1024,
	}
}

func getEnv(key string, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}

	return value
}

func requireEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		slog.Error("required environment variable is empty", "key", key)
		os.Exit(1)
	}

	return value
}
