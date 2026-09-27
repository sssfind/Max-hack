package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Max-hack/internal/catalog"
	"Max-hack/internal/maxapi"
	"Max-hack/internal/pdfreport"
	"Max-hack/internal/vacancies"
)

type messenger interface {
	SendMessage(context.Context, int64, int64, maxapi.NewMessageBody) error
	AnswerCallback(context.Context, string, int64, maxapi.SendAnswerRequest) error
	SendFile(context.Context, int64, int64, string, []byte, string, ...[]maxapi.Button) error
}

type marketAnalyzer interface {
	Analyze(context.Context, vacancies.SearchRequest) (vacancies.Snapshot, vacancies.Statistics, error)
}

type reportGenerator interface {
	Generate(context.Context, pdfreport.Input) (pdfreport.Document, error)
}

type AnalysisConfig struct {
	Workers int
	Queue   int
	Timeout time.Duration
}

type analysisJob struct {
	chatID    int64
	userID    int64
	selection selection
	program   *catalog.Program
	queries   []string
	deadline  time.Time
	lifecycle context.Context
	cancel    context.CancelFunc
}

type analysisRunner struct {
	api      messenger
	catalog  *catalog.Catalog
	sessions *sessionStore
	analyzer marketAnalyzer
	reports  reportGenerator
	jobs     chan analysisJob
	workers  int
	timeout  time.Duration
	started  atomic.Bool
	wg       sync.WaitGroup
}

func newAnalysisRunner(
	api messenger,
	cat *catalog.Catalog,
	sessions *sessionStore,
	analyzer marketAnalyzer,
	reports reportGenerator,
	cfg AnalysisConfig,
) *analysisRunner {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 100
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 75 * time.Second
	}
	return &analysisRunner{
		api: api, catalog: cat, sessions: sessions, analyzer: analyzer, reports: reports,
		jobs: make(chan analysisJob, cfg.Queue), workers: cfg.Workers, timeout: cfg.Timeout,
	}
}

// StartAnalysisWorkers starts a bounded pool for slow external I/O and PDF work.
// Webhook workers only enqueue jobs and can acknowledge MAX immediately.
func (h *Handler) StartAnalysisWorkers(ctx context.Context) {
	if h.analysis == nil || !h.analysis.started.CompareAndSwap(false, true) {
		return
	}
	for workerID := 1; workerID <= h.analysis.workers; workerID++ {
		h.analysis.wg.Add(1)
		go h.analysis.worker(ctx, workerID)
	}
	slog.Info("Analysis worker pool started", "workers", h.analysis.workers, "queue", cap(h.analysis.jobs))
}

func (h *Handler) WaitAnalysisWorkers() {
	if h.analysis != nil {
		h.analysis.wg.Wait()
	}
}

func (h *Handler) onAnalyzeCallback(ctx context.Context, update maxapi.Update, expectedGeneration uint64, expectedProgramCode string) {
	if h.analysis == nil || h.analysis.analyzer == nil || h.analysis.reports == nil || !h.analysis.started.Load() {
		h.answerAnalysisCallback(ctx, update, "Анализ рынка сейчас недоступен. Попробуй позже.", "недоступно")
		return
	}

	chatID := update.ResolvedChatID()
	lifecycle, cancel := context.WithCancel(context.Background())
	selected, err := h.sessions.startAnalysis(chatID, expectedGeneration, expectedProgramCode, cancel)
	if err != nil {
		cancel()
		switch {
		case errors.Is(err, errAnalysisRunning):
			h.answerAnalysisCallback(ctx, update, "Отчёт уже готовится. Я пришлю статистику и PDF отдельными сообщениями.", "уже запущено")
		case errors.Is(err, errStaleSelection):
			h.answerAnalysisCallback(ctx, update, "Эта кнопка анализа относится к старому выбору. Используй кнопку из последнего сообщения.", "устарело")
		default:
			h.answerAnalysisCallback(ctx, update, "Сначала заверши выбор региона, программы и квалификации.", "не готово")
		}
		return
	}

	program, ok := h.catalog.Get(selected.ProgramCode)
	if !ok {
		cancel()
		h.sessions.finishAnalysis(chatID, selected.Generation, selected.AnalysisID)
		h.answerAnalysisCallback(ctx, update, "Программа не найдена. Начни выбор заново.", "ошибка")
		return
	}
	queries := vacancyQueries(program, selected.Qualification)
	if len(queries) == 0 {
		cancel()
		h.sessions.finishAnalysis(chatID, selected.Generation, selected.AnalysisID)
		h.answerAnalysisCallback(ctx, update, "Для этой программы пока нет проверяемых запросов вакансий.", "нет запросов")
		return
	}

	job := analysisJob{
		chatID: chatID, userID: update.ResolvedUserID(), selection: selected,
		program: program, queries: queries, deadline: time.Now().Add(h.analysis.timeout),
		lifecycle: lifecycle, cancel: cancel,
	}
	select {
	case <-lifecycle.Done():
		h.answerAnalysisCallback(ctx, update, "Выбор уже изменился. Используй кнопку из последнего сообщения.", "устарело")
	case h.analysis.jobs <- job:
		if !h.sessions.isSameAnalysisAttempt(chatID, selected.Generation, selected.AnalysisID) {
			h.answerAnalysisCallback(ctx, update, "Выбор уже изменился. Используй кнопку из последнего сообщения.", "устарело")
		} else {
			h.answerAnalysisCallback(ctx, update,
				"Запустил анализ «Работы России». Обычно это занимает около минуты. Статистика и PDF придут отдельными сообщениями.",
				"анализ запущен",
			)
		}
	default:
		cancel()
		h.sessions.finishAnalysis(chatID, selected.Generation, selected.AnalysisID)
		h.answerAnalysisCallback(ctx, update, "Сейчас слишком много запросов. Попробуй запустить анализ чуть позже.", "очередь занята")
	}
}

