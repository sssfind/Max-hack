package bot

import (
	"Max-hack/internal/catalog"
	"Max-hack/internal/maxapi"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	payloadBegin    = "sg:begin"
	payloadHelp     = "sg:help"
	payloadRestart  = "sg:restart"
	payloadAnalyze  = "sg:analyze:"
	payloadRegPref  = "sg:reg:"
	payloadProgPref = "sg:p:"
	payloadQualPref = "sg:q:"

	maxButtonLabel = 58
	maxSearchHits  = 8
)

// Handler — бизнес-логика ответов на Update (сценарий SkillGap).
type Handler struct {
	api      messenger
	catalog  *catalog.Catalog
	sessions *sessionStore
	analysis *analysisRunner
}

func NewHandler(api messenger, cat *catalog.Catalog, analyzer marketAnalyzer, reports reportGenerator, cfg AnalysisConfig) *Handler {
	sessions := newSessionStore()
	h := &Handler{
		api:      api,
		catalog:  cat,
		sessions: sessions,
	}
	h.analysis = newAnalysisRunner(api, cat, sessions, analyzer, reports, cfg)
	return h
}

func (h *Handler) Handle(ctx context.Context, update maxapi.Update) {
	switch update.UpdateType {
	case maxapi.UpdateBotStarted:
		h.onStarted(ctx, update)
	case maxapi.UpdateMessageCreated:
		h.onMessage(ctx, update)
	case maxapi.UpdateMessageCallback:
		h.onCallback(ctx, update)
	case maxapi.UpdateBotAdded:
		slog.Info("Bot added to chat", "chat_id", update.ResolvedChatID(), "is_channel", update.IsChannel)
	case maxapi.UpdateBotRemoved, maxapi.UpdateBotStopped:
		slog.Info("Bot left or stopped", "type", update.UpdateType, "chat_id", update.ResolvedChatID())
	default:
		slog.Debug("Ignoring update type", "type", update.UpdateType, "chat_id", update.ResolvedChatID())
	}
}

