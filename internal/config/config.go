package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
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

func Load() (Config, error) {
	cfg := Config{
		HTTPPort: getEnv("HTTP_PORT", "8080"), DatabaseURL: os.Getenv("DATABASE_URL"),
		MinIOEndpoint: os.Getenv("MINIO_ENDPOINT"), MinIOAccessKey: os.Getenv("MINIO_ACCESS_KEY"),
		MinIOSecretKey: os.Getenv("MINIO_SECRET_KEY"), MinIOBucket: getEnv("MINIO_BUCKET", "files"),
	}
	for _, item := range []struct{ key, value string }{
		{"DATABASE_URL", cfg.DatabaseURL}, {"MINIO_ENDPOINT", cfg.MinIOEndpoint},
		{"MINIO_ACCESS_KEY", cfg.MinIOAccessKey}, {"MINIO_SECRET_KEY", cfg.MinIOSecretKey},
	} {
		if strings.TrimSpace(item.value) == "" {
			return Config{}, fmt.Errorf("%s is required", item.key)
		}
	}
	port, err := strconv.Atoi(cfg.HTTPPort)
	if err != nil || port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("HTTP_PORT must be an integer between 1 and 65535")
	}
	cfg.MinIOUseSSL, err = strconv.ParseBool(getEnv("MINIO_USE_SSL", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("MINIO_USE_SSL must be a boolean")
	}
	maxMB, err := strconv.ParseInt(getEnv("MAX_FILE_SIZE_MB", "20"), 10, 64)
	if err != nil || maxMB < 1 || maxMB > 5120 {
		return Config{}, fmt.Errorf("MAX_FILE_SIZE_MB must be between 1 and 5120 (single S3 PUT limit)")
	}
	cfg.MaxFileSizeBytes = maxMB * 1024 * 1024
	dbURL, err := url.Parse(cfg.DatabaseURL)
	if err != nil || dbURL.Host == "" || (dbURL.Scheme != "postgres" && dbURL.Scheme != "postgresql") {
		return Config{}, fmt.Errorf("DATABASE_URL must be a PostgreSQL connection URL")
	}
	endpoint, err := url.Parse("//" + cfg.MinIOEndpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return Config{}, fmt.Errorf("MINIO_ENDPOINT must be a host with optional port, without scheme or path")
	}
	return cfg, nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
