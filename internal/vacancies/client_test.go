package vacancies

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientSearchBuildsRequestAndNormalizesSchema(t *testing.T) {
	t.Parallel()

	const query = "инженер-программист 1С & SQL"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/region/7700000000000" {
			t.Errorf("path = %q", r.URL.Path)
		}
		values := r.URL.Query()
		for key, want := range map[string]string{
			"text":           query,
			"experienceFrom": "0",
			"experienceTo":   "1",
			"limit":          "7",
			"offset":         "0",
		} {
			if got := values.Get(key); got != want {
				t.Errorf("query parameter %s = %q, want %q", key, got, want)
			}
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "SkillGap/1.0" {
			t.Errorf("User-Agent = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"status": 200,
			"meta": {"total": "1"},
			"results": {"vacancies": [{"vacancy": {
				"id": " vacancy-1 ",
				"job-name": "<b>Инженер</b>&nbsp; программист",
				"vac_url": " https://trudvsem.ru/vacancy/card/company/vacancy-1 ",
				"salary": "от 50000",
				"salary_min": "50000",
				"salary_max": 70000,
				"currency": "«руб.»",
				"requirements": "Python &amp; SQL",
				"duty": "Docker<br>Linux",
				"qualification": "Разработчик 1С",
				"employment": "Полная занятость",
				"schedule": "Полный день",
				"creation-date": "2026-09-25",
				"skills": [" SQL ", "sql", "Командная работа"],
				"company": {"name": " ООО &amp; Компания ", "companycode": "company-42"},
				"region": {"name": "Город Москва"},
				"requirement": {"education": "Среднее  профессиональное", "experience": "1"},
				"addresses": {"address": [{"location": "г. Москва<br/>ул. Тестовая"}]}
			}}]}
		}`)
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		BaseURL:           server.URL + "/",
		HTTPClient:        server.Client(),
		RequestTimeout:    2 * time.Second,
		RequestsPerSecond: 1000,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	items, err := client.search(context.Background(), "7700000000000", query, 7)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	got := items[0]
	if got.ID != "vacancy-1" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Title != "Инженер программист" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Employer != "ООО & Компания" {
		t.Errorf("Employer = %q", got.Employer)
	}
	if got.EmployerID != "company-42" {
		t.Errorf("EmployerID = %q", got.EmployerID)
	}
	if got.Region != "Город Москва" || got.Location != "г. Москва ул. Тестовая" {
		t.Errorf("region/location = %q / %q", got.Region, got.Location)
	}
	if got.Requirements != "Python & SQL" || got.Duties != "Docker Linux" {
		t.Errorf("requirements/duties = %q / %q", got.Requirements, got.Duties)
	}
	if got.Education != "Среднее профессиональное" {
		t.Errorf("Education = %q", got.Education)
	}
	if got.Currency != "RUB" {
		t.Errorf("Currency = %q", got.Currency)
	}
	if got.URL != "https://trudvsem.ru/vacancy/card/company/vacancy-1" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.FoundByQuery != query {
		t.Errorf("FoundByQuery = %q", got.FoundByQuery)
	}
	if got.ExperienceYears == nil || *got.ExperienceYears != 1 {
		t.Errorf("ExperienceYears = %v", got.ExperienceYears)
	}
	if got.SalaryFrom == nil || *got.SalaryFrom != 50000 {
		t.Errorf("SalaryFrom = %v", got.SalaryFrom)
	}
	if got.SalaryTo == nil || *got.SalaryTo != 70000 {
		t.Errorf("SalaryTo = %v", got.SalaryTo)
	}
	if want := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC); !got.PublishedAt.Equal(want) {
		t.Errorf("PublishedAt = %v, want %v", got.PublishedAt, want)
	}

	skills := make(map[string]string, len(got.Skills))
	for _, skill := range got.Skills {
		skills[skill.Name] = skill.Evidence
	}
	for name, evidence := range map[string]string{
		"SQL":              "структурированное поле вакансии",
		"Командная работа": "структурированное поле вакансии",
		"1С":               "из текста требований",
		"Python":           "из текста требований",
		"Docker":           "из текста требований",
		"Linux":            "из текста требований",
	} {
		if gotEvidence, ok := skills[name]; !ok || gotEvidence != evidence {
			t.Errorf("skill %q evidence = %q, present=%v", name, gotEvidence, ok)
		}
	}
	if len(skills) != 6 {
		t.Errorf("unique skills = %d, want 6: %#v", len(skills), skills)
	}
}

func TestClientSearchHandlesEmptyResults(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"200","meta":{"total":0},"results":{}}`)
	}))
	t.Cleanup(server.Close)
	client := mustTestClient(t, server, 1024)

	items, err := client.search(context.Background(), "77", "developer", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v, want empty", items)
	}
}

