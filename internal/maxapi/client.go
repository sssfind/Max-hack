package maxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultBaseURL                = "https://platform-api2.max.ru"
	limitPerSec                   = rate.Limit(2) // max 2 messages/answers per chat per second
	burstCapacity                 = 1
	globalLimitRPS                = rate.Limit(25) // docs recommend ≤30 rps to the platform
	globalBurst                   = 10
	maxHTTPAttempts               = 3
	maxAPIResponseSize            = 4 << 20
	maxFileUploadSize             = 20 << 20 // reports are small; cap memory and upload time at 20 MiB
	maxUploadResponseSize         = 1 << 20
	attachmentReadyAttempts       = 4
	attachmentReadyInitialBackoff = 250 * time.Millisecond
)

var blockedUploadPrefixes = []netip.Prefix{
	// IPv4 special-purpose ranges that are private, local, documentation,
	// benchmarking, multicast, reserved, or otherwise not globally reachable.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// IPv6 local/special-use, transition, documentation and multicast ranges.
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

var publicIPv6Prefix = netip.MustParsePrefix("2000::/3")

// MAX documents upload URLs on oneme.ru and okcdn.ru. max.ru is retained for
// API-owned upload endpoints. Restricting production uploads to provider-owned
// domains prevents a compromised or malformed response from exfiltrating a
// report to an arbitrary public host; IP checks below remain defense in depth.
var trustedUploadDomains = []string{"oneme.ru", "okcdn.ru", "max.ru"}

// Client — HTTP-клиент MAX API с лимитами и ретраями.
type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
	global     *rate.Limiter
	limiters   *chatLimiterRegistry
	waitFn     func(context.Context, time.Duration) error
	lookupIP   func(context.Context, string) ([]net.IPAddr, error)
}

func NewClient(token string) *Client {
	return newClient(token, defaultBaseURL, &http.Client{Timeout: 15 * time.Second})
}

// newClient keeps transport and endpoint injection package-private so tests
// can use httptest without exposing configuration that production does not need.
func newClient(token, apiBaseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	apiBaseURL = strings.TrimRight(apiBaseURL, "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultBaseURL
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    apiBaseURL,
		token:      token,
		global:     rate.NewLimiter(globalLimitRPS, globalBurst),
		limiters:   newChatLimiterRegistry(defaultChatLimiterCapacity, defaultChatLimiterIdleTTL),
		waitFn:     waitContext,
		lookupIP:   net.DefaultResolver.LookupIPAddr,
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) wait(ctx context.Context, delay time.Duration) error {
	if c.waitFn != nil {
		return c.waitFn(ctx, delay)
	}
	return waitContext(ctx, delay)
}

func (c *Client) waitLimits(ctx context.Context, chatID int64) error {
	if err := c.global.Wait(ctx); err != nil {
		return fmt.Errorf("global rate limiter: %w", err)
	}
	if chatID == 0 {
		return nil
	}
	limiter, release := c.limiters.acquire(chatID)
	defer release()
	if err := limiter.Wait(ctx); err != nil {
		return fmt.Errorf("per-chat rate limiter: %w", err)
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

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var lastErr error
	retryableMethod := canRetryHTTPMethod(method)
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
			if retryableMethod && attempt < maxHTTPAttempts {
				if waitErr := c.wait(ctx, time.Duration(attempt)*200*time.Millisecond); waitErr != nil {
					return errors.Join(lastErr, waitErr)
				}
				continue
			}
			return lastErr
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read response: %w", readErr)
		}
		if len(respBody) > maxAPIResponseSize {
			return fmt.Errorf("read response: body exceeds %d bytes", maxAPIResponseSize)
		}

		if resp.StatusCode == http.StatusOK {
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("decode response: %w", err)
				}
			}
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			lastErr = newAPIError(resp.StatusCode, respBody)
			if retryableMethod && attempt < maxHTTPAttempts {
				if waitErr := c.wait(ctx, time.Duration(attempt*attempt)*300*time.Millisecond); waitErr != nil {
					return errors.Join(lastErr, waitErr)
				}
				continue
			}
			return lastErr
		}
		return newAPIError(resp.StatusCode, respBody)
	}
	return lastErr
}

