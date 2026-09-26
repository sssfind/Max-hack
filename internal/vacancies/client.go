package vacancies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultRequestTimeout  = 25 * time.Second
	defaultMaxResponseSize = int64(8 << 20)
	defaultRequestsPerSec  = 3.0
	defaultMaxPages        = 2
)

var htmlTagPattern = regexp.MustCompile(`(?s)<[^>]*>`)

type ClientConfig struct {
	BaseURL           string
	HTTPClient        *http.Client
	RequestTimeout    time.Duration
	MaxResponseBytes  int64
	RequestsPerSecond float64
	MaxPagesPerQuery  int
}

type Client struct {
	baseURL          string
	httpClient       *http.Client
	requestTimeout   time.Duration
	maxResponseBytes int64
	limiter          *rate.Limiter
	maxPages         int
}

func NewClient(cfg ClientConfig) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid vacancies base URL %q", baseURL)
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
		return nil, fmt.Errorf("vacancies base URL must use https")
	}

	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	maxBytes := cfg.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxResponseSize
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	requestsPerSecond := cfg.RequestsPerSecond
	if requestsPerSecond <= 0 {
		requestsPerSecond = defaultRequestsPerSec
	}
	maxPages := cfg.MaxPagesPerQuery
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}
	if maxPages > 10 {
		return nil, fmt.Errorf("max pages per query must not exceed 10")
	}
	return &Client{
		baseURL:          baseURL,
		httpClient:       httpClient,
		requestTimeout:   timeout,
		maxResponseBytes: maxBytes,
		limiter:          rate.NewLimiter(rate.Limit(requestsPerSecond), 3),
		maxPages:         maxPages,
	}, nil
}

type apiEnvelope struct {
	Status  flexibleString `json:"status"`
	Meta    apiMeta        `json:"meta"`
	Results apiResults     `json:"results"`
}

type apiMeta struct {
	Total flexibleInt64 `json:"total"`
}

type apiResults struct {
	Vacancies []apiVacancyWrapper `json:"vacancies"`
}

type apiVacancyWrapper struct {
	Vacancy apiVacancy `json:"vacancy"`
}

type apiVacancy struct {
	ID            string        `json:"id"`
	Title         string        `json:"job-name"`
	URL           string        `json:"vac_url"`
	SalaryText    string        `json:"salary"`
	SalaryMin     flexibleInt64 `json:"salary_min"`
	SalaryMax     flexibleInt64 `json:"salary_max"`
	Currency      string        `json:"currency"`
	Requirements  string        `json:"requirements"`
	Duties        string        `json:"duty"`
	Qualification string        `json:"qualification"`
	Employment    string        `json:"employment"`
	Schedule      string        `json:"schedule"`
	CreationDate  string        `json:"creation-date"`
	Skills        []string      `json:"skills"`
	Company       struct {
		Name        string `json:"name"`
		CompanyCode string `json:"companycode"`
	} `json:"company"`
	Region struct {
		Name string `json:"name"`
	} `json:"region"`
	Requirement struct {
		Education  string      `json:"education"`
		Experience flexibleInt `json:"experience"`
	} `json:"requirement"`
	Addresses struct {
		Address []struct {
			Location string `json:"location"`
		} `json:"address"`
	} `json:"addresses"`
}

type flexibleInt struct {
	Value int
	Valid bool
}

func (v *flexibleInt) UnmarshalJSON(data []byte) error {
	n, valid, err := parseFlexibleInt64(data)
	if err != nil {
		return err
	}
	if n > int64(^uint(0)>>1) {
		return fmt.Errorf("integer overflows int: %d", n)
	}
	v.Value, v.Valid = int(n), valid
	return nil
}

type flexibleInt64 struct {
	Value int64
	Valid bool
}

func (v *flexibleInt64) UnmarshalJSON(data []byte) error {
	n, valid, err := parseFlexibleInt64(data)
	if err != nil {
		return err
	}
	v.Value, v.Valid = n, valid
	return nil
}

