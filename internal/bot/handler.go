package bot

import (
	"Max-hack/internal/catalog"
	"Max-hack/internal/maxapi"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type processingErrors struct {
	errs []error
}

type processingErrorsKey struct{}

type deliveryContext struct {
	eventKey string
	chatID   int64
}

type deliveryContextKey struct{}

type deliveryPurposeKey struct{}

type deliveryError struct {
	err       error
	retryable bool
}

func (e deliveryError) Error() string { return e.err.Error() }
func (e deliveryError) Unwrap() error { return e.err }

// NonRetryable prevents the durable webhook worker from replaying an entire
// stateful dialog transition after an ambiguous outbound POST failure. MAX
// does not expose an idempotency key for messages, so replay could duplicate a
// delivered response or reinterpret the same input in the next dialog step.
func (e deliveryError) NonRetryable() bool { return !e.retryable }

func recordProcessingError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if recorder, ok := ctx.Value(processingErrorsKey{}).(*processingErrors); ok {
		recorder.errs = append(recorder.errs, err)
	}
}

const (
	payloadBegin    = "sg:begin"
	payloadHelp     = "sg:help"
	payloadRestart  = "sg:restart"
	payloadAnalyze  = "sg:analyze:"
	payloadCancel   = "sg:cancel:"
	payloadMoreProg = "sg:more:"
	payloadProgram  = "sg:change:program"
	payloadRegion   = "sg:change:region"
	payloadFeedback = "sg:feedback:"
	payloadRegPref  = "sg:reg:"
	payloadProgPref = "sg:p:"
	payloadQualPref = "sg:q:"

	maxButtonLabel = 58
	maxSearchHits  = 8
	maxSearchTotal = 64
)

func callbackKind(payload string) string {
	switch {
	case payload == payloadBegin:
		return "begin"
	case payload == payloadHelp:
		return "help"
	case payload == payloadRestart:
		return "restart"
	case payload == payloadProgram:
		return "change_program"
	case payload == payloadRegion:
		return "change_region"
	case strings.HasPrefix(payload, payloadAnalyze):
		return "analyze"
	case strings.HasPrefix(payload, payloadCancel):
		return "cancel"
	case strings.HasPrefix(payload, payloadFeedback):
		return "feedback"
	case strings.HasPrefix(payload, payloadMoreProg):
		return "more_programs"
	case strings.HasPrefix(payload, payloadRegPref):
		return "select_region"
	case strings.HasPrefix(payload, payloadProgPref):
		return "select_program"
	case strings.HasPrefix(payload, payloadQualPref):
		return "select_qualification"
	default:
		return "unknown"
	}
}

// Handler — бизнес-логика ответов на Update (сценарий SkillGap).
type Handler struct {
	api           messenger
	catalog       *catalog.Catalog
	catalogPolicy catalog.ReviewPolicy
	sessions      *sessionStore
	analysis      *analysisRunner
	chatLocks     [256]sync.Mutex
}

func (h *Handler) Ready(ctx context.Context) error {
	if h == nil || h.sessions == nil || h.sessions.repo == nil {
		return nil
	}
	return h.sessions.repo.Ready(ctx)
}

func NewHandler(api messenger, cat *catalog.Catalog, analyzer marketAnalyzer, reports reportGenerator, cfg AnalysisConfig) *Handler {
	return newHandler(api, cat, analyzer, reports, cfg, newSessionStore())
}

func NewHandlerWithSessionRepository(
	api messenger,
	cat *catalog.Catalog,
	analyzer marketAnalyzer,
	reports reportGenerator,
	cfg AnalysisConfig,
	repository SessionRepository,
	sessionTTL time.Duration,
) *Handler {
	return newHandler(api, cat, analyzer, reports, cfg, newSessionStoreWithRepository(repository, sessionTTL))
}

func newHandler(api messenger, cat *catalog.Catalog, analyzer marketAnalyzer, reports reportGenerator, cfg AnalysisConfig, sessions *sessionStore) *Handler {
	policy := cfg.CatalogPolicy
	if policy == "" {
		policy = catalog.ReviewPolicyPilot
	}
	h := &Handler{
		api: api, catalog: cat, catalogPolicy: policy, sessions: sessions,
	}
	h.analysis = newAnalysisRunner(api, cat, sessions, analyzer, reports, cfg)
	return h
}

func (h *Handler) Handle(ctx context.Context, update maxapi.Update) error {
	chatID := update.ResolvedChatID()
	chatLock := &h.chatLocks[uint64(chatID)%uint64(len(h.chatLocks))]
	chatLock.Lock()
	defer chatLock.Unlock()

	recorder := &processingErrors{}
	ctx = context.WithValue(ctx, processingErrorsKey{}, recorder)
	if !isForgetUpdate(update) {
		if err := h.sessions.ensureLoaded(chatID); err != nil {
			return err
		}
		eventKey, err := maxapi.EventKey(update)
		if err != nil {
			return err
		}
		ctx = context.WithValue(ctx, deliveryContextKey{}, deliveryContext{eventKey: eventKey, chatID: chatID})
		receipt, hasReceipt, err := h.sessions.delivery(chatID, eventKey)
		if err != nil {
			return err
		}
		if hasReceipt {
			return h.replayDelivery(ctx, receipt)
		}
		if _, err := h.sessions.rollbackIncompleteTransition(chatID, eventKey); err != nil {
			return err
		}
		if err := h.sessions.beginEvent(chatID, eventKey); err != nil {
			return err
		}
		defer h.sessions.endEvent(chatID, eventKey)
	}
	switch update.UpdateType {
	case maxapi.UpdateBotStarted:
		h.onStarted(ctx, update)
	case maxapi.UpdateMessageCreated:
		h.onMessage(ctx, update)
	case maxapi.UpdateMessageCallback:
		h.onCallback(ctx, update)
	case maxapi.UpdateBotAdded:
		slog.Info("Bot added to chat", "is_channel", update.IsChannel)
	case maxapi.UpdateBotRemoved, maxapi.UpdateBotStopped:
		slog.Info("Bot left or stopped", "type", update.UpdateType)
	default:
		slog.Debug("Ignoring update type", "type", update.UpdateType)
	}
	return errors.Join(recorder.errs...)
}

