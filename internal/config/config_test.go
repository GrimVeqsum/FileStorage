package config

import (
	"strings"
	"testing"
)

func testEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"HTTP_PORT": "8080", "DATABASE_URL": "postgres://local:secret@localhost/files?sslmode=disable",
		"MINIO_ENDPOINT": "localhost:9000", "MINIO_ACCESS_KEY": "local", "MINIO_SECRET_KEY": "secret",
		"MINIO_USE_SSL": "false", "MAX_FILE_SIZE_MB": "20", "MINIO_BUCKET": "files",
	} {
		t.Setenv(key, value)
	}
}

func TestLoadValidation(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"DATABASE_URL", ""}, {"DATABASE_URL", "postgres://secret:[invalid"},
		{"MINIO_SECRET_KEY", ""}, {"MINIO_ENDPOINT", "https://secret@example.com/path"},
		{"MINIO_USE_SSL", "typo"}, {"HTTP_PORT", "65536"},
		{"MAX_FILE_SIZE_MB", "-1"}, {"MAX_FILE_SIZE_MB", "5121"}, {"MAX_FILE_SIZE_MB", "9223372036854775807"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			testEnvironment(t)
			t.Setenv(tc.key, tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.key) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid configuration not safely rejected: %v", err)
			}
		})
	}
}

func TestLoadDefaultsAndSinglePutLimit(t *testing.T) {
	testEnvironment(t)
	t.Setenv("HTTP_PORT", "")
	t.Setenv("MAX_FILE_SIZE_MB", "5120")
	cfg, err := Load()
	if err != nil || cfg.HTTPPort != "8080" || cfg.MaxFileSizeBytes != 5*1024*1024*1024 {
		t.Fatalf("unexpected config %+v: %v", cfg, err)
	}
}