func parseFlexibleInt64(data []byte) (int64, bool, error) {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" || raw == `""` {
		return 0, false, nil
	}
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return 0, false, err
		}
		raw = strings.TrimSpace(s)
		if raw == "" {
			return 0, false, nil
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse integer %q: %w", raw, err)
	}
	return n, true, nil
}

type flexibleString string

func (s *flexibleString) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*s = flexibleString(value)
		return nil
	}
	*s = flexibleString(strings.TrimSpace(string(data)))
	return nil
}

func (c *Client) search(ctx context.Context, regionCode, query string, limit int) ([]Vacancy, error) {
	result, err := c.searchWithMeta(ctx, regionCode, query, limit)
	return result.vacancies, err
}

type clientSearchResult struct {
	vacancies []Vacancy
	total     int64
	truncated bool
}

func (c *Client) searchWithMeta(ctx context.Context, regionCode, query string, limit int) (clientSearchResult, error) {
	if strings.TrimSpace(regionCode) == "" {
		return clientSearchResult{}, errors.New("region code is required")
	}
	if strings.TrimSpace(query) == "" {
		return clientSearchResult{}, errors.New("vacancy query is required")
	}
	if limit < 1 || limit > 100 {
		return clientSearchResult{}, fmt.Errorf("limit must be between 1 and 100")
	}

	result := clientSearchResult{}
	for page := 0; page < c.maxPages; page++ {
		envelope, err := c.fetchPage(ctx, regionCode, query, limit, page)
		if err != nil {
			return clientSearchResult{}, err
		}
		if envelope.Meta.Total.Valid {
			result.total = envelope.Meta.Total.Value
		}
		for _, item := range envelope.Results.Vacancies {
			result.vacancies = append(result.vacancies, normalizeVacancy(item.Vacancy, query))
		}
		pageSize := len(envelope.Results.Vacancies)
		if result.total > 0 && int64(len(result.vacancies)) >= result.total {
			return result, nil
		}
		if result.total == 0 && pageSize < limit {
			return result, nil
		}
		if pageSize == 0 {
			result.truncated = result.total > int64(len(result.vacancies))
			return result, nil
		}
	}
	result.truncated = result.total == 0 || int64(len(result.vacancies)) < result.total
	return result, nil
}

func (c *Client) fetchPage(ctx context.Context, regionCode, query string, limit, offset int) (apiEnvelope, error) {
	endpoint, err := url.Parse(c.baseURL + "/region/" + url.PathEscape(regionCode))
	if err != nil {
		return apiEnvelope{}, fmt.Errorf("build vacancies URL: %w", err)
	}
	params := endpoint.Query()
	params.Set("text", query)
	params.Set("experienceFrom", "0")
	params.Set("experienceTo", "1")
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", strconv.Itoa(offset))
	endpoint.RawQuery = params.Encode()

	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	var (
		body       []byte
		statusCode int
	)
	for attempt := 1; attempt <= 2; attempt++ {
		if err := c.limiter.Wait(requestCtx); err != nil {
			return apiEnvelope{}, fmt.Errorf("vacancies rate limiter: %w", err)
		}
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return apiEnvelope{}, fmt.Errorf("create vacancies request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "SkillGap/1.0")

		resp, requestErr := c.httpClient.Do(req)
		if requestErr != nil {
			if attempt == 1 && requestCtx.Err() == nil {
				if err := waitForRetry(requestCtx, 250*time.Millisecond); err != nil {
					return apiEnvelope{}, fmt.Errorf("request vacancies: %w", err)
				}
				continue
			}
			return apiEnvelope{}, fmt.Errorf("request vacancies: %w", requestErr)
		}

		statusCode = resp.StatusCode
		retryAfter := resp.Header.Get("Retry-After")
		limited := io.LimitReader(resp.Body, c.maxResponseBytes+1)
		body, err = io.ReadAll(limited)
		_ = resp.Body.Close()
		if err != nil {
			return apiEnvelope{}, fmt.Errorf("read vacancies response: %w", err)
		}
		if int64(len(body)) > c.maxResponseBytes {
			return apiEnvelope{}, fmt.Errorf("vacancies response exceeds %d bytes", c.maxResponseBytes)
		}
		if attempt == 1 && (statusCode == http.StatusTooManyRequests || statusCode >= 500) {
			delay := retryDelay(retryAfter, attempt)
			if err := waitForRetry(requestCtx, delay); err != nil {
				return apiEnvelope{}, fmt.Errorf("request vacancies: %w", err)
			}
			continue
		}
		break
	}
	if statusCode != http.StatusOK {
		return apiEnvelope{}, fmt.Errorf("vacancies API status %d: %s", statusCode, truncateErrorBody(body))
	}

	var envelope apiEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return apiEnvelope{}, fmt.Errorf("decode vacancies response: %w", err)
	}
	if status := string(envelope.Status); status != "" && status != "200" {
		return apiEnvelope{}, fmt.Errorf("vacancies API response status %s", status)
	}
	return envelope, nil
}