func isForgetUpdate(update maxapi.Update) bool {
	return update.UpdateType == maxapi.UpdateMessageCreated && update.Message != nil && isCommand(strings.TrimSpace(update.Message.Body.Text), "forget")
}

func (h *Handler) onStarted(ctx context.Context, update maxapi.Update) {
	trackFunnel("started")
	name := "друг"
	if update.User != nil && update.User.FirstName != "" {
		name = update.User.FirstName
	}
	text := fmt.Sprintf(
		"Привет, %s! Это SkillGap — помощник для девятиклассников, которые рассматривают СПО.\n\n"+
			"Я покажу, кем можно работать после программы и какие навыки ищут работодатели. Если федеральный документ программы доступен для разбора, отдельно покажу подтверждённый Skill Gap.\n\n"+
			"Начнём с выбора направления: регион → программа СПО → квалификация.",
		name,
	)
	if err := h.send(ctx, update.ResolvedChatID(), update.ResolvedUserID(), text, startKeyboard()); err != nil {
		slog.Error("Failed to send welcome", "error", err)
	}
}

func (h *Handler) onMessage(ctx context.Context, update maxapi.Update) {
	if update.Message == nil {
		return
	}
	if update.Message.Sender != nil && update.Message.Sender.IsBot {
		return
	}

	text := strings.TrimSpace(update.Message.Body.Text)
	chatID := update.ResolvedChatID()
	userID := update.ResolvedUserID()

	switch {
	case isCommand(text, "start"):
		h.onStarted(ctx, update)
		return
	case isCommand(text, "help"):
		if err := h.send(ctx, chatID, userID, helpText(), startKeyboard()); err != nil {
			slog.Error("Failed to send help", "error", err)
		}
		return
	case isCommand(text, "privacy"):
		if err := h.send(ctx, chatID, userID, privacyText(), startKeyboard()); err != nil {
			slog.Error("Failed to send privacy notice", "error", err)
		}
		return
	case isCommand(text, "forget"):
		if err := h.sessions.forget(chatID); err != nil {
			slog.Error("Failed to delete persisted session", "error", err)
			recordProcessingError(ctx, fmt.Errorf("delete persisted session: %w", err))
			_ = h.send(ctx, chatID, userID, "Не удалось удалить сохранённое состояние. Я повторю попытку автоматически; попробуй /forget ещё раз чуть позже.", startKeyboard())
			return
		}
		trackFunnel("data_deleted")
		if err := h.send(ctx, chatID, userID, "Текущий выбор и сохранённое состояние удалены. Технические записи дедупликации очищаются автоматически по сроку хранения.", startKeyboard()); err != nil {
			slog.Error("Failed to acknowledge session deletion", "error", err)
		}
		return
	case isCommand(text, "skillgap") || strings.EqualFold(text, "skillgap"):
		h.beginDirection(ctx, chatID, userID)
		return
	case text == "":
		_ = h.send(ctx, chatID, userID, "Пришли текст или нажми «Выбрать направление».", startKeyboard())
		return
	}

	sess := h.sessions.get(chatID)
	slog.Info("SkillGap message", "step", string(sess.Step), "text_length", utf8.RuneCountInString(text))
	switch sess.Step {
	case stepAwaitRegion:
		h.handleRegionText(ctx, chatID, userID, sess, text)
	case stepAwaitProgram:
		h.handleProgramText(ctx, chatID, userID, sess, text)
	case stepAwaitQual:
		_ = h.send(ctx, chatID, userID, "Выбери квалификацию кнопкой ниже или нажми «Начать заново».", h.qualKeyboard(sess))
	case stepReady:
		if !h.selectionAllowed(sess) {
			_ = h.send(ctx, chatID, userID, "Ранее выбранная программа недоступна при текущей политике каталога. Выбери другую программу.", h.readyKeyboard(sess))
		} else if sess.AnalysisDeliveryFailed {
			_ = h.send(ctx, chatID, userID, "Предыдущий анализ завершился, но результат не удалось доставить. Выбор сохранён — запусти анализ повторно.", h.readyKeyboard(sess))
		} else if sess.AnalysisInterrupted {
			_ = h.send(ctx, chatID, userID, "Предыдущий анализ был прерван перезапуском бота. Выбор сохранён — запусти анализ повторно.", h.readyKeyboard(sess))
		} else {
			_ = h.send(ctx, chatID, userID, "Направление уже выбрано. Можно запустить анализ или начать заново.", h.readyKeyboard(sess))
		}
	case stepAnalyzing:
		_ = h.send(ctx, chatID, userID, "Анализ уже выполняется. Я пришлю статистику и PDF отдельными сообщениями.", analyzingKeyboard(sess))
	default:
		_ = h.send(ctx, chatID, userID, "Чтобы начать, нажми «Выбрать направление».", startKeyboard())
	}
}

