package bot

import (
	"Max-hack/internal/maxapi"
	"context"
	"log/slog"
	"strings"
)

const (
	payloadHelp  = "help"
	payloadAbout = "about"
	payloadPing  = "ping"
)

// Handler — бизнес-логика ответов на Update.
type Handler struct {
	api *maxapi.Client
}

func NewHandler(api *maxapi.Client) *Handler {
	return &Handler{api: api}
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
	text := "Привет, " + name + "! Я бот на MAX API.\n\nКоманды: /start, /help\nИли нажми кнопку ниже."
	if err := h.sendMenu(ctx, update.ResolvedChatID(), update.ResolvedUserID(), text); err != nil {
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
	case isCommand(text, "help"):
		if err := h.sendMenu(ctx, chatID, userID, helpText()); err != nil {
			slog.Error("Failed to send help", "error", err, "chat_id", chatID)
		}
	case text == "":
		_ = h.api.SendMessage(ctx, chatID, userID, maxapi.NewMessageBody{
			Text: maxapi.Ptr("Пришли текст или нажми /help"),
		})
	default:
		reply := "Эхо: " + text
		if err := h.api.SendMessage(ctx, chatID, userID, maxapi.NewMessageBody{Text: &reply}); err != nil {
			slog.Error("Failed to echo message", "error", err, "chat_id", chatID)
		}
	}
}

func (h *Handler) onCallback(ctx context.Context, update maxapi.Update) {
	if update.Callback == nil {
		return
	}
	chatID := update.ResolvedChatID()
	payload := update.Callback.Payload

	var (
		msgText string
		notify  string
	)
	switch payload {
	case payloadHelp:
		msgText = helpText()
		notify = "Справка"
	case payloadAbout:
		msgText = "Бот на Go + webhook MAX (platform-api2.max.ru).\nЛимит: 2 msg/s на чат, ≤30 rps глобально."
		notify = "О боте"
	case payloadPing:
		msgText = "pong ✅"
		notify = "pong"
	default:
		msgText = "Неизвестная кнопка: " + payload
		notify = "ок"
	}

	answer := maxapi.SendAnswerRequest{
		Notification: &notify,
		Message: &maxapi.NewMessageBody{
			Text:        &msgText,
			Attachments: []maxapi.AttachmentRequest{menuKeyboard()},
		},
	}
	if err := h.api.AnswerCallback(ctx, update.Callback.CallbackID, chatID, answer); err != nil {
		slog.Error("Failed to answer callback", "error", err, "callback_id", update.Callback.CallbackID)
	}
}

func (h *Handler) sendMenu(ctx context.Context, chatID, userID int64, text string) error {
	return h.api.SendMessage(ctx, chatID, userID, maxapi.NewMessageBody{
		Text:        &text,
		Attachments: []maxapi.AttachmentRequest{menuKeyboard()},
	})
}

func menuKeyboard() maxapi.AttachmentRequest {
	return maxapi.InlineKeyboard(
		[]maxapi.Button{
			maxapi.CallbackButton("❓ Помощь", payloadHelp),
			maxapi.CallbackButton("ℹ️ О боте", payloadAbout),
		},
		[]maxapi.Button{
			maxapi.CallbackButton("🏓 Ping", payloadPing),
		},
	)
}

func helpText() string {
	return "Доступно:\n• /start — приветствие и меню\n• /help — эта справка\n• любой текст — эхо\n• кнопки — callback через POST /answers"
}

func isCommand(text, name string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if !strings.HasPrefix(t, "/") {
		return false
	}
	cmd := strings.SplitN(t[1:], "@", 2)[0]
	cmd = strings.SplitN(cmd, " ", 2)[0]
	return strings.EqualFold(cmd, name)
}
