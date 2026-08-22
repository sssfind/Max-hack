package maxapi

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

type Update struct {
	UpdateType string `json:"update_type"`
	Timestamp  int64  `json:"timestamp"`
	ChatID     int64  `json:"chat_id"`
	User       *User  `json:"user,omitempty"`
	IsChannel  bool   `json:"is_channel"`
}

type User struct {
	UserID           int64   `json:"user_id"`
	FirstName        string  `json:"first_name"`
	LastName         *string `json:"last_name,omitempty"`
	Username         *string `json:"username,omitempty"`
	IsBot            bool    `json:"is_bot"`
	LastActivityTime int64   `json:"last_activity_time,omitempty"`
}

type AttachmentRequest struct {
	Type    string      `json:"type"` // "image", "video", "file", "inline_keyboard", etc
	Payload interface{} `json:"payload"`
}

type NewMessageBody struct {
	Text        *string             `json:"text,omitempty"`
	Attachments []AttachmentRequest `json:"attachments,omitempty"`
	Notify      bool                `json:"notify"`
	Format      *string             `json:"format,omitempty"`
}

type SendAnswerRequest struct {
	Message *NewMessageBody `json:"message,omitempty"`
}

type SendMessageRequest struct {
	UserID             *int64              `json:"user_id,omitempty"`
	ChatID             *int64              `json:"chat_id,omitempty"`
	DisableLinkPreview bool                `json:"disable_link_preview,omitempty"`
	Text               *string             `json:"text,omitempty"`
	Attachments        []AttachmentRequest `json:"attachments,omitempty"`
	Notify             bool                `json:"notify"`           // По умолчанию true
	Format             *string             `json:"format,omitempty"` // markdown" или "html"
}