func (h *Handler) onStarted(ctx context.Context, update maxapi.Update) {
	name := "друг"
	if update.User != nil && update.User.FirstName != "" {
		name = update.User.FirstName
	}
	text := fmt.Sprintf(
		"Привет, %s! Это SkillGap — помощник для девятиклассников, которые рассматривают СПО.\n\n"+
			"Я покажу, кем можно работать после программы, какие навыки ищут работодатели и где есть разрыв с образовательной программой.\n\n"+
			"Начнём с выбора направления: регион → программа СПО → квалификация.",
		name,
	)
	if err := h.send(ctx, update.ResolvedChatID(), update.ResolvedUserID(), text, startKeyboard()); err != nil {
		slog.Error("Failed to send welcome", "error", err, "chat_id", update.ResolvedChatID())
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
			slog.Error("Failed to send help", "error", err, "chat_id", chatID)
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
	slog.Info("SkillGap message", "chat_id", chatID, "step", string(sess.Step), "text_length", utf8.RuneCountInString(text))
	switch sess.Step {
	case stepAwaitRegion:
		h.handleRegionText(ctx, chatID, userID, sess, text)
	case stepAwaitProgram:
		h.handleProgramText(ctx, chatID, userID, sess, text)
	case stepAwaitQual:
		_ = h.send(ctx, chatID, userID, "Выбери квалификацию кнопкой ниже или нажми «Заново».", h.qualKeyboard(sess))
	case stepReady:
		_ = h.send(ctx, chatID, userID, "Направление уже выбрано. Можно запустить анализ или начать заново.", h.readyKeyboard(sess))
	case stepAnalyzing:
		_ = h.send(ctx, chatID, userID, "Анализ уже выполняется. Я пришлю статистику и PDF отдельными сообщениями.", nil)
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
	slog.Info("SkillGap callback", "chat_id", chatID, "payload", payload)

	if strings.HasPrefix(payload, payloadAnalyze) {
		generation, programCode, ok := parseAnalyzePayload(strings.TrimPrefix(payload, payloadAnalyze))
		if !ok {
			h.answerAnalysisCallback(ctx, update, "Эта кнопка анализа устарела. Используй кнопку из последнего сообщения.", "устарело")
			return
		}
		h.onAnalyzeCallback(ctx, update, generation, programCode)
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

	message := maxapi.NewMessageBody{Text: &msgText}
	if len(kb) > 0 {
		message.Attachments = []maxapi.AttachmentRequest{maxapi.InlineKeyboard(kb...)}
	}
	answer := maxapi.SendAnswerRequest{Notification: &notify, Message: &message}
	if err := h.api.AnswerCallback(ctx, update.Callback.CallbackID, chatID, answer); err != nil {
		slog.Error("Failed to answer callback", "error", err, "callback_id", update.Callback.CallbackID)
	}
}

func (h *Handler) beginDirection(ctx context.Context, chatID, userID int64) {
	text, kb := h.beginDirectionContent(chatID)
	if err := h.send(ctx, chatID, userID, text, kb); err != nil {
		slog.Error("Failed to begin direction", "error", err, "chat_id", chatID)
	}
}

func (h *Handler) beginDirectionContent(chatID int64) (string, [][]maxapi.Button) {
	h.sessions.reset(chatID)
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
	rows = append(rows, []maxapi.Button{maxapi.CallbackButton("🔄 Заново", payloadRestart)})
	_ = h.send(ctx, chatID, userID, b.String(), rows)
}

func (h *Handler) selectRegion(chatID int64, short string) (string, [][]maxapi.Button, string) {
	sess := h.sessions.get(chatID)
	if sess.Step != stepAwaitRegion {
		return staleSelectionMessage(sess)
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
		sess.SearchRegions = nil
		sess.Step = stepAwaitProgram
	}); !ok {
		return staleSelectionMessage(h.sessions.get(chatID))
	}
	msg := fmt.Sprintf(
		"Регион: %s\n\nШаг 2/3 — программа СПО\n\nНапиши код (например 09.02.07) или название программы / специальности.",
		r.Name,
	)
	kb := [][]maxapi.Button{{maxapi.CallbackButton("🔄 Заново", payloadRestart)}}
	return msg, kb, r.Name
}

func (h *Handler) handleProgramText(ctx context.Context, chatID, userID int64, sess session, text string) {
	hits := h.catalog.Search(text, maxSearchHits)
	if len(hits) == 0 {
		_ = h.send(ctx, chatID, userID,
			"Программа не найдена. Уточни код или название (например «09.02.07» или «информационные системы»).",
			[][]maxapi.Button{{maxapi.CallbackButton("🔄 Заново", payloadRestart)}},
		)
		return
	}
	if len(hits) == 1 {
		msg, kb, _ := h.applyProgram(chatID, sess.Generation, hits[0])
		_ = h.send(ctx, chatID, userID, msg, kb)
		return
	}

	sess.SearchPrograms = make([]string, 0, len(hits))
	rows := make([][]maxapi.Button, 0, len(hits)+1)
	var b strings.Builder
	b.WriteString("Нашёл несколько программ — выбери нужную:\n\n")
	for _, p := range hits {
		sess.SearchPrograms = append(sess.SearchPrograms, p.Code)
		status := "актуальна"
		if !p.IsCurrent {
			status = "историческая"
		}
		b.WriteString(fmt.Sprintf("• %s — %s (%s, %s)\n", p.Code, p.ProgramName, p.ProgramType, status))
		label := truncate(fmt.Sprintf("%s %s", p.Code, p.ProgramName), maxButtonLabel)
		rows = append(rows, []maxapi.Button{
			maxapi.CallbackButton(label, payloadProgPref+p.Code),
		})
	}
	if _, ok := h.sessions.updateIf(chatID, sess.Generation, stepAwaitProgram, func(current *session) {
		current.SearchPrograms = append([]string(nil), sess.SearchPrograms...)
	}); !ok {
		_ = h.send(ctx, chatID, userID, "Выбор уже изменился. Используй кнопки из последнего сообщения.", startKeyboardRows())
		return
	}
	rows = append(rows, []maxapi.Button{maxapi.CallbackButton("🔄 Заново", payloadRestart)})
	_ = h.send(ctx, chatID, userID, b.String(), rows)
}

func (h *Handler) selectProgram(chatID int64, code string) (string, [][]maxapi.Button, string) {
	sess := h.sessions.get(chatID)
	if sess.Step != stepAwaitProgram || !containsFold(sess.SearchPrograms, code) {
		return staleSelectionMessage(sess)
	}
	p, ok := h.catalog.Get(code)
	if !ok {
		return "Программа не найдена. Напиши код или название ещё раз.",
			[][]maxapi.Button{{maxapi.CallbackButton("🔄 Заново", payloadRestart)}},
			"ошибка"
	}
	return h.applyProgram(chatID, sess.Generation, p)
}

func (h *Handler) applyProgram(chatID int64, generation uint64, p *catalog.Program) (string, [][]maxapi.Button, string) {
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
		return staleSelectionMessage(h.sessions.get(chatID))
	}
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
		return staleSelectionMessage(sess)
	}
	p, ok := h.catalog.Get(sess.ProgramCode)
	if !ok {
		return "Сначала выбери программу СПО.", [][]maxapi.Button{{maxapi.CallbackButton("🔄 Заново", payloadRestart)}}, "ошибка"
	}
	if idx < 0 || idx >= len(p.Qualifications) {
		return "Некорректная квалификация. Выбери кнопку из списка.", h.qualKeyboard(sess), "ошибка"
	}
	sess, ok = h.sessions.updateIf(chatID, sess.Generation, stepAwaitQual, func(sess *session) {
		sess.Qualification = p.Qualifications[idx]
		sess.Step = stepReady
	})
	if !ok {
		return staleSelectionMessage(h.sessions.get(chatID))
	}
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
	if url := p.SourceURL(); url != "" {
		b.WriteString("\n\nФедеральный источник программы: ")
		b.WriteString(url)
	}
	return b.String()
}

