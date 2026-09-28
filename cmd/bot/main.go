package main

import (
	"Max-hack/internal/bot"
	"Max-hack/internal/catalog"
	"Max-hack/internal/handler"
	"Max-hack/internal/maxapi"
	"Max-hack/internal/pdfreport"
	"Max-hack/internal/skillgap"
	"Max-hack/internal/vacancies"
	"Max-hack/internal/worker"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var webhookSecretPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{5,256}$`)

type webhookSubscriber interface {
	Subscribe(context.Context, string, string, []string) error
}

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	botToken := os.Getenv("MAX_BOT_TOKEN")
	webhookSecret := os.Getenv("MAX_WEBHOOK_SECRET")
	webhookURL := strings.TrimSpace(os.Getenv("MAX_WEBHOOK_URL"))

	if dbURL == "" || botToken == "" || webhookSecret == "" {
		slog.Error("Missing required environment variables: DATABASE_URL, MAX_BOT_TOKEN, MAX_WEBHOOK_SECRET")
		os.Exit(1)
	}
	if !webhookSecretPattern.MatchString(webhookSecret) {
		slog.Error("MAX_WEBHOOK_SECRET must match ^[a-zA-Z0-9_-]{5,256}$")
		os.Exit(1)
	}

	dbConfig, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		// Parser errors can echo the DSN, so never attach them to logs.
		slog.Error("Failed to parse DATABASE_URL")
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
	validation := spoCatalog.ValidationReport()
	slog.Info("SPO catalog loaded", "programs", spoCatalog.Len(), "warnings", validation.WarningCount(), "pilot_programs", len(spoCatalog.PilotPrograms()), "path", catalogPath)

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
		CacheMaxBytes:    int64(envInt("SKILLGAP_VACANCY_CACHE_MAX_BYTES", 32<<20, 1<<20, 256<<20)),
	})
	reportGenerator := pdfreport.New(pdfreport.Config{
		MaxVacancies: envInt("SKILLGAP_PDF_MAX_VACANCIES", 15, 1, 30),
	})
	programAnalyzer := skillgap.New(skillgap.Options{
		Timeout:           envDuration("SKILLGAP_DOCUMENT_TIMEOUT", 20*time.Second),
		PDFParseTimeout:   envDuration("SKILLGAP_PDF_PARSE_TIMEOUT", 15*time.Second),
		PDFMaxMemoryBytes: int64(envInt("SKILLGAP_PDF_MAX_MEMORY_BYTES", 512<<20, 128<<20, 1<<30)),
		CacheTTL:          envDuration("SKILLGAP_DOCUMENT_CACHE_TTL", 30*time.Minute),
		MaxCacheBytes:     int64(envInt("SKILLGAP_DOCUMENT_CACHE_MAX_BYTES", 32<<20, 1<<20, 256<<20)),
		MaxDownloadBytes:  int64(envInt("SKILLGAP_DOCUMENT_MAX_BYTES", 20<<20, 1<<20, 64<<20)),
		MaxPages:          envInt("SKILLGAP_DOCUMENT_MAX_PAGES", 300, 10, 1000),
		MaxSkills:         envInt("SKILLGAP_DOCUMENT_MAX_SKILLS", 100, 1, 200),
	})
	botHandler := bot.NewHandlerWithSessionRepository(maxClient, spoCatalog, vacancyService, reportGenerator, bot.AnalysisConfig{
		Workers:       envInt("SKILLGAP_ANALYSIS_WORKERS", 4, 1, 16),
		Queue:         envInt("SKILLGAP_ANALYSIS_QUEUE", 100, 1, 1000),
		Timeout:       envDuration("SKILLGAP_ANALYSIS_TIMEOUT", 90*time.Second),
		SkillGap:      programAnalyzer,
		CatalogPolicy: catalogPolicy(envString("SKILLGAP_CATALOG_POLICY", string(catalog.ReviewPolicyPilot))),
	}, bot.NewPostgresSessionRepository(db), envDuration("SKILLGAP_SESSION_TTL", 24*time.Hour))

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStartup()

	_, err = maxClient.GetMe(startupCtx)
	if err != nil {
		slog.Error("GET /me failed — check MAX_BOT_TOKEN and Минцифры CA", "error", err)
		os.Exit(1)
	}
	slog.Info("Bot authorized")

	commands := []maxapi.BotCommand{
		{Name: "start", Description: "Начать диалог SkillGap"},
		{Name: "help", Description: "Справка по командам"},
		{Name: "privacy", Description: "Какие данные использует бот"},
		{Name: "forget", Description: "Удалить сохранённый выбор"},
		{Name: "skillgap", Description: "Выбрать направление (регион и СПО)"},
	}
	if err := maxClient.SetCommands(startupCtx, commands); err != nil {
		slog.Warn("Failed to register bot commands", "error", err)
	} else {
		slog.Info("Bot commands registered", "count", len(commands))
	}

	webhookUpdateTypes := []string{
		maxapi.UpdateBotStarted,
		maxapi.UpdateBotAdded,
		maxapi.UpdateBotRemoved,
		maxapi.UpdateBotStopped,
		maxapi.UpdateMessageCreated,
		maxapi.UpdateMessageCallback,
	}
	if webhookURL != "" {
		if !validWebhookURL(webhookURL) {
			// Do not log the raw value: a malformed URL may contain credentials
			// or a token in its query string.
			slog.Error("MAX_WEBHOOK_URL must be an exact https://host/webhook URL on port 443 without credentials, query, or fragment")
			os.Exit(1)
		}
		if err := maxClient.Subscribe(startupCtx, webhookURL, webhookSecret, webhookUpdateTypes); err != nil {
			slog.Error("Failed to subscribe webhook", "error", err, "url", webhookURL)
			os.Exit(1)
		}
		slog.Info("Webhook subscription OK", "url", webhookURL)
	} else {
		slog.Warn("MAX_WEBHOOK_URL is empty — subscribe manually via POST /subscriptions")
	}

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	if webhookURL != "" {
		go maintainWebhookSubscription(
			workerCtx,
			maxClient,
			webhookURL,
			webhookSecret,
			webhookUpdateTypes,
			envDuration("MAX_WEBHOOK_RECONCILE_INTERVAL", 30*time.Minute),
		)
	}
	analysisCtx, cancelAnalysis := context.WithCancel(context.Background())
	botHandler.StartAnalysisWorkers(analysisCtx)
	workerConfig := worker.DefaultConfig()
	workerConfig.HandlerTimeout = envDuration("SKILLGAP_EVENT_HANDLER_TIMEOUT", workerConfig.HandlerTimeout)
	workerConfig.ProcessingTimeout = envDuration("SKILLGAP_EVENT_PROCESSING_TIMEOUT", workerConfig.ProcessingTimeout)
	workerConfig.RecoveryInterval = envDuration("SKILLGAP_EVENT_RECOVERY_INTERVAL", workerConfig.RecoveryInterval)
	workerConfig.CleanupInterval = envDuration("SKILLGAP_EVENT_CLEANUP_INTERVAL", workerConfig.CleanupInterval)
	workerConfig.CompletedTTL = envDuration("SKILLGAP_EVENT_COMPLETED_TTL", workerConfig.CompletedTTL)
	workerConfig.FailedTTL = envDuration("SKILLGAP_EVENT_FAILED_TTL", workerConfig.FailedTTL)
	workerConfig.MaxAttempts = envInt("SKILLGAP_EVENT_MAX_ATTEMPTS", workerConfig.MaxAttempts, 1, 100)
	workerConfig.RecoveryBatchSize = envInt("SKILLGAP_EVENT_RECOVERY_BATCH", workerConfig.RecoveryBatchSize, 1, 5000)
	pool := worker.NewPoolWithOptions(
		// Keep five of the 25 database connections available for webhook
		// ingestion, readiness checks and analysis while workers hold chat locks.
		envInt("SKILLGAP_EVENT_WORKERS", 20, 1, 20),
		envInt("SKILLGAP_EVENT_QUEUE", 10000, 1, 100000),
		db,
		botHandler,
		workerConfig,
	)
	pool.Start(workerCtx)

	webhookHandler := handler.NewWebhookHandlerWithLimit(
		pool,
		int64(envInt("MAX_WEBHOOK_MAX_BODY_BYTES", int(handler.DefaultMaxWebhookBodyBytes), 1024, 4<<20)),
	)
	mux := http.NewServeMux()

	secureWebhook := handler.WebhookSecretMiddleware(webhookSecret, webhookHandler.Handle)
	mux.HandleFunc("POST /webhook", secureWebhook)
	mux.HandleFunc("GET /healthz", handler.Health)
	mux.HandleFunc("GET /readyz", handler.Readiness(handler.CombineReadiness(pool, botHandler), time.Second))
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

func maintainWebhookSubscription(
	ctx context.Context,
	client webhookSubscriber,
	webhookURL string,
	secret string,
	updateTypes []string,
	interval time.Duration,
) {
	if client == nil || strings.TrimSpace(webhookURL) == "" {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := client.Subscribe(attemptCtx, webhookURL, secret, updateTypes)
			cancel()
			if err != nil {
				slog.Error("Failed to reconcile webhook subscription", "error", err, "url", webhookURL)
				continue
			}
			slog.Info("Webhook subscription reconciled", "url", webhookURL)
		}
	}
}

func validWebhookURL(raw string) bool {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.EscapedPath() != "/webhook" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Port() == "" || parsed.Port() == "443"
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

func catalogPolicy(raw string) catalog.ReviewPolicy {
	switch policy := catalog.ReviewPolicy(strings.ToLower(strings.TrimSpace(raw))); policy {
	case catalog.ReviewPolicyAll, catalog.ReviewPolicyCurrent, catalog.ReviewPolicyPilot:
		return policy
	default:
		slog.Warn("Invalid catalog policy; using reviewed pilot programs", "value", raw)
		return catalog.ReviewPolicyPilot
	}
}
