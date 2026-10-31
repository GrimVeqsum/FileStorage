package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"filestorage/internal/config"
	"filestorage/internal/files"
	"filestorage/internal/httptransport"
	"filestorage/internal/response"
	"filestorage/internal/storage"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		// Do not log DSNs or raw driver errors, which can include credentials.
		slog.Error("application stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func healthcheck() error {
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get("http://127.0.0.1:" + port + "/ready")
	if err != nil {
		return errors.New("readiness request failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return errors.New("service is not ready")
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err // Validation errors contain variable names, never values.
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startupCtx, cancelStartup := context.WithTimeout(signals, 30*time.Second)
	defer cancelStartup()

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return errors.New("open database failed")
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	fileRepository := files.NewRepository(db)
	if err = fileRepository.Ready(startupCtx); err != nil {
		return errors.New("database is unavailable or migrations have not completed")
	}
	minioClient, err := storage.NewClient(startupCtx, cfg)
	if err != nil {
		return errors.New("object storage initialization failed")
	}
	cancelStartup()
	fileService := files.NewService(fileRepository, minioClient, cfg.MaxFileSizeBytes)
	router := httptransport.NewRouter(files.NewHandler(fileService), fileService)
	var stopping atomic.Bool
	var requests sync.WaitGroup
	server := &http.Server{
		Addr: ":" + cfg.HTTPPort,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			defer requests.Done()
			if stopping.Load() && r.URL.Path == "/ready" {
				response.Error(w, http.StatusServiceUnavailable, "service_not_ready", "service is stopping")
				return
			}
			router.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       files.OperationTimeout,
		// Covers receiving a body, an S3 upload and writing its response.
		WriteTimeout:   2*files.OperationTimeout + 15*time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 32 << 10,
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		runReconciler(workerCtx, fileService)
	}()
	defer func() {
		cancelWorker()
		<-workerDone
	}()

	serveError := make(chan error, 1)
	go func() { serveError <- server.ListenAndServe() }()
	slog.Info("server started", "port", cfg.HTTPPort)
	select {
	case err = <-serveError:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("HTTP server failed")
		}
	case <-signals.Done():
	}
	stopping.Store(true)
	// A second signal can terminate immediately if a dependency is stuck.
	stop()
	slog.Info("server stopping")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		server.Close() // Cancels active request contexts and closes truncated transfers.
	}
	cancelWorker()
	<-workerDone
	// Allow independently bounded upload compensation to finish after forced close.
	drained := make(chan struct{})
	go func() { requests.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(files.CleanupTimeout + time.Second):
		return errors.New("shutdown handler drain timed out; durable cleanup will resume on restart")
	}
	if shutdownErr != nil {
		return errors.New("graceful shutdown timed out; connections closed")
	}
	slog.Info("server stopped")
	return nil
}

func runReconciler(ctx context.Context, service *files.Service) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := service.Reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.Error("file reconciliation failed; durable work will be retried")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