func TestClientSearchUsesBoundedPaginationAndReportsTruncation(t *testing.T) {
	t.Parallel()

	var offsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		offsets = append(offsets, offset)
		body := `{"status":200,"meta":{"total":3},"results":{"vacancies":[{"vacancy":{"id":"v` + offset + `","job-name":"Developer"}}]}}`
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, HTTPClient: server.Client(), RequestTimeout: time.Second,
		RequestsPerSecond: 1000, MaxPagesPerQuery: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := client.searchWithMeta(context.Background(), "77", "developer", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.vacancies) != 2 || result.total != 3 || !result.truncated {
		t.Fatalf("paged result = %+v", result)
	}
	if strings.Join(offsets, ",") != "0,1" {
		t.Fatalf("offsets = %#v, want [0 1]", offsets)
	}
}

func TestClientContinuesAfterShortPageWhenTotalShowsMoreResults(t *testing.T) {
	t.Parallel()

	var offsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		offsets = append(offsets, offset)
		if offset == "0" {
			_, _ = io.WriteString(w, `{"status":200,"meta":{"total":3},"results":{"vacancies":[{"vacancy":{"id":"v1","job-name":"Developer"}}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":200,"meta":{"total":3},"results":{"vacancies":[{"vacancy":{"id":"v2","job-name":"Developer"}},{"vacancy":{"id":"v3","job-name":"Developer"}}]}}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, HTTPClient: server.Client(), RequestTimeout: time.Second,
		RequestsPerSecond: 1000, MaxPagesPerQuery: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := client.searchWithMeta(context.Background(), "77", "developer", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.vacancies) != 3 || result.truncated {
		t.Fatalf("paged result = %+v", result)
	}
	if strings.Join(offsets, ",") != "0,1" {
		t.Fatalf("offsets = %#v, want [0 1]", offsets)
	}
}

func TestClientHonorsRetryAfterOnRateLimit(t *testing.T) {
	t.Parallel()

	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"status":200,"meta":{"total":0},"results":{}}`)
	}))
	t.Cleanup(server.Close)
	client := mustTestClient(t, server, 1024)
	if _, err := client.search(context.Background(), "77", "developer", 10); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
	if got := retryDelay("30", 1); got != 30*time.Second {
		t.Errorf("Retry-After = %v, want 30s", got)
	}
}

func TestClientDoesNotRetryBeforeRetryAfterDeadline(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, HTTPClient: server.Client(), RequestTimeout: 30 * time.Millisecond,
		RequestsPerSecond: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.search(context.Background(), "77", "developer", 10)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("search error = %v, want context deadline", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestClientSearchRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	client, err := NewClient(ClientConfig{BaseURL: "https://example.test/vacancies"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tests := []struct {
		name        string
		region      string
		query       string
		limit       int
		wantErrPart string
	}{
		{name: "missing region", query: "developer", limit: 1, wantErrPart: "region code is required"},
		{name: "missing query", region: "77", query: "  ", limit: 1, wantErrPart: "vacancy query is required"},
		{name: "zero limit", region: "77", query: "developer", limit: 0, wantErrPart: "limit must be between"},
		{name: "too large limit", region: "77", query: "developer", limit: 101, wantErrPart: "limit must be between"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.search(context.Background(), tt.region, tt.query, tt.limit)
			if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErrPart)
			}
		})
	}
}

func TestClientSearchResponseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		statusCode  int
		body        string
		maxBytes    int64
		wantErrPart string
	}{
		{name: "oversized", statusCode: http.StatusOK, body: strings.Repeat("x", 65), maxBytes: 64, wantErrPart: "response exceeds 64 bytes"},
		{name: "http status", statusCode: http.StatusServiceUnavailable, body: strings.Repeat("temporary ", 100), maxBytes: 4096, wantErrPart: "API status 503"},
		{name: "malformed json", statusCode: http.StatusOK, body: `{"status":`, maxBytes: 4096, wantErrPart: "decode vacancies response"},
		{name: "api status string", statusCode: http.StatusOK, body: `{"status":"500","results":{}}`, maxBytes: 4096, wantErrPart: "response status 500"},
		{name: "api status number", statusCode: http.StatusOK, body: `{"status":503,"results":{}}`, maxBytes: 4096, wantErrPart: "response status 503"},
		{name: "bad flexible integer", statusCode: http.StatusOK, body: `{"status":"200","meta":{"total":"many"},"results":{}}`, maxBytes: 4096, wantErrPart: "parse integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)
			client := mustTestClient(t, server, tt.maxBytes)

			_, err := client.search(context.Background(), "77", "developer", 1)
			if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErrPart)
			}
		})
	}
}

