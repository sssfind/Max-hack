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
	"Max-hack/internal/skillgap"
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

type programAnalyzer interface {
	Analyze(context.Context, skillgap.Request) (skillgap.Result, error)
}

type AnalysisConfig struct {
	Workers       int
	Queue         int
	Timeout       time.Duration
	SkillGap      programAnalyzer
	CatalogPolicy catalog.ReviewPolicy
}

type analysisJob struct {
	chatID    int64
	userID    int64
	selection selection
	program   *catalog.Program
	queries   []string
	startedAt time.Time
	deadline  time.Time
	lifecycle context.Context
	cancel    context.CancelFunc
	startGate <-chan bool
}

type analysisRunner struct {
	api      messenger
	catalog  *catalog.Catalog
	sessions *sessionStore
	analyzer marketAnalyzer
	reports  reportGenerator
	skillGap programAnalyzer
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
		api: api, catalog: cat, sessions: sessions, analyzer: analyzer, reports: reports, skillGap: cfg.SkillGap,
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
	h.analysis.wg.Add(1)
	go h.analysis.sessionJanitor(ctx)
	slog.Info("Analysis worker pool started", "workers", h.analysis.workers, "queue", cap(h.analysis.jobs))
}

func (r *analysisRunner) sessionJanitor(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sessions.cleanupExpired()
		}
	}
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
	requestedProgram, ok := h.catalog.Get(expectedProgramCode)
	if !ok || !catalog.ProgramAllowed(requestedProgram, h.catalogPolicy) {
		h.answerAnalysisCallback(ctx, update, h.programUnavailableText(), "недоступно")
		return
	}

	chatID := update.ResolvedChatID()
	lifecycle, cancel := context.WithCancel(context.Background())
	selected, err := h.sessions.startAnalysis(chatID, expectedGeneration, expectedProgramCode, update.Callback.CallbackID, cancel)
	if err != nil {
		cancel()
		switch {
		case errors.Is(err, errAnalysisRunning):
			h.answerAnalysisCallback(ctx, update, "Отчёт уже готовится. Я пришлю статистику и PDF отдельными сообщениями.", "уже запущено")
		case errors.Is(err, errAnalysisDuplicate):
			sess := h.sessions.get(chatID)
			if sess.AnalysisDeliveryFailed {
				h.answerAnalysisCallback(ctx, update, "Предыдущий результат не удалось доставить. Нажми новую кнопку «Запустить анализ» из последнего сообщения.", "нужен повторный запуск")
			} else if sess.AnalysisInterrupted {
				h.answerAnalysisCallback(ctx, update, "Этот запуск был прерван перезапуском бота. Нажми новую кнопку «Запустить анализ» из последнего сообщения.", "нужен повторный запуск")
			} else {
				h.answerAnalysisCallback(ctx, update, "Этот запрос анализа уже принят. Повторно его не запускаю.", "уже обработано")
			}
		case errors.Is(err, errStaleSelection):
			h.answerAnalysisCallback(ctx, update, "Эта кнопка анализа относится к старому выбору. Используй кнопку из последнего сообщения.", "устарело")
		default:
			h.answerAnalysisCallback(ctx, update, "Сначала заверши выбор региона, программы и квалификации.", "не готово")
		}
		return
	}

	program, ok := h.catalog.Get(selected.ProgramCode)
	if !ok || !catalog.ProgramAllowed(program, h.catalogPolicy) {
		cancel()
		h.sessions.finishAnalysis(chatID, selected.Generation, selected.AnalysisID)
		h.answerAnalysisCallback(ctx, update, h.programUnavailableText(), "недоступно")
		return
	}
	queries := vacancyQueries(program, selected.Qualification)
	if len(queries) == 0 {
		cancel()
		h.sessions.finishAnalysis(chatID, selected.Generation, selected.AnalysisID)
		h.answerAnalysisCallback(ctx, update, "Для этой программы пока нет проверяемых запросов вакансий.", "нет запросов")
		return
	}

	startGate := make(chan bool, 1)
	job := analysisJob{
		chatID: chatID, userID: update.ResolvedUserID(), selection: selected,
		program: program, queries: queries, startedAt: time.Now(), deadline: time.Now().Add(h.analysis.timeout),
		lifecycle: lifecycle, cancel: cancel, startGate: startGate,
	}
	select {
	case <-lifecycle.Done():
		h.answerAnalysisCallback(ctx, update, "Выбор уже изменился. Используй кнопку из последнего сообщения.", "устарело")
	case h.analysis.jobs <- job:
		trackFunnel("analysis_queued")
		if !h.sessions.isSameAnalysisAttempt(chatID, selected.Generation, selected.AnalysisID) {
			startGate <- false
			cancel()
			h.answerAnalysisCallback(ctx, update, "Выбор уже изменился. Используй кнопку из последнего сообщения.", "устарело")
		} else {
			text := "Запустил анализ вакансий. Обычно это занимает около минуты. Статистика и PDF придут отдельными сообщениями."
			body := messageWithRestart(text, analyzingKeyboard(h.sessions.get(chatID)))
			notification := "анализ запущен"
			answerCtx := context.WithValue(ctx, deliveryPurposeKey{}, "analysis_start")
			jobReleased := false
			if err := h.answerAfterPrepared(answerCtx, update.Callback.CallbackID, chatID, maxapi.SendAnswerRequest{Message: &body, Notification: &notification}, func() {
				startGate <- true
				jobReleased = true
			}); err != nil {
				slog.Error("Failed to acknowledge queued analysis", "error", err)
				if !jobReleased {
					startGate <- false
					cancel()
					h.sessions.finishAnalysis(chatID, selected.Generation, selected.AnalysisID)
				}
			}
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
	sess := h.sessions.get(update.ResolvedChatID())
	var rows [][]maxapi.Button
	switch sess.Step {
	case stepReady:
		rows = h.readyKeyboard(sess)
	case stepAnalyzing:
		rows = analyzingKeyboard(sess)
	case stepAwaitQual:
		rows = h.qualKeyboard(sess)
	case stepAwaitProgram:
		rows = programSearchKeyboardRows()
	case stepAwaitRegion:
		rows = regionKeyboardRows()
	}
	body := messageWithRestart(text, rows)
	req := maxapi.SendAnswerRequest{Message: &body, Notification: &notification}
	if err := h.answer(ctx, update.Callback.CallbackID, update.ResolvedChatID(), req); err != nil {
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
			if job.startGate != nil {
				select {
				case approved := <-job.startGate:
					if !approved {
						job.cancel()
						continue
					}
				case <-ctx.Done():
					job.cancel()
					return
				}
			}
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
	interrupted := false
	deliveryFailed := false
	defer func() {
		// A lifecycle cancellation means that the selection was changed or the
		// process is shutting down. In the shutdown case, keep the durable
		// "analyzing" state so the next process can restore it as explicitly
		// interrupted instead of pretending that a result was delivered.
		if deliveryFailed {
			r.sessions.markAnalysisDeliveryFailed(job.chatID, job.selection.Generation, job.selection.AnalysisID)
		} else if !interrupted {
			r.sessions.finishAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID)
		}
	}()
	markUndelivered := func(delivered bool) {
		if delivered {
			return
		}
		if lifecycleCtx.Err() != nil {
			interrupted = true
			return
		}
		deliveryFailed = true
	}

	snapshot, stats, err := r.analyzer.Analyze(jobCtx, vacancies.SearchRequest{
		RegionCode:            job.selection.RegionCode,
		RegionName:            job.selection.RegionName,
		ProgramCode:           job.selection.ProgramCode,
		Qualification:         job.selection.Qualification,
		Queries:               job.queries,
		QueryModelNeedsReview: job.program.RequiresManualReview,
	})
	if err != nil {
		if lifecycleCtx.Err() != nil {
			interrupted = true
			return
		}
		trackAnalysis("error", "unknown", job.startedAt)
		slog.Error("Vacancy analysis failed", "error", err, "program_code", job.selection.ProgramCode)
		markUndelivered(r.sendFailure(lifecycleCtx, job, "Не удалось получить вакансии из настроенного источника. Попробуй повторить анализ позже."))
		return
	}
	if len(snapshot.Vacancies) == 0 {
		if !r.sessions.isCurrentAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID) {
			return
		}
		trackAnalysis("empty", string(snapshot.Mode), job.startedAt)
		text := fmt.Sprintf(
			"По текущей выдаче источника «%s» вакансий начального уровня для программы %s в регионе «%s» не найдено.\n\n"+
				"Срез: %s, режим: %s. Попробуй позже или выбери другое направление.",
			snapshot.Source, job.program.Code, job.selection.RegionName, formatSnapshotTime(snapshot.FetchedAt), localizedMode(snapshot.Mode),
		)
		if err := r.api.SendMessage(lifecycleCtx, job.chatID, job.userID, messageWithRestart(text, readySelectionKeyboard(job.selection))); err != nil {
			if lifecycleCtx.Err() != nil {
				interrupted = true
				return
			}
			markUndelivered(r.sendFailure(lifecycleCtx, job, "Не удалось отправить результат анализа. Попробуй повторить его позже."))
		}
		return
	}

	if !r.sessions.isCurrentAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID) {
		return
	}
	gapResult, gapWarning := r.analyzeProgram(jobCtx, job.program, stats.TopSkills)
	statisticsText := formatStatistics(job, snapshot, stats, gapResult, gapWarning)
	if err := r.api.SendMessage(lifecycleCtx, job.chatID, job.userID, messageWithRestart(statisticsText, feedbackKeyboard(job.selection))); err != nil {
		if lifecycleCtx.Err() != nil {
			interrupted = true
			return
		}
		trackAnalysis("delivery_error", string(snapshot.Mode), job.startedAt)
		slog.Error("Failed to send vacancy statistics", "error", err, "program_code", job.selection.ProgramCode)
		markUndelivered(r.sendFailure(lifecycleCtx, job, "Статистика готова, но отправить её не получилось. Запусти анализ ещё раз позже."))
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
		SkillGap:      gapResult,
		SkillGapNote:  gapWarning,
		GeneratedAt:   time.Now(),
	})
	if err != nil {
		if lifecycleCtx.Err() != nil {
			interrupted = true
			return
		}
		trackAnalysis("report_error", string(snapshot.Mode), job.startedAt)
		slog.Error("Failed to generate vacancy PDF", "error", err, "program_code", job.selection.ProgramCode)
		markUndelivered(r.sendFailure(lifecycleCtx, job, "Статистика готова, но PDF сформировать не удалось. Попробуй повторить анализ позже."))
		return
	}
	if !r.sessions.isCurrentAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID) {
		return
	}
	caption := "PDF с подходящими вакансиями и требованиями работодателей"
	if err := r.api.SendFile(lifecycleCtx, job.chatID, job.userID, document.Filename, document.Data, caption, feedbackKeyboard(job.selection)...); err != nil {
		if lifecycleCtx.Err() != nil {
			interrupted = true
			return
		}
		trackAnalysis("delivery_error", string(snapshot.Mode), job.startedAt)
		slog.Error("Failed to upload vacancy PDF", "error", err, "program_code", job.selection.ProgramCode)
		markUndelivered(r.sendFailure(lifecycleCtx, job, "Статистика готова, но отправить PDF не получилось. Запусти анализ ещё раз позже."))
		return
	}
	result := "success"
	if len(snapshot.Warnings) > 0 {
		result = "partial"
	}
	trackAnalysis(result, string(snapshot.Mode), job.startedAt)
	trackFunnel("report_delivered")
}