func retryDelay(value string, attempt int) time.Duration {
	fallback := time.Duration(attempt) * 250 * time.Millisecond
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		const maxDurationSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if seconds > maxDurationSeconds {
			return time.Duration(^uint64(0) >> 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			return 0
		}
		return delay
	}
	return fallback
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func normalizeVacancy(raw apiVacancy, query string) Vacancy {
	v := Vacancy{
		ID:           strings.TrimSpace(raw.ID),
		Title:        cleanText(raw.Title),
		Employer:     cleanText(raw.Company.Name),
		EmployerID:   strings.TrimSpace(raw.Company.CompanyCode),
		Region:       cleanText(raw.Region.Name),
		Duties:       cleanText(raw.Duties),
		Requirements: cleanText(raw.Requirements),
		Education:    cleanText(raw.Requirement.Education),
		Employment:   cleanText(raw.Employment),
		Schedule:     cleanText(raw.Schedule),
		Currency:     normalizeCurrency(raw.Currency),
		URL:          normalizeVacancyURL(raw.URL),
		FoundByQuery: strings.TrimSpace(query),
	}
	if len(raw.Addresses.Address) > 0 {
		v.Location = cleanText(raw.Addresses.Address[0].Location)
	}
	if raw.Requirement.Experience.Valid {
		experience := raw.Requirement.Experience.Value
		v.ExperienceYears = &experience
	}
	if raw.SalaryMin.Valid && raw.SalaryMin.Value > 0 {
		value := raw.SalaryMin.Value
		v.SalaryFrom = &value
	}
	if raw.SalaryMax.Valid && raw.SalaryMax.Value > 0 {
		value := raw.SalaryMax.Value
		v.SalaryTo = &value
	}
	v.PublishedAt = parseAPITime(raw.CreationDate)
	v.Skills = collectSkills(raw.Skills, strings.Join([]string{raw.Qualification, raw.Requirements, raw.Duties}, " "))
	return v
}

func normalizeVacancyURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "trudvsem.ru" && !strings.HasSuffix(host, ".trudvsem.ru") {
		return ""
	}
	return parsed.String()
}

func cleanText(value string) string {
	value = strings.ReplaceAll(value, "<br>", "\n")
	value = strings.ReplaceAll(value, "<br/>", "\n")
	value = strings.ReplaceAll(value, "<br />", "\n")
	value = html.UnescapeString(htmlTagPattern.ReplaceAllString(value, " "))
	return strings.Join(strings.Fields(value), " ")
}

func parseAPITime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05-0700"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func normalizeCurrency(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("«", "", "»", "", ".", "", " ", "").Replace(value)
	switch value {
	case "руб", "рубль", "рубли", "рублей", "rub", "rur", "₽":
		return "RUB"
	case "usd", "$":
		return "USD"
	case "eur", "€":
		return "EUR"
	default:
		return strings.ToUpper(value)
	}
}

func truncateErrorBody(body []byte) string {
	const max = 512
	text := strings.TrimSpace(string(body))
	if len(text) > max {
		return text[:max] + "..."
	}
	return text
}