func TestFlexibleJSONValues(t *testing.T) {
	t.Parallel()

	intTests := []struct {
		name      string
		input     string
		want      int64
		wantValid bool
		wantErr   bool
	}{
		{name: "number", input: `42`, want: 42, wantValid: true},
		{name: "quoted number", input: `" 42 "`, want: 42, wantValid: true},
		{name: "negative", input: `-7`, want: -7, wantValid: true},
		{name: "null", input: `null`},
		{name: "empty string", input: `""`},
		{name: "bad string", input: `"4.2"`, wantErr: true},
		{name: "object", input: `{}`, wantErr: true},
	}
	for _, tt := range intTests {
		t.Run("int64/"+tt.name, func(t *testing.T) {
			var got flexibleInt64
			err := json.Unmarshal([]byte(tt.input), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tt.wantErr)
			}
			if err == nil && (got.Value != tt.want || got.Valid != tt.wantValid) {
				t.Fatalf("got = %+v, want value=%d valid=%v", got, tt.want, tt.wantValid)
			}
		})
	}

	for _, input := range []string{`"200"`, `200`} {
		var got flexibleString
		if err := json.Unmarshal([]byte(input), &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", input, err)
		}
		if got != "200" {
			t.Errorf("Unmarshal(%s) = %q, want 200", input, got)
		}
	}
}

func TestNormalizeVacancyOptionalNumbersDatesAndCurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          apiVacancy
		wantDate     time.Time
		wantCurrency string
	}{
		{
			name: "RFC3339 date and dollars",
			raw: apiVacancy{
				CreationDate: "2026-09-25T14:48:24+03:00",
				Currency:     "$",
			},
			wantDate:     time.Date(2026, 9, 25, 14, 48, 24, 0, time.FixedZone("+0300", 3*60*60)),
			wantCurrency: "USD",
		},
		{
			name: "compact timezone and euros",
			raw: apiVacancy{
				CreationDate: "2026-09-25T14:48:24+0300",
				Currency:     " EUR ",
			},
			wantDate:     time.Date(2026, 9, 25, 14, 48, 24, 0, time.FixedZone("+0300", 3*60*60)),
			wantCurrency: "EUR",
		},
		{
			name:         "invalid date and unknown currency",
			raw:          apiVacancy{CreationDate: "not-a-date", Currency: "cad"},
			wantCurrency: "CAD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeVacancy(tt.raw, "query")
			if !got.PublishedAt.Equal(tt.wantDate) {
				t.Errorf("PublishedAt = %v, want %v", got.PublishedAt, tt.wantDate)
			}
			if got.Currency != tt.wantCurrency {
				t.Errorf("Currency = %q, want %q", got.Currency, tt.wantCurrency)
			}
			if got.SalaryFrom != nil || got.SalaryTo != nil || got.ExperienceYears != nil {
				t.Errorf("optional numeric fields should be nil: %+v", got)
			}
		})
	}
}

func TestNewClientValidatesBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{name: "default"},
		{name: "https", baseURL: "https://example.test/api"},
		{name: "localhost http", baseURL: "http://localhost:8080/api"},
		{name: "loopback http", baseURL: "http://127.0.0.1:8080/api"},
		{name: "remote http", baseURL: "http://example.test/api", wantErr: true},
		{name: "missing scheme", baseURL: "example.test/api", wantErr: true},
		{name: "malformed", baseURL: "://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(ClientConfig{BaseURL: tt.baseURL})
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewClient() error = %v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestNewClientRejectsExcessivePageLimit(t *testing.T) {
	t.Parallel()
	if _, err := NewClient(ClientConfig{BaseURL: "https://example.test", MaxPagesPerQuery: 11}); err == nil {
		t.Fatal("NewClient accepted more than 10 pages per query")
	}
}

func mustTestClient(t *testing.T, server *httptest.Server, maxBytes int64) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		BaseURL:           server.URL,
		HTTPClient:        server.Client(),
		RequestTimeout:    2 * time.Second,
		MaxResponseBytes:  maxBytes,
		RequestsPerSecond: 1000,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}