func (r *analysisRunner) analyzeProgram(ctx context.Context, program *catalog.Program, skills []vacancies.SkillFrequency) (*skillgap.Result, string) {
	if r.skillGap == nil {
		return nil, "Сопоставление с федеральной программой сейчас недоступно."
	}
	if len(skills) == 0 {
		return nil, "В вакансиях не выделено достаточно навыков для сопоставления с программой."
	}
	document := program.SourceDocument()
	if document.URL == "" {
		return nil, "Для программы не найден подходящий утверждённый федеральный документ."
	}
	primary := skillgap.DocumentRef{URL: document.URL, Type: documentType(document.Kind)}
	request := skillgap.Request{Primary: primary, Skills: marketSkills(skills)}
	if document.Kind == "ПОП" {
		if fgos := program.FGOSDocument(); fgos.URL != "" && fgos.URL != document.URL {
			fallback := skillgap.DocumentRef{URL: fgos.URL, Type: skillgap.DocumentFGOS}
			request.Fallback = &fallback
		}
	}

	// The document analyzer enforces SKILLGAP_DOCUMENT_TIMEOUT for network I/O;
	// the parent analysis context remains the upper bound for the whole job.
	result, err := r.skillGap.Analyze(ctx, request)
	if err != nil {
		slog.Warn("Educational document analysis failed", "error", err, "program_code", program.Code)
		return nil, "Федеральный документ не удалось разобрать автоматически; выводы Skill Gap не сформированы."
	}
	trackFunnel("skillgap_completed")
	return &result, ""
}

