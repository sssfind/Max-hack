package skillgap

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func validateSourceURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || !u.IsAbs() {
		return "", fmt.Errorf("%w: only absolute HTTPS URLs are allowed", ErrUnsafeSource)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: credentials in URL are forbidden", ErrUnsafeSource)
	}
	if !strings.EqualFold(u.Hostname(), AllowedHost) {
		return "", fmt.Errorf("%w: host %q is not allowlisted", ErrUnsafeSource, u.Hostname())
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", fmt.Errorf("%w: only port 443 is allowed", ErrUnsafeSource)
	}
	u.Scheme = "https"
	u.Host = AllowedHost
	u.Fragment = ""
	return u.String(), nil
}

func (a *Analyzer) fetch(ctx context.Context, sourceURL string) ([]byte, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("create document request: %w", err)
	}
	req.Header.Set("Accept", "application/pdf, application/vnd.openxmlformats-officedocument.wordprocessingml.document, application/zip, application/octet-stream")
	req.Header.Set("User-Agent", "SkillGap/1.0")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("download document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
		return nil, "", "", fmt.Errorf("download document: unexpected HTTP status %d", resp.StatusCode)
	}
	if resp.ContentLength > a.opts.MaxDownloadBytes {
		return nil, "", "", fmt.Errorf("%w: content length %d", ErrDownloadTooLarge, resp.ContentLength)
	}

	limited := io.LimitReader(resp.Body, a.opts.MaxDownloadBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", "", fmt.Errorf("read document: %w", err)
	}
	if int64(len(payload)) > a.opts.MaxDownloadBytes {
		return nil, "", "", fmt.Errorf("%w: limit is %d bytes", ErrDownloadTooLarge, a.opts.MaxDownloadBytes)
	}
	if len(payload) == 0 {
		return nil, "", "", fmt.Errorf("download document: empty response")
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	fileName := fileNameFromResponse(resp, sourceURL)
	return payload, strings.ToLower(mediaType), fileName, nil
}

func fileNameFromResponse(resp *http.Response, sourceURL string) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if candidate := safeDisplayName(params["filename"]); candidate != "" {
			return candidate
		}
	}
	u, err := url.Parse(sourceURL)
	if err != nil {
		return "document"
	}
	if candidate := safeDisplayName(path.Base(u.Path)); candidate != "" && candidate != "." && candidate != "/" {
		return candidate
	}
	return "document"
}

func safeDisplayName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = path.Base(name)
	if name == "." || name == "/" || name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	result := []rune(b.String())
	if len(result) > 240 {
		result = result[:240]
	}
	return string(result)
}
