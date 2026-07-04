package httptransport

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"filestorage/internal/files"
	"filestorage/internal/response"
)

func NewRouter(fileHandler *files.Handler, fileService *files.Service) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		response.JSON(w, http.StatusOK, map[string]string{
			"status": "ok",
		})
	})

	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		err := fileService.Ready(ctx)
		if err != nil {
			slog.Error("readiness check failed", "error", err)
			response.Error(w, http.StatusServiceUnavailable, "service_not_ready", "service is not ready")
			return
		}

		response.JSON(w, http.StatusOK, map[string]string{
			"status": "ready",
		})
	})

	mux.HandleFunc("POST /files", fileHandler.Upload)
	mux.HandleFunc("GET /files", fileHandler.List)
	mux.HandleFunc("GET /files/{id}", fileHandler.Download)
	mux.HandleFunc("DELETE /files/{id}", fileHandler.Delete)

	return loggingMiddleware(mux)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		next.ServeHTTP(w, r)

		slog.Info(
			"http request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