func (h *Handler) onCallback(ctx context.Context, update maxapi.Update) {
	if update.Callback == nil {
		return
	}
	chatID := update.ResolvedChatID()
	payload := update.Callback.Payload
	slog.Info("SkillGap callback", "kind", callbackKind(payload))

	if strings.HasPrefix(payload, payloadAnalyze) {
		generation, programCode, ok := parseAnalyzePayload(strings.TrimPrefix(payload, payloadAnalyze))
		if !ok {
			h.answerAnalysisCallback(ctx, update, "Эта кнопка анализа устарела. Используй кнопку из последнего сообщения.", "устарело")
			return
		}
		h.onAnalyzeCallback(ctx, update, generation, programCode)
		return
	}
	if strings.HasPrefix(payload, payloadCancel) {
		h.onCancelAnalysis(ctx, update, strings.TrimPrefix(payload, payloadCancel))
		return
	}
	if strings.HasPrefix(payload, payloadFeedback) {
		h.onFeedback(ctx, update, strings.TrimPrefix(payload, payloadFeedback))
		return
	}

	var (
		msgText string
		notify  string
		kb      [][]maxapi.Button
	)

	switch {
	case payload == payloadHelp:
		msgText = helpText()
		notify = "Справка"
		kb = startKeyboardRows()
	case payload == payloadBegin, payload == payloadRestart:
		msgText, kb = h.beginDirectionContent(chatID)
		notify = "Выбор направления"
	case payload == payloadRegion:
		msgText, kb = h.beginDirectionContent(chatID)
		notify = "Выбор региона"
	case payload == payloadProgram:
		msgText, kb, notify = h.reselectProgram(chatID)
	case strings.HasPrefix(payload, payloadMoreProg):
		msgText, kb, notify = h.morePrograms(chatID, strings.TrimPrefix(payload, payloadMoreProg))
	case strings.HasPrefix(payload, payloadRegPref):
		short := strings.TrimPrefix(payload, payloadRegPref)
		msgText, kb, notify = h.selectRegion(chatID, short)
	case strings.HasPrefix(payload, payloadProgPref):
		code := strings.TrimPrefix(payload, payloadProgPref)
		msgText, kb, notify = h.selectProgram(chatID, code)
	case strings.HasPrefix(payload, payloadQualPref):
		programCode, idx, ok := parseQualificationPayload(strings.TrimPrefix(payload, payloadQualPref))
		if !ok {
			msgText = "Некорректная кнопка квалификации. Начни выбор заново."
			notify = "ошибка"
			kb = startKeyboardRows()
		} else {
			msgText, kb, notify = h.selectQualification(chatID, programCode, idx)
		}
	default:
		msgText = "Неизвестная кнопка. Нажми «Выбрать направление»."
		notify = "ок"
		kb = startKeyboardRows()
	}

	message := messageWithRestart(msgText, kb)
	answer := maxapi.SendAnswerRequest{Notification: &notify, Message: &message}
	if err := h.answer(ctx, update.Callback.CallbackID, chatID, answer); err != nil {
		slog.Error("Failed to answer callback", "error", err)
	}
}

func (h *Handler) onCancelAnalysis(ctx context.Context, update maxapi.Update, value string) {
	if update.Callback == nil {
		return
	}
	generationText, analysisText, ok := strings.Cut(value, ":")
	generation, genErr := strconv.ParseUint(generationText, 10, 64)
	analysisID, analysisErr := strconv.ParseUint(analysisText, 10, 64)
	if !ok || genErr != nil || analysisErr != nil {
		h.answerAnalysisCallback(ctx, update, "Кнопка отмены устарела.", "устарело")
		return
	}
	sess, canceled := h.sessions.cancelAnalysis(update.ResolvedChatID(), generation, analysisID)
	if !canceled {
		if sess.AnalysisDeliveryFailed && sess.Generation == generation && sess.AnalysisID == analysisID {
			text := "Этот анализ завершился, но результат не удалось доставить. Выбор сохранён — запусти анализ повторно."
			body := messageWithRestart(text, h.readyKeyboard(sess))
			notify := "нужен повторный запуск"
			if err := h.answer(ctx, update.Callback.CallbackID, update.ResolvedChatID(), maxapi.SendAnswerRequest{
				Message: &body, Notification: &notify,
			}); err != nil {
				slog.Error("Failed to acknowledge analysis delivery failure", "error", err)
			}
			return
		}
		if sess.AnalysisInterrupted && sess.Generation == generation && sess.AnalysisID == analysisID {
			text := "Этот анализ был прерван перезапуском бота и сейчас не выполняется. Выбор сохранён — запусти анализ повторно."
			body := messageWithRestart(text, h.readyKeyboard(sess))
			notify := "нужен повторный запуск"
			if err := h.answer(ctx, update.Callback.CallbackID, update.ResolvedChatID(), maxapi.SendAnswerRequest{
				Message: &body, Notification: &notify,
			}); err != nil {
				slog.Error("Failed to acknowledge interrupted analysis", "error", err)
			}
			return
		}
		h.answerAnalysisCallback(ctx, update, "Этот анализ уже завершён или был отменён.", "не выполняется")
		return
	}
	trackAnalysis("canceled", "unknown", time.Time{})
	text := "Анализ отменён. Выбранное направление сохранено — можно запустить его ещё раз или изменить выбор."
	body := messageWithRestart(text, h.readyKeyboard(sess))
	notify := "анализ отменён"
	if err := h.answer(ctx, update.Callback.CallbackID, update.ResolvedChatID(), maxapi.SendAnswerRequest{
		Message: &body, Notification: &notify,
	}); err != nil {
		slog.Error("Failed to acknowledge analysis cancellation", "error", err)
	}
}