func (h *Handler) qualKeyboard(sess session) [][]maxapi.Button {
	p, ok := h.catalog.Get(sess.ProgramCode)
	if !ok {
		return [][]maxapi.Button{{maxapi.CallbackButton("🔄 Заново", payloadRestart)}}
	}
	rows := make([][]maxapi.Button, 0, len(p.Qualifications)+1)
	for i, q := range p.Qualifications {
		rows = append(rows, []maxapi.Button{
			maxapi.CallbackButton(truncate(q, maxButtonLabel), fmt.Sprintf("%s%s:%d", payloadQualPref, p.Code, i)),
		})
	}
	rows = append(rows, []maxapi.Button{maxapi.CallbackButton("🔄 Заново", payloadRestart)})
	return rows
}

func (h *Handler) readyKeyboard(sess session) [][]maxapi.Button {
	rows := [][]maxapi.Button{
		{maxapi.CallbackButton("📊 Запустить анализ", fmt.Sprintf("%s%d:%s", payloadAnalyze, sess.Generation, sess.ProgramCode))},
		{maxapi.CallbackButton("🔄 Выбрать заново", payloadRestart)},
	}
	if p, ok := h.catalog.Get(sess.ProgramCode); ok {
		if url := p.SourceURL(); url != "" {
			rows = append([][]maxapi.Button{
				{maxapi.LinkButton("📄 Источник программы", url)},
			}, rows...)
		}
	}
	return rows
}

func (h *Handler) send(ctx context.Context, chatID, userID int64, text string, rows [][]maxapi.Button) error {
	body := maxapi.NewMessageBody{Text: &text}
	if len(rows) > 0 {
		body.Attachments = []maxapi.AttachmentRequest{maxapi.InlineKeyboard(rows...)}
	}
	return h.api.SendMessage(ctx, chatID, userID, body)
}

func startKeyboard() [][]maxapi.Button {
	return startKeyboardRows()
}

func startKeyboardRows() [][]maxapi.Button {
	return [][]maxapi.Button{
		{maxapi.CallbackButton("🎯 Выбрать направление", payloadBegin)},
		{maxapi.CallbackButton("❓ Справка", payloadHelp)},
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
	rows = append(rows, []maxapi.Button{maxapi.CallbackButton("🔄 Заново", payloadRestart)})
	return rows
}

func helpText() string {
	return "SkillGap помогает понять перспективы после программы СПО.\n\n" +
		"Команды:\n• /start — приветствие\n• /help — справка\n• /skillgap — выбор направления\n\n" +
		"Сценарий:\n1) регион\n2) программа СПО (код или название)\n3) квалификация (если их несколько)\n4) список возможных должностей и запуск анализа рынка"
}

func truncate(s string, max int) string {
	if max <= 1 || utf8.RuneCountInString(s) <= max {
		return s
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

func staleSelectionMessage(sess session) (string, [][]maxapi.Button, string) {
	switch sess.Step {
	case stepAwaitRegion:
		return "Эта кнопка уже устарела. Выбери регион из последнего сообщения.", regionKeyboardRows(), "устарело"
	case stepAwaitProgram:
		return "Эта кнопка уже устарела. Напиши код или название программы ещё раз.", [][]maxapi.Button{{maxapi.CallbackButton("🔄 Заново", payloadRestart)}}, "устарело"
	case stepAwaitQual:
		return "Эта кнопка уже устарела. Выбери квалификацию из последнего сообщения.", nil, "устарело"
	case stepReady:
		return "Направление уже выбрано. Запусти анализ или начни заново.", nil, "устарело"
	case stepAnalyzing:
		return "Анализ уже выполняется. Дождись статистики и PDF либо начни выбор заново.", nil, "анализ идёт"
	default:
		return "Эта кнопка уже устарела. Начни выбор заново.", startKeyboardRows(), "устарело"
	}
}