func documentType(kind string) skillgap.DocumentType {
	switch strings.ToUpper(strings.TrimSpace(kind)) {
	case "ПОП":
		return skillgap.DocumentPOP
	case "ФГОС":
		return skillgap.DocumentFGOS
	default:
		return skillgap.DocumentOther
	}
}

func marketSkills(skills []vacancies.SkillFrequency) []skillgap.MarketSkill {
	result := make([]skillgap.MarketSkill, 0, len(skills))
	for _, frequency := range skills {
		name := strings.TrimSpace(frequency.Name)
		if name == "" {
			continue
		}
		aliases, partial := skillTerms(name)
		result = append(result, skillgap.MarketSkill{Name: name, Aliases: aliases, PartialTerms: partial})
	}
	return result
}

func skillTerms(name string) ([]string, []string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "sql":
		return []string{"язык SQL", "PostgreSQL", "MySQL"}, []string{"реляционные базы данных", "базы данных", "управление данными"}
	case "javascript":
		return []string{"JavaScript", "Node.js", "React", "Vue.js"}, []string{"веб-программирование"}
	case "1с":
		return []string{"1С:Предприятие", "1C:Enterprise"}, []string{"автоматизация учёта"}
	case "microsoft excel":
		return []string{"Excel"}, []string{"электронные таблицы"}
	case "работа с клиентами":
		return []string{"клиентский сервис", "обслуживание клиентов"}, nil
	case "командная работа":
		return []string{"работа в команде", "командное взаимодействие"}, nil
	case "сапр":
		return []string{"CAD", "системы автоматизированного проектирования"}, nil
	default:
		return nil, nil
	}
}

