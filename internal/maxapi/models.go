package maxapi

import "encoding/json"

const (
	UpdateBotAdded        = "bot_added"
	UpdateBotStarted      = "bot_started"
	UpdateBotStopped      = "bot_stopped"
	UpdateBotRemoved      = "bot_removed"
	UpdateMessageCreated  = "message_created"
	UpdateMessageCallback = "message_callback"
	UpdateMessageEdited   = "message_edited"
	UpdateMessageRemoved  = "message_removed"
)

type Chat struct {
	ChatID            int64   `json:"chat_id"`
	Type              string  `json:"type"`   // "chat", "channel", "dialog"
	Status            string  `json:"status"` // "active", "removed", "left", "closed"
	Title             *string `json:"title,omitempty"`
	LastEventTime     int64   `json:"last_event_time"`
	ParticipantsCount int32   `json:"participants_count"`
	OwnerID           *int64  `json:"owner_id,omitempty"`
	IsPublic          bool    `json:"is_public"`
	Description       *string `json:"description,omitempty"`
	MessagesCount     *int    `json:"messages_count,omitempty"`
}

type User struct {
	UserID           int64   `json:"user_id"`
	FirstName        string  `json:"first_name"`
	LastName         *string `json:"last_name,omitempty"`
	Username         *string `json:"username,omitempty"`
	Name             string  `json:"name,omitempty"`
	IsBot            bool    `json:"is_bot"`
	LastActivityTime int64   `json:"last_activity_time,omitempty"`
}

type BotCommand struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type BotInfo struct {
	User
	Description   string       `json:"description,omitempty"`
	AvatarURL     string       `json:"avatar_url,omitempty"`
	FullAvatarURL string       `json:"full_avatar_url,omitempty"`
	Commands      []BotCommand `json:"commands,omitempty"`
}

type Recipient struct {
	ChatID   int64  `json:"chat_id"`
	ChatType string `json:"chat_type"`
	UserID   int64  `json:"user_id"`
}

type MessageBody struct {
	Mid  string `json:"mid"`
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

type Message struct {
	Sender    *User       `json:"sender,omitempty"`
	Recipient Recipient   `json:"recipient"`
	Timestamp int64       `json:"timestamp"`
	Body      MessageBody `json:"body"`
	URL       *string     `json:"url,omitempty"`
}

type Callback struct {
	Timestamp  int64  `json:"timestamp"`
	CallbackID string `json:"callback_id"`
	Payload    string `json:"payload"`
	User       User   `json:"user"`
}

// Update — событие webhook / long polling.
// Для message_* chat_id часто лежит в message.recipient.chat_id.
type Update struct {
	UpdateType string    `json:"update_type"`
	Timestamp  int64     `json:"timestamp"`
	ChatID     int64     `json:"chat_id"`
	UserID     int64     `json:"user_id,omitempty"`
	UserLocale string    `json:"user_locale,omitempty"`
	User       *User     `json:"user,omitempty"`
	IsChannel  bool      `json:"is_channel"`
	MessageID  string    `json:"message_id,omitempty"`
	Message    *Message  `json:"message,omitempty"`
	Callback   *Callback `json:"callback,omitempty"`
}

func (u Update) ResolvedChatID() int64 {
	if u.ChatID != 0 {
		return u.ChatID
	}
	if u.Message != nil && u.Message.Recipient.ChatID != 0 {
		return u.Message.Recipient.ChatID
	}
	return 0
}

func (u Update) ResolvedUserID() int64 {
	if u.User != nil && u.User.UserID != 0 {
		return u.User.UserID
	}
	if u.UserID != 0 {
		return u.UserID
	}
	if u.Callback != nil && u.Callback.User.UserID != 0 {
		return u.Callback.User.UserID
	}
	if u.Message != nil && u.Message.Sender != nil && u.Message.Sender.UserID != 0 {
		return u.Message.Sender.UserID
	}
	return 0
}

type Button struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
	URL     string `json:"url,omitempty"`
}

type InlineKeyboardPayload struct {
	Buttons [][]Button `json:"buttons"`
}

type UploadType string

const UploadTypeFile UploadType = "file"

// UploadEndpoint is the signed, absolute URL returned by POST /uploads.
// Token is optional because file uploads receive their token only after the
// multipart body has been uploaded to URL.
type UploadEndpoint struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// FileUploadResult is returned by the external upload host after a file has
// been accepted. FileID is intentionally opaque; only Token is needed when
// attaching the file to a message.
type FileUploadResult struct {
	FileID json.RawMessage `json:"fileId,omitempty"`
	Token  string          `json:"token"`
}

type UploadedInfo struct {
	Token string `json:"token"`
}

type AttachmentRequest struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type NewMessageBody struct {
	Text        *string             `json:"text,omitempty"`
	Attachments []AttachmentRequest `json:"attachments,omitempty"`
	Notify      *bool               `json:"notify,omitempty"`
	Format      *string             `json:"format,omitempty"`
}

type SendAnswerRequest struct {
	Message      *NewMessageBody `json:"message,omitempty"`
	Notification *string         `json:"notification,omitempty"`
}

type SubscriptionRequest struct {
	URL         string   `json:"url"`
	UpdateTypes []string `json:"update_types,omitempty"`
	Secret      string   `json:"secret,omitempty"`
}

type SimpleResult struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

type SetCommandsRequest struct {
	Commands []BotCommand `json:"commands"`
}
