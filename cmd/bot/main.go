package main

import (
	"Max-hack/internal/handler"
	"Max-hack/internal/maxapi"
	"Max-hack/internal/worker"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	botToken := os.Getenv("MAX_BOT_TOKEN")
	webhookSecret := os.Getenv("MAX_WEBHOOK_SECRET")

	if dbURL == "" || botToken == "" || webhookSecret == "" {
		slog.Error("Missing required environment variables")
		os.Exit(1)
	}

	dbConfig, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		slog.Error("Failed to parse database URL", "error", err)
		os.Exit(1)
	}

	// Настройка пула соединений БД
	dbConfig.MaxConns = 25
	dbConfig.MinConns = 5
	dbConfig.MaxConnIdleTime = 5 * time.Minute

	db, err := pgxpool.NewWithConfig(context.Background(), dbConfig)
	if err != nil {
		slog.Error("Failed to create connection pool", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(context.Background()); err != nil {
		slog.Error("Database is unreachable", "error", err)
		os.Exit(1)
	}
	slog.Info("Connected to PostgreSQL")

	maxClient := maxapi.NewClient(botToken)

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	pool := worker.NewPool(30, 10000, db, maxClient)
	pool.Start(workerCtx)

	webhookHandler := handler.NewWebhookHandler(pool)
	mux := http.NewServeMux()

	secureWebhook := handler.WebhookSecretMiddleware(webhookSecret, webhookHandler.Handle)
	mux.HandleFunc("POST /webhook", secureWebhook)

	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	go func() {
		slog.Info("Starting HTTP server on :8080")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful Shutdown
	quit := make(chan os.Signal, 1)

	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("Shutting down server...")

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("Server forced to shutdown", "error", err)
	}

	// Отменяем контекст воркеров, чтобы они вышли из цикла
	cancelWorkers()
	// TODO
	// В идеале тут нужен sync.WaitGroup, чтобы дождаться остановки всех воркеров,
	// но для хакатона хватит небольшой паузы или доверия завершению процесса

	slog.Info("Server exited gracefully")
}