func (h *Handler) onFeedback(ctx context.Context, update maxapi.Update, value string) {
	if update.Callback == nil {
		return
	}
	analysisText, rating, ok := strings.Cut(value, ":")
	analysisID, err := strconv.ParseUint(analysisText, 10, 64)
	if !ok || err != nil || (rating != "up" && rating != "down") {
		h.answerAnalysisCallback(ctx, update, "Не удалось сохранить оценку.", "ошибка")
		return
	}
	if !h.sessions.recordFeedback(update.ResolvedChatID(), analysisID) {
		h.answerAnalysisCallback(ctx, update, "Оценка уже сохранена. Спасибо!", "спасибо")
		return
	}
	feedbackTotal.WithLabelValues(rating).Inc()
	sess := h.sessions.get(update.ResolvedChatID())
	rows := h.readyKeyboard(sess)
	if sess.Step == stepAnalyzing {
		rows = analyzingKeyboard(sess)
	}
	body := messageWithRestart("Спасибо! Обезличенная оценка поможет улучшить релевантность вакансий.", rows)
	notify := "спасибо"
	if err := h.answer(ctx, update.Callback.CallbackID, update.ResolvedChatID(), maxapi.SendAnswerRequest{
		Message: &body, Notification: &notify,
	}); err != nil {
		slog.Error("Failed to acknowledge feedback", "error", err)
	}
}

func (h *Handler) beginDirection(ctx context.Context, chatID, userID int64) {
	text, kb := h.beginDirectionContent(chatID)
	if err := h.send(ctx, chatID, userID, text, kb); err != nil {
		slog.Error("Failed to begin direction", "error", err)
	}
}

func (h *Handler) beginDirectionContent(chatID int64) (string, [][]maxapi.Button) {
	h.sessions.reset(chatID)
	trackFunnel("region_prompt")
	text := "Шаг 1/3 — регион\n\nВыбери регион кнопкой или напиши название (например: «Татарстан», «Новосибирская»)."
	return text, regionKeyboardRows()
}

func (h *Handler) handleRegionText(ctx context.Context, chatID, userID int64, sess session, text string) {
	hits := catalog.SearchRegions(text, maxSearchHits)
	if len(hits) == 0 {
		_ = h.send(ctx, chatID, userID, "Регион не найден. Попробуй ещё раз или выбери из популярных.", regionKeyboardRows())
		return
	}
	if len(hits) == 1 {
		msg, kb, _ := h.applyRegion(chatID, sess.Generation, hits[0])
		_ = h.send(ctx, chatID, userID, msg, kb)
		return
	}
	sess.SearchRegions = make([]string, 0, len(hits))
	rows := make([][]maxapi.Button, 0, len(hits)+1)
	var b strings.Builder
	b.WriteString("Нашёл несколько регионов — выбери нужный:\n")
	for _, r := range hits {
		sess.SearchRegions = append(sess.SearchRegions, r.Short)
		b.WriteString("• ")
		b.WriteString(r.Name)
		b.WriteByte('\n')
		rows = append(rows, []maxapi.Button{
			maxapi.CallbackButton(truncate(r.Name, maxButtonLabel), payloadRegPref+r.Short),
		})
	}
	if _, ok := h.sessions.updateIf(chatID, sess.Generation, stepAwaitRegion, func(current *session) {
		current.SearchRegions = append([]string(nil), sess.SearchRegions...)
	}); !ok {
		_ = h.send(ctx, chatID, userID, "Выбор уже изменился. Используй кнопки из последнего сообщения.", startKeyboardRows())
		return
	}
	rows = append(rows, restartKeyboardRows()...)
	_ = h.send(ctx, chatID, userID, b.String(), rows)
}

func (h *Handler) selectRegion(chatID int64, short string) (string, [][]maxapi.Button, string) {
	sess := h.sessions.get(chatID)
	if sess.Step != stepAwaitRegion {
		return h.staleSelectionMessage(sess)
	}
	r, ok := catalog.RegionByShort(short)
	if !ok {
		return "Неизвестный регион. Выбери из списка.", regionKeyboardRows(), "ошибка"
	}
	return h.applyRegion(chatID, sess.Generation, r)
}

func (h *Handler) applyRegion(chatID int64, generation uint64, r catalog.Region) (string, [][]maxapi.Button, string) {
	if _, ok := h.sessions.updateIf(chatID, generation, stepAwaitRegion, func(sess *session) {
		sess.RegionShort = r.Short
		sess.RegionCode = r.Code
		sess.RegionName = r.Name
		sess.ProgramCode = ""
		sess.Qualification = ""
		sess.SearchPrograms = nil
		sess.ProgramQuery = ""
		sess.ProgramOffset = 0
		sess.SearchRegions = nil
		sess.Step = stepAwaitProgram
	}); !ok {
		return h.staleSelectionMessage(h.sessions.get(chatID))
	}
	msg := fmt.Sprintf(
		"Регион: %s\n\nШаг 2/3 — программа СПО\n\nНапиши код (например 09.02.07) или название программы / специальности.",
		r.Name,
	)
	trackFunnel("region_selected")
	kb := programSearchKeyboardRows()
	return msg, kb, r.Name
}

func (h *Handler) handleProgramText(ctx context.Context, chatID, userID int64, sess session, text string) {
	hits := h.catalog.SearchWithPolicy(text, maxSearchTotal, h.catalogPolicy)
	if len(hits) == 0 {
		message := "Программа не найдена. Уточни код или название (например «09.02.11» или «программное обеспечение»)."
		if h.catalogPolicy == catalog.ReviewPolicyPilot {
			message = "Программа не входит в проверенный пилотный набор. Сейчас доступны только актуальные программы с экспертно проверенной поисковой моделью."
		}
		_ = h.send(ctx, chatID, userID,
			message,
			restartKeyboardRows(),
		)
		return
	}
	if len(hits) == 1 {
		msg, kb, _ := h.applyProgram(chatID, sess.Generation, hits[0])
		_ = h.send(ctx, chatID, userID, msg, kb)
		return
	}

	codes := make([]string, 0, len(hits))
	for _, p := range hits {
		codes = append(codes, p.Code)
	}
	if _, ok := h.sessions.updateIf(chatID, sess.Generation, stepAwaitProgram, func(current *session) {
		current.SearchPrograms = append([]string(nil), codes...)
		current.ProgramQuery = text
		current.ProgramOffset = 0
	}); !ok {
		_ = h.send(ctx, chatID, userID, "Выбор уже изменился. Используй кнопки из последнего сообщения.", startKeyboardRows())
		return
	}
	current := h.sessions.get(chatID)
	msg, rows := h.programChoices(current, 0)
	_ = h.send(ctx, chatID, userID, msg, rows)
}