func (h *Handler) answerAnalysisCallback(ctx context.Context, update maxapi.Update, text, notification string) {
	if update.Callback == nil {
		return
	}
	body := messageWithRestart(text, nil)
	req := maxapi.SendAnswerRequest{Message: &body, Notification: &notification}
	if err := h.api.AnswerCallback(ctx, update.Callback.CallbackID, update.ResolvedChatID(), req); err != nil {
		slog.Error("Failed to acknowledge analysis callback", "error", err)
	}
}

func (r *analysisRunner) worker(ctx context.Context, workerID int) {
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			slog.Info("Analysis worker stopped", "worker_id", workerID)
			return
		case job := <-r.jobs:
			deadline := job.deadline
			if deadline.IsZero() {
				deadline = time.Now().Add(r.timeout)
			}
			stopOnShutdown := context.AfterFunc(ctx, job.cancel)
			jobCtx, cancelDeadline := context.WithDeadline(job.lifecycle, deadline)
			r.run(job.lifecycle, jobCtx, job)
			cancelDeadline()
			job.cancel()
			stopOnShutdown()
		}
	}
}

func (r *analysisRunner) run(lifecycleCtx, jobCtx context.Context, job analysisJob) {
	defer r.sessions.finishAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID)

	snapshot, stats, err := r.analyzer.Analyze(jobCtx, vacancies.SearchRequest{
		RegionCode:            job.selection.RegionCode,
		RegionName:            job.selection.RegionName,
		ProgramCode:           job.selection.ProgramCode,
		Qualification:         job.selection.Qualification,
		Queries:               job.queries,
		QueryModelNeedsReview: job.program.RequiresManualReview,
	})
	if err != nil {
		slog.Error("Vacancy analysis failed", "error", err, "program_code", job.selection.ProgramCode)
		r.sendFailure(lifecycleCtx, job, "Не удалось получить вакансии с «Работы России». Попробуй повторить анализ позже.")
		return
	}
	if len(snapshot.Vacancies) == 0 {
		if !r.sessions.isCurrentAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID) {
			return
		}
		text := fmt.Sprintf(
			"По текущей выдаче «Работы России» вакансий начального уровня для программы %s в регионе «%s» не найдено.\n\n"+
				"Срез: %s, режим: %s. Попробуй позже или выбери другое направление.",
			job.program.Code, job.selection.RegionName, snapshot.FetchedAt.Format("02.01.2006 15:04"), snapshot.Mode,
		)
		if err := r.api.SendMessage(lifecycleCtx, job.chatID, job.userID, messageWithRestart(text, nil)); err != nil {
			r.sendFailure(lifecycleCtx, job, "Не удалось отправить результат анализа. Попробуй повторить его позже.")
		}
		return
	}

	if !r.sessions.isCurrentAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID) {
		return
	}
	statisticsText := formatStatistics(job, snapshot, stats)
	if err := r.api.SendMessage(lifecycleCtx, job.chatID, job.userID, messageWithRestart(statisticsText, nil)); err != nil {
		slog.Error("Failed to send vacancy statistics", "error", err, "program_code", job.selection.ProgramCode)
		r.sendFailure(lifecycleCtx, job, "Статистика готова, но отправить её не получилось. Запусти анализ ещё раз позже.")
		return
	}

	document, err := r.reports.Generate(jobCtx, pdfreport.Input{
		ProgramCode:   job.program.Code,
		ProgramName:   job.program.ProgramName,
		Qualification: job.selection.Qualification,
		RegionName:    job.selection.RegionName,
		Roles:         roleTitles(job.program, job.selection.Qualification),
		Snapshot:      snapshot,
		Statistics:    stats,
		GeneratedAt:   time.Now(),
	})
	if err != nil {
		slog.Error("Failed to generate vacancy PDF", "error", err, "program_code", job.selection.ProgramCode)
		r.sendFailure(lifecycleCtx, job, "Статистика готова, но PDF сформировать не удалось. Попробуй повторить анализ позже.")
		return
	}
	if !r.sessions.isCurrentAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID) {
		return
	}
	caption := "PDF с подходящими вакансиями и требованиями работодателей"
	if err := r.api.SendFile(lifecycleCtx, job.chatID, job.userID, document.Filename, document.Data, caption, restartKeyboardRows()...); err != nil {
		slog.Error("Failed to upload vacancy PDF", "error", err, "program_code", job.selection.ProgramCode)
		r.sendFailure(lifecycleCtx, job, "Статистика готова, но отправить PDF не получилось. Запусти анализ ещё раз позже.")
	}
}

