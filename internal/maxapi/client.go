package maxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	baseURL       = "https://platform-api2.max.ru" // Домен из документации
	limitPerSec   = rate.Limit(2)                  // Лимит: 2 сообщения в секунду
	burstCapacity = 1
)

// Client - обертка над HTTP-клиентом для безопасной работы с API МАХ
type Client struct {
	httpClient *http.Client
	token      string

	// Менеджер лимитеров. Ключ - chat_id
	limiters sync.Map
}

func NewClient(token string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		token:      token,
	}
}

// getLimiter потокобезопасно отдает лимитер для конкретного чата. Используется Double-Checked Locking с использованием конкурентной мапы
func (c *Client) getLimiter(chatID int64) *rate.Limiter {
	if val, ok := c.limiters.Load(chatID); ok {
		return val.(*rate.Limiter)
	}

	newLimiter := rate.NewLimiter(limitPerSec, burstCapacity)

	actual, _ := c.limiters.LoadOrStore(chatID, newLimiter)

	return actual.(*rate.Limiter)
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, req SendMessageRequest) error {
	limiter := c.getLimiter(chatID)
	if err := limiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter error for chat %d: %w", chatID, err)
	}

	if req.ChatID == nil {
		req.ChatID = &chatID
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/messages", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Обязательные заголовки по документации МАХ
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", c.token)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}

	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close max maxapi response body", "error", closeErr, "chat_id", chatID)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		// TODO в будущем можно добавить проверку на 429 или 500 для запуска механизма Retry
		return fmt.Errorf("max maxapi returned status: %d", resp.StatusCode)
	}

	return nil
}