func (h *Handler) programChoices(sess session, offset int) (string, [][]maxapi.Button) {
	if offset < 0 || offset >= len(sess.SearchPrograms) {
		offset = 0
	}
	end := offset + maxSearchHits
	if end > len(sess.SearchPrograms) {
		end = len(sess.SearchPrograms)
	}

	rows := make([][]maxapi.Button, 0, end-offset+3)
	var b strings.Builder
	b.WriteString("Нашёл несколько программ — выбери нужную:\n\n")
	for _, code := range sess.SearchPrograms[offset:end] {
		p, ok := h.catalog.Get(code)
		if !ok || !catalog.ProgramAllowed(p, h.catalogPolicy) {
			continue
		}
		status := "актуальна"
		if !p.IsCurrent {
			status = "историческая"
		}
		fmt.Fprintf(&b, "• %s — %s (%s, %s)\n", p.Code, p.ProgramName, p.ProgramType, status)
		label := truncate(fmt.Sprintf("%s %s", p.Code, p.ProgramName), maxButtonLabel)
		rows = append(rows, []maxapi.Button{maxapi.CallbackButton(label, payloadProgPref+p.Code)})
	}
	if end < len(sess.SearchPrograms) {
		rows = append(rows, []maxapi.Button{maxapi.CallbackButton(
			"Показать ещё",
			fmt.Sprintf("%s%d:%d", payloadMoreProg, sess.Generation, end),
		)})
	}
	rows = append(rows, programSearchKeyboardRows()...)
	return b.String(), rows
}

func (h *Handler) morePrograms(chatID int64, value string) (string, [][]maxapi.Button, string) {
	generationText, offsetText, ok := strings.Cut(value, ":")
	if !ok {
		return "Кнопка устарела. Введи запрос программы ещё раз.", programSearchKeyboardRows(), "устарело"
	}
	generation, genErr := strconv.ParseUint(generationText, 10, 64)
	offset, offsetErr := strconv.Atoi(offsetText)
	if genErr != nil || offsetErr != nil || offset < 0 {
		return "Кнопка устарела. Введи запрос программы ещё раз.", programSearchKeyboardRows(), "устарело"
	}
	sess, changed := h.sessions.updateIf(chatID, generation, stepAwaitProgram, func(current *session) {
		current.ProgramOffset = offset
	})
	if !changed || offset >= len(sess.SearchPrograms) {
		return h.staleSelectionMessage(h.sessions.get(chatID))
	}
	msg, rows := h.programChoices(sess, offset)
	return msg, rows, "ещё программы"
}

func (h *Handler) reselectProgram(chatID int64) (string, [][]maxapi.Button, string) {
	sess, ok := h.sessions.reselectProgram(chatID)
	if !ok {
		msg, rows := h.beginDirectionContent(chatID)
		return msg, rows, "выбор региона"
	}
	trackFunnel("program_prompt")
	return fmt.Sprintf(
		"Регион: %s\n\nШаг 2/3 — программа СПО\n\nНапиши новый код или название программы.",
		sess.RegionName,
	), programSearchKeyboardRows(), "другая программа"
}

func (h *Handler) selectProgram(chatID int64, code string) (string, [][]maxapi.Button, string) {
	sess := h.sessions.get(chatID)
	if sess.Step != stepAwaitProgram || !containsFold(sess.SearchPrograms, code) {
		return h.staleSelectionMessage(sess)
	}
	p, ok := h.catalog.Get(code)
	if !ok {
		return "Программа не найдена. Напиши код или название ещё раз.",
			restartKeyboardRows(),
			"ошибка"
	}
	if !catalog.ProgramAllowed(p, h.catalogPolicy) {
		return h.programUnavailableResponse()
	}
	return h.applyProgram(chatID, sess.Generation, p)
}

func (h *Handler) applyProgram(chatID int64, generation uint64, p *catalog.Program) (string, [][]maxapi.Button, string) {
	if !catalog.ProgramAllowed(p, h.catalogPolicy) {
		return h.programUnavailableResponse()
	}
	qualifications := append([]string(nil), p.Qualifications...)
	sess, ok := h.sessions.updateIf(chatID, generation, stepAwaitProgram, func(sess *session) {
		sess.ProgramCode = p.Code
		sess.Qualification = ""
		sess.SearchPrograms = nil
		switch len(qualifications) {
		case 0:
			sess.Step = stepReady
		case 1:
			sess.Qualification = qualifications[0]
			sess.Step = stepReady
		default:
			sess.Step = stepAwaitQual
		}
	})
	if !ok {
		return h.staleSelectionMessage(h.sessions.get(chatID))
	}
	trackFunnel("program_selected")
	status := "актуальна"
	if !p.IsCurrent {
		status = "историческая / приём может быть закрыт"
	}
	header := fmt.Sprintf(
		"Программа: %s — %s\nТип: %s\nСтатус: %s\nРегион: %s",
		p.Code, p.ProgramName, p.ProgramType, status, sess.RegionName,
	)

	if len(p.Qualifications) == 0 {
		msg := header + "\n\nКвалификации в справочнике не указаны.\n\n" + h.formatRoles(p, "")
		return msg, h.readyKeyboard(sess), p.Code
	}
	if len(p.Qualifications) == 1 {
		msg := header + "\nКвалификация: " + sess.Qualification + "\n\n" + h.formatRoles(p, sess.Qualification)
		return msg, h.readyKeyboard(sess), p.Code
	}

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n\nШаг 3/3 — квалификация\n\nВ программе несколько квалификаций. Выбери одну:")
	return b.String(), h.qualKeyboard(sess), p.Code
}