func (r *analysisRunner) sendFailure(ctx context.Context, job analysisJob, text string) bool {
	if ctx.Err() != nil || !r.sessions.isCurrentAnalysis(job.chatID, job.selection.Generation, job.selection.AnalysisID) {
		return false
	}
	if err := r.api.SendMessage(ctx, job.chatID, job.userID, messageWithRestart(text, readySelectionKeyboard(job.selection))); err != nil {
		slog.Error("Failed to deliver analysis failure notice", "error", err, "program_code", job.selection.ProgramCode)
		return false
	}
	return true
}

func readySelectionKeyboard(sel selection) [][]maxapi.Button {
	return [][]maxapi.Button{
		{maxapi.CallbackButton("📊 Повторить анализ", fmt.Sprintf("%s%d:%s", payloadAnalyze, sel.Generation, sel.ProgramCode))},
		{maxapi.CallbackButton("📚 Другая программа", payloadProgram), maxapi.CallbackButton("📍 Сменить регион", payloadRegion)},
		{restartButton()},
	}
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

func formatStatistics(job analysisJob, snapshot vacancies.Snapshot, stats vacancies.Statistics, gap *skillgap.Result, gapNote string) string {
	var b strings.Builder
	var gapFooter string
	b.WriteString("📊 Рынок для программы ")
	b.WriteString(job.program.Code)
	b.WriteString(" в регионе «")
	b.WriteString(job.selection.RegionName)
	b.WriteString("»\n\n")
	fmt.Fprintf(&b, "• вакансий в выборке: %d\n", stats.VacancyCount)
	fmt.Fprintf(&b, "• работодателей: %d\n", stats.EmployerCount)
	fmt.Fprintf(&b, "• явно без опыта: %d%% (%d)\n", stats.NoExperiencePercent, stats.NoExperienceCount)
	fmt.Fprintf(&b, "• опыт до года, включая без опыта: %d%% (%d)\n", stats.EntryLevelPercent, stats.EntryLevelCount)
	fmt.Fprintf(&b, "• опыт не указан: %d%% (%d)\n", stats.ExperienceUnknownPercent, stats.ExperienceUnknownCount)
	fmt.Fprintf(&b, "• удалённый формат: %d%%\n", stats.RemotePercent)
	fmt.Fprintf(&b, "• зарплата указана в %d%% вакансий (%d из %d)\n", stats.SalaryCoveragePercent, stats.SalaryCount, stats.VacancyCount)
	if stats.SalaryMedian == nil {
		if stats.SalaryCount > 0 {
			fmt.Fprintf(&b, "• ориентир стартовой зарплаты: недостаточно сопоставимых данных (нужно минимум %d вакансий одной валюты)\n", vacancies.MinimumSalarySample)
		} else {
			b.WriteString("• ориентир стартовой зарплаты: Нет данных\n")
		}
	} else {
		fmt.Fprintf(&b, "• медианный ориентир: %s\n", formatMoney(*stats.SalaryMedian, stats.Currency))
		fmt.Fprintf(&b, "• опубликованный диапазон: %s\n", formatSalaryRange(stats.SalaryMin, stats.SalaryMax, stats.Currency))
		sampleCount := stats.SalarySampleCount
		if sampleCount == 0 {
			sampleCount = stats.SalaryCount
		}
		if sampleCount != stats.SalaryCount {
			fmt.Fprintf(&b, "• медиана рассчитана по %d вакансиям в валюте %s\n", sampleCount, stats.Currency)
		}
	}
	if len(stats.TopSkills) > 0 {
		b.WriteString("\nЧастые навыки:\n")
		for _, skill := range stats.TopSkills {
			fmt.Fprintf(&b, "• %s — %d вакансий (%d%%)\n", skill.Name, skill.Count, skill.Percent)
		}
	}
	if gap != nil && len(gap.Matches) > 0 {
		b.WriteString("\nSkill Gap по федеральному документу:\n")
		limit := min(len(gap.Matches), 6)
		for _, match := range gap.Matches[:limit] {
			b.WriteString("• ")
			b.WriteString(match.Skill)
			b.WriteString(" — ")
			b.WriteString(localizedGapStatus(match.Status))
			b.WriteByte('\n')
			if len(match.Evidence) > 0 && match.Status != skillgap.StatusNotFound {
				evidence := match.Evidence[0]
				location := evidenceLocation(evidence)
				if location != "" {
					b.WriteString("  ")
					b.WriteString(location)
					b.WriteString(": ")
				}
				b.WriteString("«")
				b.WriteString(truncate(strings.TrimSpace(evidence.Excerpt), 180))
				b.WriteString("»\n")
			}
		}
		gapFooter = fmt.Sprintf(
			"\nSkill Gap — документ: %s, срез %s%s\nИсточник документа: %s\n"+
				"Статус «Не найдено» относится только к анализируемому документу и не означает, что конкретный колледж не обучает навыку.\n",
			localizedDocumentType(gap.Source.Type), formatSnapshotTime(gap.Source.FetchedAt), fallbackLabel(gap.UsedFallback), gap.Source.URL,
		)
	} else if note := strings.TrimSpace(gapNote); note != "" {
		gapFooter = "\n⚠️ Skill Gap: " + truncate(note, 600) + "\n"
	}
	if stats.SalaryUnknownCurrencyCount > 0 {
		fmt.Fprintf(&b, "• зарплат без указанной валюты исключено из расчёта: %d\n", stats.SalaryUnknownCurrencyCount)
	}
	if method := strings.TrimSpace(stats.SalaryMethod); method != "" {
		b.WriteString("• методика: ")
		b.WriteString(method)
		b.WriteByte('\n')
	}
	if len(snapshot.Vacancies) > 0 {
		b.WriteString("\nНесколько подходящих вакансий:\n")
		limit := 3
		if len(snapshot.Vacancies) < limit {
			limit = len(snapshot.Vacancies)
		}
		for i := 0; i < limit; i++ {
			vacancy := snapshot.Vacancies[i]
			fmt.Fprintf(&b, "%d. %s", i+1, strings.TrimSpace(vacancy.Title))
			if employer := strings.TrimSpace(vacancy.Employer); employer != "" {
				b.WriteString(" — ")
				b.WriteString(employer)
			}
			b.WriteByte('\n')
			b.WriteString("   Зарплата: ")
			b.WriteString(formatSalaryRange(vacancy.SalaryFrom, vacancy.SalaryTo, vacancy.Currency))
			if vacancy.URL != "" {
				b.WriteString("\n   ")
				b.WriteString(vacancy.URL)
			}
			b.WriteByte('\n')
		}
	}
	if stats.SmallSample {
		b.WriteString("\n⚠️ Выборка небольшая: ориентир может заметно меняться.\n")
	}
	if sampling := snapshot.Sampling; sampling.Retrieved > 0 || sampling.SourceTotal > 0 {
		fmt.Fprintf(&b,
			"\nЭтапы выборки: источник %d → получено %d → стартовые %d → подходят по образованию %d → релевантные %d → уникальные %d → в отчёте %d.\n",
			sampling.SourceTotal, sampling.Retrieved, sampling.EntryLevelPassed, sampling.EducationPassed,
			sampling.RelevantPassed, sampling.Deduplicated, sampling.Included,
		)
		if sampling.RejectedHigherEducation > 0 {
			fmt.Fprintf(&b, "Вакансий с обязательным высшим образованием исключено: %d.\n", sampling.RejectedHigherEducation)
		}
	}
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
	source := strings.TrimSpace(snapshot.Source)
	if source == "" {
		source = "Работа России"
	}
	footer := gapFooter + fmt.Sprintf(
		"\nИсточник: %s\nСрез: %s\nРежим данных: %s\n\nЭто ориентир по текущим стартовым вакансиям, а не прогноз или гарантия зарплаты после выпуска.",
		source, formatSnapshotTime(snapshot.FetchedAt), localizedMode(snapshot.Mode),
	)
	return withGuaranteedFooter(b.String(), footer, 3900)
}

func localizedGapStatus(status skillgap.MatchStatus) string {
	switch status {
	case skillgap.StatusFound:
		return "Найдено"
	case skillgap.StatusPartial:
		return "Частично найдено"
	default:
		return "Не найдено в анализируемом документе"
	}
}

func localizedDocumentType(documentType skillgap.DocumentType) string {
	switch documentType {
	case skillgap.DocumentPOP:
		return "федеральная примерная образовательная программа (ПОП)"
	case skillgap.DocumentFGOS:
		return "ФГОС"
	default:
		return "федеральный документ"
	}
}

func fallbackLabel(used bool) string {
	if used {
		return ", использован резервный ФГОС"
	}
	return ""
}

func evidenceLocation(evidence skillgap.Evidence) string {
	var parts []string
	if evidence.Section != "" {
		parts = append(parts, evidence.Section)
	}
	if evidence.File != "" {
		parts = append(parts, evidence.File)
	}
	if evidence.Page > 0 {
		parts = append(parts, fmt.Sprintf("стр. %d", evidence.Page))
	}
	return strings.Join(parts, ", ")
}

func formatSnapshotTime(value time.Time) string {
	if value.IsZero() {
		return "не указано"
	}
	moscow := time.FixedZone("МСК", 3*60*60)
	return value.In(moscow).Format("02.01.2006 15:04 МСК")
}

func localizedMode(mode vacancies.Mode) string {
	switch mode {
	case vacancies.ModeLive:
		return "актуальные данные источника"
	case vacancies.ModeCache:
		return "последний сохранённый срез"
	case vacancies.ModeTest:
		return "тестовые данные"
	default:
		return string(mode)
	}
}

func formatSalaryRange(from, to *int64, currency string) string {
	if strings.TrimSpace(currency) == "" && (from != nil || to != nil) {
		var value string
		switch {
		case from != nil && to != nil:
			value = fmt.Sprintf("%s - %s", formatNumber(*from), formatNumber(*to))
		case from != nil:
			value = "от " + formatNumber(*from)
		default:
			value = "до " + formatNumber(*to)
		}
		return value + " (валюта не указана)"
	}
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
	digits := formatNumber(value)
	label := strings.TrimSpace(currency)
	if label == "" {
		return digits + " (валюта не указана)"
	}
	if strings.EqualFold(label, "RUB") {
		label = "₽"
	}
	return digits + " " + label
}

func formatNumber(value int64) string {
	digits := fmt.Sprintf("%d", value)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + " " + digits[i:]
	}
	return digits
}
