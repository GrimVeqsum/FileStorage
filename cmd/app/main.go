package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"filestorage/internal/config"
	"filestorage/internal/files"
	"filestorage/internal/httptransport"
	"filestorage/internal/storage"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg := config.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		slog.Error("open database failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	err = db.PingContext(ctx)
	if err != nil {
		slog.Error("connect to database failed", "error", err)
		os.Exit(1)
	}

	minioClient, err := storage.NewClient(ctx, cfg)
	if err != nil {
		slog.Error("connect to minio failed", "error", err)
		os.Exit(1)
	}

	fileRepository := files.NewRepository(db)
	fileService := files.NewService(fileRepository, minioClient, cfg.MaxFileSizeBytes)
	fileHandler := files.NewHandler(fileService)

	router := httptransport.NewRouter(fileHandler, fileService)

	server := &http.Server{
		Addr:         ":" + cfg.HTTPPort,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("server started", "port", cfg.HTTPPort)

		err = server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-shutdownCtx.Done()

	slog.Info("server stopping")

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = server.Shutdown(ctx)
	if err != nil {
		slog.Error("server shutdown failed", "error", err)
		os.Exit(1)
	}

	slog.Info("server stopped")
}
