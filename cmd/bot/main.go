package main

import (
	"Max-hack/internal/bot"
	"Max-hack/internal/handler"
	"Max-hack/internal/maxapi"
	"Max-hack/internal/worker"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	webhookURL := strings.TrimSpace(os.Getenv("MAX_WEBHOOK_URL"))

	if dbURL == "" || botToken == "" || webhookSecret == "" {
		slog.Error("Missing required environment variables: DATABASE_URL, MAX_BOT_TOKEN, MAX_WEBHOOK_SECRET")
		os.Exit(1)
	}

	dbConfig, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		slog.Error("Failed to parse database URL", "error", err)
		os.Exit(1)
	}

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
	botHandler := bot.NewHandler(maxClient)

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStartup()

	info, err := maxClient.GetMe(startupCtx)
	if err != nil {
		slog.Error("GET /me failed — check MAX_BOT_TOKEN and Минцифры CA", "error", err)
		os.Exit(1)
	}
	slog.Info("Bot authorized", "user_id", info.UserID, "name", info.FirstName, "username", info.Username)

	commands := []maxapi.BotCommand{
		{Name: "start", Description: "Начать диалог"},
		{Name: "help", Description: "Справка по командам"},
	}
	if err := maxClient.SetCommands(startupCtx, commands); err != nil {
		slog.Warn("Failed to register bot commands", "error", err)
	} else {
		slog.Info("Bot commands registered", "count", len(commands))
	}

	if webhookURL != "" {
		if !strings.HasPrefix(webhookURL, "https://") {
			slog.Error("MAX_WEBHOOK_URL must be https:// on port 443", "url", webhookURL)
			os.Exit(1)
		}
		updateTypes := []string{
			maxapi.UpdateBotStarted,
			maxapi.UpdateBotAdded,
			maxapi.UpdateBotRemoved,
			maxapi.UpdateBotStopped,
			maxapi.UpdateMessageCreated,
			maxapi.UpdateMessageCallback,
		}
		if err := maxClient.Subscribe(startupCtx, webhookURL, webhookSecret, updateTypes); err != nil {
			slog.Error("Failed to subscribe webhook", "error", err, "url", webhookURL)
			os.Exit(1)
		}
		slog.Info("Webhook subscription OK", "url", webhookURL)
	} else {
		slog.Warn("MAX_WEBHOOK_URL is empty — subscribe manually via POST /subscriptions")
	}

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	pool := worker.NewPool(30, 10000, db, botHandler)
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

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("Shutting down server...")

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("Server forced to shutdown", "error", err)
	}

	cancelWorkers()
	done := make(chan struct{})
	go func() {
		pool.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		slog.Warn("Workers did not stop in time")
	}

	slog.Info("Server exited gracefully")
}
