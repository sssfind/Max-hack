package maxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	baseURL         = "https://platform-api2.max.ru"
	limitPerSec     = rate.Limit(2) // max 2 messages/answers per chat per second
	burstCapacity   = 1
	globalLimitRPS  = rate.Limit(25) // docs recommend ≤30 rps to the platform
	globalBurst     = 10
	maxHTTPAttempts = 3
)

// Client — HTTP-клиент MAX API с лимитами и ретраями.
type Client struct {
	httpClient *http.Client
	token      string
	global     *rate.Limiter
	limiters   sync.Map
}

func NewClient(token string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		token:      token,
		global:     rate.NewLimiter(globalLimitRPS, globalBurst),
	}
}

func (c *Client) getLimiter(chatID int64) *rate.Limiter {
	if val, ok := c.limiters.Load(chatID); ok {
		return val.(*rate.Limiter)
	}
	newLimiter := rate.NewLimiter(limitPerSec, burstCapacity)
	actual, _ := c.limiters.LoadOrStore(chatID, newLimiter)
	return actual.(*rate.Limiter)
}

func (c *Client) waitLimits(ctx context.Context, chatID int64) error {
	if err := c.global.Wait(ctx); err != nil {
		return fmt.Errorf("global rate limiter: %w", err)
	}
	if chatID == 0 {
		return nil
	}
	if err := c.getLimiter(chatID).Wait(ctx); err != nil {
		return fmt.Errorf("chat %d rate limiter: %w", chatID, err)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, reqBody any, out any) error {
	var payload []byte
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		payload = raw
	}

	endpoint := baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var lastErr error
	for attempt := 1; attempt <= maxHTTPAttempts; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}

		httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Authorization", c.token)
		if payload != nil {
			httpReq.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("network error: %w", err)
			if attempt < maxHTTPAttempts {
				time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
				continue
			}
			return lastErr
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read response: %w", readErr)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("decode response: %w", err)
				}
			}
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			lastErr = fmt.Errorf("max api status %d: %s", resp.StatusCode, string(respBody))
			if attempt < maxHTTPAttempts {
				time.Sleep(time.Duration(attempt*attempt) * 300 * time.Millisecond)
				continue
			}
			return lastErr
		default:
			return fmt.Errorf("max api status %d: %s", resp.StatusCode, string(respBody))
		}
	}
	return lastErr
}

func (c *Client) GetMe(ctx context.Context) (*BotInfo, error) {
	if err := c.global.Wait(ctx); err != nil {
		return nil, err
	}
	var info BotInfo
	if err := c.doJSON(ctx, http.MethodGet, "/me", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *Client) SetCommands(ctx context.Context, commands []BotCommand) error {
	if err := c.global.Wait(ctx); err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodPatch, "/me/commands", nil, SetCommandsRequest{Commands: commands}, nil)
}

func (c *Client) Subscribe(ctx context.Context, webhookURL, secret string, updateTypes []string) error {
	if err := c.global.Wait(ctx); err != nil {
		return err
	}
	var result SimpleResult
	err := c.doJSON(ctx, http.MethodPost, "/subscriptions", nil, SubscriptionRequest{
		URL:         webhookURL,
		Secret:      secret,
		UpdateTypes: updateTypes,
	}, &result)
	if err != nil {
		return err
	}
	if !result.Success && result.Message != "" {
		return fmt.Errorf("subscribe failed: %s", result.Message)
	}
	return nil
}

// SendMessage отправляет сообщение. chat_id и user_id — query-параметры по доке.
func (c *Client) SendMessage(ctx context.Context, chatID, userID int64, body NewMessageBody) error {
	limiterKey := chatID
	if limiterKey == 0 {
		limiterKey = userID
	}
	if err := c.waitLimits(ctx, limiterKey); err != nil {
		return err
	}

	query := url.Values{}
	if chatID != 0 {
		query.Set("chat_id", strconv.FormatInt(chatID, 10))
	}
	if userID != 0 {
		query.Set("user_id", strconv.FormatInt(userID, 10))
	}
	if len(query) == 0 {
		return fmt.Errorf("either chat_id or user_id is required")
	}

	return c.doJSON(ctx, http.MethodPost, "/messages", query, body, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID string, chatID int64, req SendAnswerRequest) error {
	if callbackID == "" {
		return fmt.Errorf("callback_id is required")
	}
	if err := c.waitLimits(ctx, chatID); err != nil {
		return err
	}
	query := url.Values{}
	query.Set("callback_id", callbackID)
	var result SimpleResult
	if err := c.doJSON(ctx, http.MethodPost, "/answers", query, req, &result); err != nil {
		return err
	}
	if !result.Success && result.Message != "" {
		slog.Warn("callback answer returned unsuccessful", "message", result.Message, "callback_id", callbackID)
	}
	return nil
}

// InlineKeyboard собирает attachment inline_keyboard.
func InlineKeyboard(rows ...[]Button) AttachmentRequest {
	return AttachmentRequest{
		Type:    "inline_keyboard",
		Payload: InlineKeyboardPayload{Buttons: rows},
	}
}

func CallbackButton(text, payload string) Button {
	return Button{Type: "callback", Text: text, Payload: payload}
}

func LinkButton(text, url string) Button {
	return Button{Type: "link", Text: text, URL: url}
}

func Ptr[T any](v T) *T { return &v }