func (h *Handler) selectQualification(chatID int64, programCode string, idx int) (string, [][]maxapi.Button, string) {
	sess := h.sessions.get(chatID)
	if sess.Step != stepAwaitQual || !strings.EqualFold(sess.ProgramCode, programCode) {
		return h.staleSelectionMessage(sess)
	}
	p, ok := h.catalog.Get(sess.ProgramCode)
	if !ok {
		return "Сначала выбери программу СПО.", restartKeyboardRows(), "ошибка"
	}
	if !catalog.ProgramAllowed(p, h.catalogPolicy) {
		return h.programUnavailableResponse()
	}
	if idx < 0 || idx >= len(p.Qualifications) {
		return "Некорректная квалификация. Выбери кнопку из списка.", h.qualKeyboard(sess), "ошибка"
	}
	sess, ok = h.sessions.updateIf(chatID, sess.Generation, stepAwaitQual, func(sess *session) {
		sess.Qualification = p.Qualifications[idx]
		sess.Step = stepReady
	})
	if !ok {
		return h.staleSelectionMessage(h.sessions.get(chatID))
	}
	trackFunnel("qualification_selected")
	status := "актуальна"
	if !p.IsCurrent {
		status = "историческая / приём может быть закрыт"
	}
	msg := fmt.Sprintf(
		"Программа: %s — %s\nТип: %s\nСтатус: %s\nКвалификация: %s\nРегион: %s\n\n%s",
		p.Code, p.ProgramName, p.ProgramType, status, sess.Qualification, sess.RegionName,
		h.formatRoles(p, sess.Qualification),
	)
	return msg, h.readyKeyboard(sess), "квалификация"
}

