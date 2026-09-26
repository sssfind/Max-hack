package main

import (
	"Max-hack/internal/bot"
	"Max-hack/internal/catalog"
	"Max-hack/internal/handler"
	"Max-hack/internal/maxapi"
	"Max-hack/internal/pdfreport"
	"Max-hack/internal/vacancies"
	"Max-hack/internal/worker"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

	catalogPath := strings.TrimSpace(os.Getenv("SKILLGAP_CATALOG_PATH"))
	if catalogPath == "" {
		catalogPath = "spo_program_vacancy_map.json"
	}
	spoCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		slog.Error("Failed to load SPO catalog", "error", err, "path", catalogPath)
		os.Exit(1)
	}
	slog.Info("SPO catalog loaded", "programs", spoCatalog.Len(), "path", catalogPath)

	maxClient := maxapi.NewClient(botToken)
	vacancyClient, err := vacancies.NewClient(vacancies.ClientConfig{
		BaseURL:           envString("TRUDVSEM_BASE_URL", vacancies.DefaultBaseURL),
		RequestTimeout:    envDuration("TRUDVSEM_REQUEST_TIMEOUT", 25*time.Second),
		MaxResponseBytes:  int64(envInt("TRUDVSEM_MAX_RESPONSE_BYTES", 8<<20, 1<<20, 64<<20)),
		RequestsPerSecond: float64(envInt("TRUDVSEM_REQUESTS_PER_SECOND", 3, 1, 10)),
		MaxPagesPerQuery:  envInt("TRUDVSEM_MAX_PAGES_PER_QUERY", 2, 1, 10),
	})
	if err != nil {
		slog.Error("Failed to configure Работа России client", "error", err)
		os.Exit(1)
	}
	vacancyService := vacancies.NewService(vacancyClient, vacancies.ServiceOptions{
		MaxQueries:       envInt("SKILLGAP_MAX_QUERIES", 6, 1, 20),
		QueryConcurrency: envInt("SKILLGAP_QUERY_CONCURRENCY", 3, 1, 8),
		PerQueryLimit:    envInt("SKILLGAP_PER_QUERY_LIMIT", 100, 1, 100),
		MaxVacancies:     envInt("SKILLGAP_MAX_VACANCIES", 50, 1, 200),
	})
	reportGenerator := pdfreport.New(pdfreport.Config{
		MaxVacancies: envInt("SKILLGAP_PDF_MAX_VACANCIES", 15, 1, 30),
	})
	botHandler := bot.NewHandler(maxClient, spoCatalog, vacancyService, reportGenerator, bot.AnalysisConfig{
		Workers: envInt("SKILLGAP_ANALYSIS_WORKERS", 4, 1, 16),
		Queue:   envInt("SKILLGAP_ANALYSIS_QUEUE", 100, 1, 1000),
		Timeout: envDuration("SKILLGAP_ANALYSIS_TIMEOUT", 75*time.Second),
	})

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStartup()

	info, err := maxClient.GetMe(startupCtx)
	if err != nil {
		slog.Error("GET /me failed — check MAX_BOT_TOKEN and Минцифры CA", "error", err)
		os.Exit(1)
	}
	slog.Info("Bot authorized", "user_id", info.UserID, "name", info.FirstName, "username", info.Username)

	commands := []maxapi.BotCommand{
		{Name: "start", Description: "Начать диалог SkillGap"},
		{Name: "help", Description: "Справка по командам"},
		{Name: "skillgap", Description: "Выбрать направление (регион и СПО)"},
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
	analysisCtx, cancelAnalysis := context.WithCancel(context.Background())
	botHandler.StartAnalysisWorkers(analysisCtx)
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

	cancelAnalysis()
	analysisDone := make(chan struct{})
	go func() {
		botHandler.WaitAnalysisWorkers()
		close(analysisDone)
	}()
	select {
	case <-analysisDone:
	case <-time.After(10 * time.Second):
		slog.Warn("Analysis workers did not stop in time")
	}

	slog.Info("Server exited gracefully")
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback, minValue, maxValue int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		slog.Warn("Invalid integer environment value; using default", "name", name, "value", raw, "default", fallback)
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		slog.Warn("Invalid duration environment value; using default", "name", name, "value", raw, "default", fallback)
		return fallback
	}
	return value
}