func (r *analysisRunner) sendFailure(ctx context.Context, job analysisJob, text string) {
	if ctx.Err() != nil || !r.sessions.isCurrentAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID) {
		return
	}
	_ = r.api.SendMessage(ctx, job.chatID, job.userID, messageWithRestart(text, nil))
}

func vacancyQueries(program *catalog.Program, qualification string) []string {
	var queries []string
	for _, role := range program.RolesForQualification(qualification) {
		queries = append(queries, role.VacancyQueries...)
	}
	if len(queries) == 0 {
		queries = append(queries, program.VacancyQueries...)
	}
	seen := make(map[string]struct{}, len(queries))
	result := make([]string, 0, len(queries))
	for _, query := range queries {
		query = strings.TrimSpace(query)
		key := strings.ToLower(query)
		if query == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, query)
	}
	return result
}

func roleTitles(program *catalog.Program, qualification string) []string {
	roles := program.RolesForQualification(qualification)
	result := make([]string, 0, len(roles))
	for _, role := range roles {
		if title := strings.TrimSpace(role.CanonicalTitle); title != "" {
			result = append(result, title)
		}
	}
	return result
}

func formatStatistics(job analysisJob, snapshot vacancies.Snapshot, stats vacancies.Statistics) string {
	var b strings.Builder
	b.WriteString("📊 Рынок для программы ")
	b.WriteString(job.program.Code)
	b.WriteString(" в регионе «")
	b.WriteString(job.selection.RegionName)
	b.WriteString("»\n\n")
	b.WriteString(fmt.Sprintf("• вакансий в выборке: %d\n", stats.VacancyCount))
	b.WriteString(fmt.Sprintf("• работодателей: %d\n", stats.EmployerCount))
	b.WriteString(fmt.Sprintf("• без опыта или с опытом до года: %d%%\n", stats.EntryLevelPercent))
	b.WriteString(fmt.Sprintf("• удалённый формат: %d%%\n", stats.RemotePercent))
	b.WriteString(fmt.Sprintf("• зарплата указана в %d%% вакансий (%d из %d)\n", stats.SalaryCoveragePercent, stats.SalaryCount, stats.VacancyCount))
	if stats.SalaryMedian == nil {
		b.WriteString("• ориентир стартовой зарплаты: Нет данных\n")
	} else {
		b.WriteString(fmt.Sprintf("• медианный ориентир: %s\n", formatMoney(*stats.SalaryMedian, stats.Currency)))
		b.WriteString(fmt.Sprintf("• опубликованный диапазон: %s\n", formatSalaryRange(stats.SalaryMin, stats.SalaryMax, stats.Currency)))
		sampleCount := stats.SalarySampleCount
		if sampleCount == 0 {
			sampleCount = stats.SalaryCount
		}
		if sampleCount != stats.SalaryCount {
			b.WriteString(fmt.Sprintf("• медиана рассчитана по %d вакансиям в валюте %s\n", sampleCount, stats.Currency))
		}
	}
	if len(stats.TopSkills) > 0 {
		b.WriteString("\nЧастые навыки:\n")
		for _, skill := range stats.TopSkills {
			b.WriteString(fmt.Sprintf("• %s — %d вакансий (%d%%)\n", skill.Name, skill.Count, skill.Percent))
		}
	}
	if stats.SmallSample {
		b.WriteString("\n⚠️ Выборка небольшая: ориентир может заметно меняться.\n")
	}
	b.WriteString(fmt.Sprintf("\nИсточник: %s\nСрез: %s\nРежим данных: %s\n", snapshot.Source, snapshot.FetchedAt.Format("02.01.2006 15:04"), snapshot.Mode))
	for i, warning := range snapshot.Warnings {
		if i == 2 {
			break
		}
		if warning = strings.TrimSpace(warning); warning != "" {
			b.WriteString("⚠️ ")
			b.WriteString(warning)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nЭто ориентир по текущим стартовым вакансиям, а не прогноз или гарантия зарплаты после выпуска.")
	return truncate(b.String(), 3900)
}

func formatSalaryRange(from, to *int64, currency string) string {
	switch {
	case from != nil && to != nil:
		return fmt.Sprintf("%s - %s", formatMoney(*from, currency), formatMoney(*to, currency))
	case from != nil:
		return "от " + formatMoney(*from, currency)
	case to != nil:
		return "до " + formatMoney(*to, currency)
	default:
		return "Нет данных"
	}
}

func formatMoney(value int64, currency string) string {
	digits := fmt.Sprintf("%d", value)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + " " + digits[i:]
	}
	label := currency
	if label == "" || strings.EqualFold(label, "RUB") {
		label = "₽"
	}
	return digits + " " + label
}