// Retrying a POST/PATCH after a lost response can duplicate a message or a
// callback answer. Only methods whose semantics are read-only are retried here;
// explicitly safe POST retries (attachment.not.ready) live at the call site.
func canRetryHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// APIError preserves MAX's machine-readable error code so callers can retry
// only errors that are explicitly safe to retry.
type APIError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"code,omitempty"`
	Err        string `json:"error,omitempty"`
	Message    string `json:"message,omitempty"`
}

// IsRetryableDeliveryError classifies failures for a caller that has already
// persisted the exact outbound request. Explicit client-side rejections are
// terminal; network failures, cancellation/timeouts, throttling and 5xx
// responses may be replayed from that durable receipt.
func IsRetryableDeliveryError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusRequestTimeout || apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func (e *APIError) Error() string {
	detail := e.Message
	if detail == "" {
		detail = e.Err
	}
	if detail == "" {
		detail = http.StatusText(e.StatusCode)
	}
	if e.Code != "" {
		return fmt.Sprintf("max api status %d (%s): %s", e.StatusCode, e.Code, detail)
	}
	return fmt.Sprintf("max api status %d: %s", e.StatusCode, detail)
}

func newAPIError(statusCode int, body []byte) *APIError {
	apiErr := &APIError{StatusCode: statusCode}
	if err := json.Unmarshal(body, apiErr); err != nil {
		apiErr.Message = strings.TrimSpace(string(body))
	}
	return apiErr
}

func isAttachmentNotReady(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == "attachment.not.ready"
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
	if !result.Success {
		message := strings.TrimSpace(result.Message)
		if message == "" {
			message = "MAX returned success=false"
		}
		return fmt.Errorf("subscribe failed: %s", message)
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

// UploadFile uploads an in-memory file to MAX and returns the opaque token
// needed by a file attachment. It intentionally accepts bounded bytes: reports
// are generated in memory and should never make the bot buffer multi-gigabyte
// files even though the platform itself supports them.
func (c *Client) UploadFile(ctx context.Context, filename string, data []byte) (string, error) {
	cleanName, err := safeUploadFilename(filename)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("upload file: data is empty")
	}
	if len(data) > maxFileUploadSize {
		return "", fmt.Errorf("upload file: size %d exceeds %d byte limit", len(data), maxFileUploadSize)
	}

	if err := c.global.Wait(ctx); err != nil {
		return "", fmt.Errorf("upload file rate limiter: %w", err)
	}
	query := url.Values{"type": {string(UploadTypeFile)}}
	var endpoint UploadEndpoint
	if err := c.doJSON(ctx, http.MethodPost, "/uploads", query, nil, &endpoint); err != nil {
		return "", fmt.Errorf("request upload URL: %w", err)
	}
	if err := validateUploadURL(endpoint.URL, c.allowPrivateUploadHost()); err != nil {
		return "", err
	}

	token, err := c.uploadMultipart(ctx, endpoint.URL, cleanName, data)
	if err != nil {
		return "", err
	}
	return token, nil
}

// SendFile uploads a file and sends it as a message, optionally with an inline
// keyboard. MAX may acknowledge the upload before attachment processing is
// complete; only that explicit error is retried, with a short exponential backoff.
func (c *Client) SendFile(ctx context.Context, chatID, userID int64, filename string, data []byte, caption string, keyboard ...[]Button) error {
	if chatID == 0 && userID == 0 {
		return fmt.Errorf("send file: either chat_id or user_id is required")
	}

	token, err := c.UploadFile(ctx, filename, data)
	if err != nil {
		return err
	}

	body := NewMessageBody{
		Attachments: []AttachmentRequest{FileAttachment(token)},
	}
	if len(keyboard) > 0 {
		body.Attachments = append(body.Attachments, InlineKeyboard(keyboard...))
	}
	if caption != "" {
		body.Text = Ptr(caption)
	}

	var lastErr error
	for attempt := 0; attempt < attachmentReadyAttempts; attempt++ {
		lastErr = c.SendMessage(ctx, chatID, userID, body)
		if lastErr == nil {
			return nil
		}
		if !isAttachmentNotReady(lastErr) {
			return fmt.Errorf("send uploaded file: %w", lastErr)
		}
		if attempt == attachmentReadyAttempts-1 {
			break
		}

		delay := attachmentReadyInitialBackoff * time.Duration(1<<attempt)
		if err := c.wait(ctx, delay); err != nil {
			return fmt.Errorf("wait for uploaded file processing: %w", err)
		}
	}
	return fmt.Errorf("send uploaded file after %d attempts: %w", attachmentReadyAttempts, lastErr)
}

func safeUploadFilename(filename string) (string, error) {
	filename = strings.TrimSpace(filename)
	filename = strings.ReplaceAll(filename, "\\", "/")
	filename = path.Base(filename)
	if filename == "" || filename == "." || filename == ".." || filename == "/" {
		return "", fmt.Errorf("upload file: filename is required")
	}
	if len(filename) > 255 {
		return "", fmt.Errorf("upload file: filename is too long")
	}
	if strings.IndexFunc(filename, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", fmt.Errorf("upload file: filename contains control characters")
	}
	return filename, nil
}

func validateUploadURL(rawURL string, allowPrivate bool) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("upload file: invalid upload URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("upload file: upload URL must be an absolute HTTPS URL")
	}
	hostname := normalizeUploadHostname(parsed.Hostname())
	if hostname == "" {
		return fmt.Errorf("upload file: upload URL has no hostname")
	}
	if !allowPrivate {
		if !isTrustedUploadHost(hostname) {
			return fmt.Errorf("upload file: upload host is not a trusted MAX upload domain")
		}
		if isPrivateUploadHost(hostname) {
			return fmt.Errorf("upload file: private or local upload host is not allowed")
		}
	}
	return nil
}

func isTrustedUploadHost(hostname string) bool {
	hostname = normalizeUploadHostname(hostname)
	for _, domain := range trustedUploadDomains {
		if hostname == domain || strings.HasSuffix(hostname, "."+domain) {
			return true
		}
	}
	return false
}

func (c *Client) allowPrivateUploadHost() bool {
	parsed, err := url.Parse(c.baseURL)
	return err == nil && isPrivateUploadHost(normalizeUploadHostname(parsed.Hostname()))
}

func isPrivateUploadHost(hostname string) bool {
	hostname = normalizeUploadHostname(hostname)
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && isPrivateUploadIP(ip)
}

func normalizeUploadHostname(hostname string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(hostname)), ".")
}

func isPrivateUploadIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	// Currently assigned public IPv6 unicast space is within 2000::/3.
	// Fail closed for legacy site-local and other reserved unicast blocks that
	// net.IP.IsGlobalUnicast intentionally classifies as unicast.
	if address.Is6() && !publicIPv6Prefix.Contains(address) {
		return true
	}
	for _, prefix := range blockedUploadPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (c *Client) uploadMultipart(ctx context.Context, uploadURL, filename string, data []byte) (string, error) {
	allowPrivate := c.allowPrivateUploadHost()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("data", filename)
	if err != nil {
		return "", fmt.Errorf("upload file: create multipart field: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("upload file: write multipart field: %w", err)
	}
	contentType := writer.FormDataContentType()
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("upload file: close multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", fmt.Errorf("upload file: create multipart request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(body.Len())

	uploadClient := *c.httpClient
	if !allowPrivate {
		transport, err := guardedUploadTransport(uploadClient.Transport, c.lookupIP)
		if err != nil {
			return "", err
		}
		uploadClient.Transport = transport
	}
	previousRedirectCheck := uploadClient.CheckRedirect
	uploadClient.CheckRedirect = func(redirected *http.Request, via []*http.Request) error {
		if err := validateUploadURL(redirected.URL.String(), allowPrivate); err != nil {
			return err
		}
		if previousRedirectCheck != nil {
			return previousRedirectCheck(redirected, via)
		}
		if len(via) >= 10 {
			return errors.New("upload file: stopped after 10 redirects")
		}
		return nil
	}
	resp, err := uploadClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload file: network error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadResponseSize+1))
	if err != nil {
		return "", fmt.Errorf("upload file: read response: %w", err)
	}
	if len(responseBody) > maxUploadResponseSize {
		return "", fmt.Errorf("upload file: response exceeds %d bytes", maxUploadResponseSize)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload file: %w", newAPIError(resp.StatusCode, responseBody))
	}

	var result FileUploadResult
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("upload file: decode response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("upload file: response contains no token")
	}
	return result.Token, nil
}

func guardedUploadTransport(
	base http.RoundTripper,
	lookupIP func(context.Context, string) ([]net.IPAddr, error),
) (*http.Transport, error) {
	var transport *http.Transport
	switch typed := base.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = typed.Clone()
	default:
		return nil, fmt.Errorf("upload file: unsupported HTTP transport for guarded upload")
	}
	if lookupIP == nil {
		lookupIP = net.DefaultResolver.LookupIPAddr
	}

	baseDial := transport.DialContext
	if baseDial == nil {
		dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		baseDial = dialer.DialContext
	}
	// A proxy or a custom TLS dialer could resolve the untrusted hostname after
	// our check. Direct all upload connections through the guarded dialer.
	transport.Proxy = nil
	transport.DialTLS = nil //nolint:staticcheck // The deprecated hook must be cleared so it cannot bypass the guarded DialContext.
	transport.DialTLSContext = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("upload file: invalid dial address: %w", err)
		}
		host = normalizeUploadHostname(host)
		if host == "" || isPrivateUploadHost(host) {
			return nil, fmt.Errorf("upload file: private or local upload host is not allowed")
		}

		var addresses []net.IPAddr
		if literal := net.ParseIP(host); literal != nil {
			addresses = []net.IPAddr{{IP: literal}}
		} else {
			addresses, err = lookupIP(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("upload file: resolve upload host: %w", err)
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("upload file: upload host resolved to no addresses")
		}
		for _, resolved := range addresses {
			if isPrivateUploadIP(resolved.IP) {
				return nil, fmt.Errorf("upload file: upload host resolved to a private or local address")
			}
		}

		var lastErr error
		for _, resolved := range addresses {
			conn, dialErr := baseDial(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, fmt.Errorf("upload file: connect to upload host: %w", lastErr)
	}
	return transport, nil
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
	if !result.Success {
		message := strings.TrimSpace(result.Message)
		if message == "" {
			message = "MAX returned success=false"
		}
		return fmt.Errorf("callback answer failed: %s", message)
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

func FileAttachment(token string) AttachmentRequest {
	return AttachmentRequest{
		Type:    "file",
		Payload: UploadedInfo{Token: token},
	}
}

func CallbackButton(text, payload string) Button {
	return Button{Type: "callback", Text: text, Payload: payload}
}

func LinkButton(text, url string) Button {
	return Button{Type: "link", Text: text, URL: url}
}

func Ptr[T any](v T) *T { return &v }