func (h *Handler) formatRoles(p *catalog.Program, qualification string) string {
	roles := p.RolesForQualification(qualification)
	var b strings.Builder
	b.WriteString("Возможные стартовые должности после обучения:\n")
	if len(roles) == 0 {
		b.WriteString("• пока нет данных в справочнике\n")
	} else {
		seen := make(map[string]struct{}, len(roles))
		for _, role := range roles {
			title := strings.TrimSpace(role.CanonicalTitle)
			if title == "" {
				continue
			}
			if _, ok := seen[title]; ok {
				continue
			}
			seen[title] = struct{}{}
			b.WriteString("• ")
			b.WriteString(title)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nЭто возможные направления работы, а не гарантия трудоустройства.")
	if p.RequiresManualReview {
		b.WriteString("\n⚠️ Поисковая модель этой программы ещё проходит экспертную проверку; результат анализа будет предварительным.")
	}
	if duration := strings.TrimSpace(p.ActiveStandard.DurationAfterGrade9); duration != "" {
		b.WriteString("\n\nСрок обучения после 9 класса: ")
		b.WriteString(duration)
	}
	if duration := strings.TrimSpace(p.ActiveStandard.DurationAfterGrade11); duration != "" {
		b.WriteString("\nСрок обучения после 11 класса: ")
		b.WriteString(duration)
	}
	if gia := strings.TrimSpace(p.ActiveStandard.GIAForm); gia != "" {
		b.WriteString("\nИтоговая аттестация: ")
		b.WriteString(gia)
	}
	if end := strings.TrimSpace(p.ActiveStandard.AdmissionEnds); end != "" {
		b.WriteString("\nОкончание приёма по стандарту: ")
		b.WriteString(end)
	}
	document := p.SourceDocument()
	if document.URL != "" {
		b.WriteString("\n\nФедеральный источник: ")
		b.WriteString(document.Kind)
		if document.Status != "" {
			b.WriteString(" (")
			b.WriteString(document.Status)
			b.WriteString(")")
		}
		b.WriteString("\n")
		b.WriteString(document.URL)
	}
	return b.String()
}

func (h *Handler) qualKeyboard(sess session) [][]maxapi.Button {
	p, ok := h.catalog.Get(sess.ProgramCode)
	if !ok || !catalog.ProgramAllowed(p, h.catalogPolicy) {
		return programSearchKeyboardRows()
	}
	rows := make([][]maxapi.Button, 0, len(p.Qualifications)+3)
	for i, q := range p.Qualifications {
		rows = append(rows, []maxapi.Button{
			maxapi.CallbackButton(truncate(q, maxButtonLabel), fmt.Sprintf("%s%s:%d", payloadQualPref, p.Code, i)),
		})
	}
	rows = append(rows, programSearchKeyboardRows()...)
	return rows
}

func (h *Handler) readyKeyboard(sess session) [][]maxapi.Button {
	if !h.selectionAllowed(sess) {
		return [][]maxapi.Button{
			{maxapi.CallbackButton("📚 Выбрать другую программу", payloadProgram), maxapi.CallbackButton("📍 Сменить регион", payloadRegion)},
			{restartButton()},
		}
	}
	label := "📊 Запустить анализ"
	if program, ok := h.catalog.Get(sess.ProgramCode); ok && program.RequiresManualReview {
		label = "🧪 Предварительный анализ"
	}
	return [][]maxapi.Button{
		{maxapi.CallbackButton(label, fmt.Sprintf("%s%d:%s", payloadAnalyze, sess.Generation, sess.ProgramCode))},
		{maxapi.CallbackButton("📚 Другая программа", payloadProgram), maxapi.CallbackButton("📍 Сменить регион", payloadRegion)},
		{restartButton()},
	}
}

func (h *Handler) selectionAllowed(sess session) bool {
	program, ok := h.catalog.Get(sess.ProgramCode)
	return ok && catalog.ProgramAllowed(program, h.catalogPolicy)
}

func (h *Handler) programUnavailableResponse() (string, [][]maxapi.Button, string) {
	return h.programUnavailableText(), programSearchKeyboardRows(), "недоступно"
}

func (h *Handler) programUnavailableText() string {
	message := "Программа недоступна при текущей политике каталога. Введи другой код или название."
	if h.catalogPolicy == catalog.ReviewPolicyPilot {
		message = "Программа не входит в проверенный пилотный набор. Выбери другую актуальную программу с экспертно проверенной поисковой моделью."
	}
	return message
}

func (h *Handler) send(ctx context.Context, chatID, userID int64, text string, rows [][]maxapi.Button) error {
	body := messageWithRestart(text, rows)
	if err := h.prepareDelivery(ctx, deliveryReceipt{Kind: "message", ChatID: chatID, UserID: userID}, body); err != nil {
		recordProcessingError(ctx, err)
		return err
	}
	err := h.api.SendMessage(ctx, chatID, userID, body)
	if err != nil {
		recordProcessingError(ctx, classifyDeliveryError(err))
		return err
	}
	return h.completeDelivery(ctx)
}

func (h *Handler) answer(ctx context.Context, callbackID string, chatID int64, request maxapi.SendAnswerRequest) error {
	return h.answerAfterPrepared(ctx, callbackID, chatID, request, nil)
}

// answerAfterPrepared runs after the exact callback response is durably
// recorded but before it is sent to MAX. Analysis uses this boundary to make
// its job visible only after a restart-safe acknowledgement exists.
func (h *Handler) answerAfterPrepared(
	ctx context.Context,
	callbackID string,
	chatID int64,
	request maxapi.SendAnswerRequest,
	afterPrepared func(),
) error {
	if err := h.prepareDelivery(ctx, deliveryReceipt{Kind: "answer", ChatID: chatID, CallbackID: callbackID}, request); err != nil {
		recordProcessingError(ctx, err)
		return err
	}
	if afterPrepared != nil {
		afterPrepared()
	}
	err := h.api.AnswerCallback(ctx, callbackID, chatID, request)
	if err != nil {
		recordProcessingError(ctx, classifyDeliveryError(err))
		return err
	}
	return h.completeDelivery(ctx)
}

func classifyDeliveryError(err error) deliveryError {
	return deliveryError{err: err, retryable: maxapi.IsRetryableDeliveryError(err)}
}

func (h *Handler) prepareDelivery(ctx context.Context, receipt deliveryReceipt, request any) error {
	meta, ok := ctx.Value(deliveryContextKey{}).(deliveryContext)
	if !ok {
		return nil
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode outbound delivery receipt: %w", err)
	}
	receipt.EventKey = meta.eventKey
	if purpose, ok := ctx.Value(deliveryPurposeKey{}).(string); ok {
		receipt.Purpose = purpose
	}
	receipt.Payload = payload
	return h.sessions.recordDelivery(meta.chatID, receipt)
}

func (h *Handler) completeDelivery(ctx context.Context) error {
	meta, ok := ctx.Value(deliveryContextKey{}).(deliveryContext)
	if !ok {
		return nil
	}
	if err := h.sessions.markDeliveryCompleted(meta.chatID, meta.eventKey); err != nil {
		wrapped := deliveryError{err: err, retryable: false}
		recordProcessingError(ctx, wrapped)
		return wrapped
	}
	return nil
}

func (h *Handler) replayDelivery(ctx context.Context, receipt deliveryReceipt) error {
	if receipt.Delivered {
		return nil
	}
	if !receipt.Durable {
		if err := h.sessions.ensureDeliveryDurable(receipt.ChatID, receipt.EventKey); err != nil {
			return err
		}
	}
	var err error
	switch receipt.Kind {
	case "message":
		var body maxapi.NewMessageBody
		if decodeErr := json.Unmarshal(receipt.Payload, &body); decodeErr != nil {
			return deliveryError{err: fmt.Errorf("decode stored message receipt: %w", decodeErr), retryable: false}
		}
		err = h.api.SendMessage(ctx, receipt.ChatID, receipt.UserID, body)
	case "answer":
		var request maxapi.SendAnswerRequest
		if decodeErr := json.Unmarshal(receipt.Payload, &request); decodeErr != nil {
			return deliveryError{err: fmt.Errorf("decode stored callback receipt: %w", decodeErr), retryable: false}
		}
		err = h.api.AnswerCallback(ctx, receipt.CallbackID, receipt.ChatID, request)
	default:
		return deliveryError{err: fmt.Errorf("unknown stored delivery kind %q", receipt.Kind), retryable: false}
	}
	if err != nil {
		return classifyDeliveryError(err)
	}
	if err := h.sessions.markDeliveryCompleted(receipt.ChatID, receipt.EventKey); err != nil {
		return deliveryError{err: err, retryable: false}
	}
	return nil
}

func messageWithRestart(text string, rows [][]maxapi.Button) maxapi.NewMessageBody {
	rows = withRestartButton(rows)
	body := maxapi.NewMessageBody{Text: &text}
	if len(rows) > 0 {
		body.Attachments = []maxapi.AttachmentRequest{maxapi.InlineKeyboard(rows...)}
	}
	return body
}

func withRestartButton(rows [][]maxapi.Button) [][]maxapi.Button {
	for _, row := range rows {
		for _, button := range row {
			if button.Type == "callback" && button.Payload == payloadRestart {
				return rows
			}
		}
	}

	result := make([][]maxapi.Button, len(rows), len(rows)+1)
	copy(result, rows)
	return append(result, restartKeyboardRows()...)
}

func restartKeyboardRows() [][]maxapi.Button {
	return [][]maxapi.Button{{restartButton()}}
}

func restartButton() maxapi.Button {
	return maxapi.CallbackButton("🔄 Начать заново", payloadRestart)
}

func programSearchKeyboardRows() [][]maxapi.Button {
	return [][]maxapi.Button{
		{maxapi.CallbackButton("← Сменить регион", payloadRegion)},
		{restartButton()},
	}
}

func analyzingKeyboard(sess session) [][]maxapi.Button {
	return [][]maxapi.Button{
		{maxapi.CallbackButton("⏹ Отменить анализ", fmt.Sprintf("%s%d:%d", payloadCancel, sess.Generation, sess.AnalysisID))},
		{restartButton()},
	}
}

func feedbackKeyboard(sel selection) [][]maxapi.Button {
	return [][]maxapi.Button{
		{
			maxapi.CallbackButton("👍 Полезно", fmt.Sprintf("%s%d:up", payloadFeedback, sel.AnalysisID)),
			maxapi.CallbackButton("👎 Не помогло", fmt.Sprintf("%s%d:down", payloadFeedback, sel.AnalysisID)),
		},
		{maxapi.CallbackButton("📊 Повторить анализ", fmt.Sprintf("%s%d:%s", payloadAnalyze, sel.Generation, sel.ProgramCode))},
		{maxapi.CallbackButton("📚 Другая программа", payloadProgram), maxapi.CallbackButton("📍 Сменить регион", payloadRegion)},
		{restartButton()},
	}
}

func startKeyboard() [][]maxapi.Button {
	return startKeyboardRows()
}

func startKeyboardRows() [][]maxapi.Button {
	return [][]maxapi.Button{
		{maxapi.CallbackButton("🎯 Выбрать направление", payloadBegin)},
		{maxapi.CallbackButton("❓ Справка", payloadHelp)},
		{restartButton()},
	}
}

func regionKeyboardRows() [][]maxapi.Button {
	rows := make([][]maxapi.Button, 0, len(catalog.PopularRegions)/2+2)
	var row []maxapi.Button
	for _, short := range catalog.PopularRegions {
		r, ok := catalog.RegionByShort(short)
		if !ok {
			continue
		}
		row = append(row, maxapi.CallbackButton(r.Name, payloadRegPref+r.Short))
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, restartKeyboardRows()...)
	return rows
}

func helpText() string {
	return "SkillGap помогает понять перспективы после программы СПО.\n\n" +
		"Команды:\n• /start — приветствие\n• /help — справка\n• /privacy — какие данные используются\n• /forget — удалить сохранённый выбор\n• /skillgap — выбор направления\n\n" +
		"Сценарий:\n1) регион\n2) программа СПО (код или название)\n3) квалификация (если их несколько)\n4) список возможных должностей и запуск анализа рынка"
}

func privacyText() string {
	return "Конфиденциальность SkillGap\n\n" +
		"Для работы бот использует технический идентификатор чата, последний запрос по каталогу и выбранные регион, программу и квалификацию. Текст сообщений не используется для профилирования и не передаётся рекламным системам. " +
		"До завершения обработки исходное событие и подготовленный ответ могут временно храниться для безопасного повтора; сразу после успешной доставки их содержимое и адресные данные очищаются. Технические идентификаторы и статусы удаляются автоматически по сроку хранения. " +
		"В продуктовой статистике учитываются только обезличенные этапы сценария, длительность и результат анализа.\n\n" +
		"Не отправляй в бот ФИО, документы, адрес, телефон и другие персональные данные. Команда /forget удаляет сохранённый выбор; «Начать заново» только сбрасывает сценарий."
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

func isCommand(text, name string) bool {
	t := strings.TrimSpace(text)
	if t == "" || !strings.HasPrefix(t, "/") {
		return false
	}
	cmd := strings.SplitN(t[1:], "@", 2)[0]
	cmd = strings.SplitN(cmd, " ", 2)[0]
	return strings.EqualFold(cmd, name)
}

func parseQualificationPayload(value string) (string, int, bool) {
	programCode, indexText, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(programCode) == "" {
		return "", 0, false
	}
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 {
		return "", 0, false
	}
	return programCode, index, true
}

func parseAnalyzePayload(value string) (uint64, string, bool) {
	generationText, programCode, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(programCode) == "" {
		return 0, "", false
	}
	generation, err := strconv.ParseUint(generationText, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return generation, programCode, true
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

func (h *Handler) staleSelectionMessage(sess session) (string, [][]maxapi.Button, string) {
	switch sess.Step {
	case stepAwaitRegion:
		return "Эта кнопка уже устарела. Выбери регион из последнего сообщения.", regionKeyboardRows(), "устарело"
	case stepAwaitProgram:
		return "Эта кнопка уже устарела. Напиши код или название программы ещё раз.", restartKeyboardRows(), "устарело"
	case stepAwaitQual:
		return "Эта кнопка уже устарела. Выбери квалификацию из последнего сообщения.", h.qualKeyboard(sess), "устарело"
	case stepReady:
		return "Направление уже выбрано. Запусти анализ или начни заново.", h.readyKeyboard(sess), "устарело"
	case stepAnalyzing:
		return "Анализ уже выполняется. Дождись статистики и PDF либо начни выбор заново.", restartKeyboardRows(), "анализ идёт"
	default:
		return "Эта кнопка уже устарела. Начни выбор заново.", startKeyboardRows(), "устарело"
	}
}
