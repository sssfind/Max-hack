package vacancies

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceAnalyzeDeduplicatesAndFiltersMarketResults(t *testing.T) {
	t.Parallel()

	zero, one, two := 0, 1, 2
	var (
		mu   sync.Mutex
		hits = make(map[string]int)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("text")
		mu.Lock()
		hits[query]++
		mu.Unlock()

		switch query {
		case "golang":
			writeTestVacancies(t, w,
				testAPIVacancy{ID: "duplicate", Title: "Junior backend Golang разработчик", Employer: "A", Experience: &zero, Skills: []string{"Go"}, Published: "2026-09-20"},
				testAPIVacancy{ID: "senior", Title: "Senior Golang developer", Employer: "B", Experience: &zero, Published: "2026-09-25"},
				testAPIVacancy{ID: "experienced", Title: "Golang developer", Employer: "C", Experience: &two, Published: "2026-09-24"},
				testAPIVacancy{ID: "irrelevant", Title: "Бухгалтер", Employer: "D", Experience: &zero, Requirements: "Ведение учета", Published: "2026-09-23"},
			)
		case "backend":
			writeTestVacancies(t, w,
				testAPIVacancy{ID: "duplicate", Title: "Junior backend Golang разработчик", Employer: "A", Experience: &zero, Skills: []string{"SQL"}, Published: "2026-09-20"},
				testAPIVacancy{ID: "fresh", Title: "Backend разработчик", Employer: "E", Experience: &one, Skills: []string{"Docker"}, Published: "2026-09-26"},
				testAPIVacancy{ID: "lead", Title: "Team Lead backend", Employer: "F", Experience: &zero, Published: "2026-09-26"},
			)
		default:
			t.Errorf("unexpected query %q", query)
			writeTestVacancies(t, w)
		}
	}))
	t.Cleanup(server.Close)

	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{
		MaxQueries:       5,
		QueryConcurrency: 2,
		PerQueryLimit:    20,
		MaxVacancies:     20,
		Now: func() time.Time {
			return time.Date(2026, 9, 26, 15, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
		},
	})

	snapshot, stats, err := service.Analyze(context.Background(), SearchRequest{
		RegionCode: "7700000000000",
		Queries:    []string{"golang", " GOLANG ", "backend"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got, want := len(snapshot.Vacancies), 2; got != want {
		t.Fatalf("vacancies = %d, want %d: %#v", got, want, snapshot.Vacancies)
	}
	if snapshot.Vacancies[0].ID != "fresh" || snapshot.Vacancies[1].ID != "duplicate" {
		t.Errorf("vacancies not sorted newest first: %#v", snapshot.Vacancies)
	}
	duplicate := snapshot.Vacancies[1]
	skillNames := make(map[string]bool)
	for _, skill := range duplicate.Skills {
		skillNames[skill.Name] = true
	}
	if !skillNames["Go"] || !skillNames["SQL"] || len(skillNames) != 2 {
		t.Errorf("deduplicated skills = %#v, want Go and SQL", duplicate.Skills)
	}
	if stats.VacancyCount != 2 || stats.EntryLevelCount != 2 {
		t.Errorf("statistics = %+v", stats)
	}
	if snapshot.Mode != ModeTest || snapshot.Source == officialSourceName || !strings.Contains(snapshot.Source, "тестовый") {
		t.Errorf("snapshot metadata = %+v", snapshot)
	}
	if want := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC); !snapshot.FetchedAt.Equal(want) {
		t.Errorf("FetchedAt = %v, want %v", snapshot.FetchedAt, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if hits["golang"] != 1 || hits["backend"] != 1 || len(hits) != 2 {
		t.Errorf("query hits = %#v; duplicate queries must be removed", hits)
	}
}

func TestServiceAnalyzeReturnsPartialResultsAndWarnings(t *testing.T) {
	t.Parallel()

	zero := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("text") {
		case "developer":
			writeTestVacancies(t, w, testAPIVacancy{
				ID:         "ok",
				Title:      "Junior developer",
				Employer:   "Employer",
				Experience: &zero,
				Published:  "2026-09-26",
			})
		case "broken":
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		default:
			t.Errorf("unexpected query %q", r.URL.Query().Get("text"))
		}
	}))
	t.Cleanup(server.Close)

	now := time.Date(2026, 9, 26, 15, 30, 0, 0, time.UTC)
	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{
		QueryConcurrency: 2,
		Now:              func() time.Time { return now },
	})
	snapshot, stats, err := service.Analyze(context.Background(), SearchRequest{
		RegionCode:            "77",
		Queries:               []string{"developer", "broken"},
		QueryModelNeedsReview: true,
	})
	if err != nil {
		t.Fatalf("Analyze returned error for partial success: %v", err)
	}
	if len(snapshot.Vacancies) != 1 || snapshot.Vacancies[0].ID != "ok" {
		t.Errorf("Vacancies = %#v", snapshot.Vacancies)
	}
	if stats.VacancyCount != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if !snapshot.Partial {
		t.Error("partial upstream result was not marked partial")
	}
	if !snapshot.FetchedAt.Equal(now) {
		t.Errorf("FetchedAt = %v, want %v", snapshot.FetchedAt, now)
	}
	assertWarningContains(t, snapshot.Warnings, "требует ручной проверки")
	assertWarningContains(t, snapshot.Warnings, "Запрос «broken» временно не обработан")
}

func TestServiceMergeOrderFollowsQueryOrderNotResponseTiming(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("text") {
		case "developer first":
			time.Sleep(60 * time.Millisecond)
			writeTestVacancies(t, w, testAPIVacancy{ID: "same", Title: "Developer first", Skills: []string{"Go"}})
		case "developer second":
			writeTestVacancies(t, w, testAPIVacancy{ID: "same", Title: "Developer second", Skills: []string{"SQL"}})
		default:
			t.Errorf("unexpected query %q", r.URL.Query().Get("text"))
		}
	}))
	t.Cleanup(server.Close)

	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{QueryConcurrency: 2})
	snapshot, _, err := service.Analyze(context.Background(), SearchRequest{
		RegionCode: "77",
		Queries:    []string{"developer first", "developer second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Vacancies) != 1 || snapshot.Vacancies[0].Title != "Developer first" {
		t.Fatalf("merge did not preserve query priority: %#v", snapshot.Vacancies)
	}
	if len(snapshot.Vacancies[0].Skills) != 2 {
		t.Fatalf("duplicate skills were not merged: %#v", snapshot.Vacancies[0].Skills)
	}
}

func TestServiceAnalyzeFailsWhenAllQueriesFail(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("text") {
		case "broken-http":
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case "broken-json":
			_, _ = io.WriteString(w, `{`)
		default:
			t.Errorf("unexpected query %q", r.URL.Query().Get("text"))
		}
	}))
	t.Cleanup(server.Close)

	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{QueryConcurrency: 2})
	snapshot, stats, err := service.Analyze(context.Background(), SearchRequest{
		RegionCode: "77",
		Queries:    []string{"broken-http", "broken-json"},
	})
	if err == nil {
		t.Fatal("Analyze error = nil")
	}
	for _, part := range []string{"all vacancy requests failed", "broken-http", "broken-json"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not contain %q", err, part)
		}
	}
	if len(snapshot.Warnings) != 2 {
		t.Errorf("warnings = %#v, want one per failed query", snapshot.Warnings)
	}
	if len(snapshot.Vacancies) != 0 || stats.VacancyCount != 0 {
		t.Errorf("snapshot/stats must not contain results: %+v / %+v", snapshot, stats)
	}
}

func TestServiceDoesNotReportEmptyMarketWhenOtherQueriesFailed(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("text") {
		case "empty":
			writeTestVacancies(t, w)
		case "broken":
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		default:
			t.Errorf("unexpected query %q", r.URL.Query().Get("text"))
		}
	}))
	t.Cleanup(server.Close)

	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{QueryConcurrency: 2})
	snapshot, _, err := service.Analyze(context.Background(), SearchRequest{
		RegionCode: "77",
		Queries:    []string{"empty", "broken"},
	})
	if err == nil || !strings.Contains(err.Error(), "search is incomplete") {
		t.Fatalf("Analyze error = %v, want incomplete-search error", err)
	}
	assertWarningContains(t, snapshot.Warnings, "Запрос «broken» временно не обработан")
}

func TestServiceAnalyzeValidation(t *testing.T) {
	t.Parallel()

	configuredClient, err := NewClient(ClientConfig{BaseURL: "https://example.test/vacancies"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tests := []struct {
		name        string
		service     *Service
		request     SearchRequest
		wantErrPart string
	}{
		{name: "nil receiver", service: nil, request: SearchRequest{RegionCode: "77", Queries: []string{"x"}}, wantErrPart: "not configured"},
		{name: "nil client", service: NewService(nil, ServiceOptions{}), request: SearchRequest{RegionCode: "77", Queries: []string{"x"}}, wantErrPart: "not configured"},
		{name: "missing region", service: NewService(configuredClient, ServiceOptions{}), request: SearchRequest{Queries: []string{"developer"}}, wantErrPart: "region code is required"},
		{name: "empty queries", service: NewService(configuredClient, ServiceOptions{}), request: SearchRequest{RegionCode: "77", Queries: []string{"", "  "}}, wantErrPart: "queries are empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.service.Analyze(context.Background(), tt.request)
			if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErrPart)
			}
		})
	}
}

func TestServiceSortsAndTruncatesByRelevanceThenFreshness(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestVacancies(t, w,
			testAPIVacancy{ID: "old", Title: "Developer old", Published: "2026-09-20"},
			testAPIVacancy{ID: "new-b", Title: "Developer B", Published: "2026-09-26"},
			testAPIVacancy{ID: "new-a", Title: "Developer A", Published: "2026-09-26"},
		)
	}))
	t.Cleanup(server.Close)

	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{MaxVacancies: 2})
	snapshot, _, err := service.Analyze(context.Background(), SearchRequest{RegionCode: "77", Queries: []string{"developer"}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(snapshot.Vacancies) != 2 || snapshot.Vacancies[0].ID != "new-a" || snapshot.Vacancies[1].ID != "new-b" {
		t.Errorf("sorted/truncated vacancies = %#v", snapshot.Vacancies)
	}
	assertWarningContains(t, snapshot.Warnings, "2 наиболее релевантных вакансий из полученной выборки")
}

func TestEntryLevelAndRelevanceRules(t *testing.T) {
	t.Parallel()

	zero, one, two := 0, 1, 2
	entryTests := []struct {
		name string
		item Vacancy
		want bool
	}{
		{name: "missing experience", item: Vacancy{Title: "Developer"}, want: true},
		{name: "zero experience", item: Vacancy{Title: "Developer", ExperienceYears: &zero}, want: true},
		{name: "one year", item: Vacancy{Title: "Developer", ExperienceYears: &one}, want: true},
		{name: "two years", item: Vacancy{Title: "Developer", ExperienceYears: &two}},
		{name: "senior", item: Vacancy{Title: "Senior developer", ExperienceYears: &zero}},
		{name: "team lead", item: Vacancy{Title: "Team Lead developer", ExperienceYears: &zero}},
		{name: "russian lead", item: Vacancy{Title: "Ведущий программист", ExperienceYears: &zero}},
		{name: "manager", item: Vacancy{Title: "Руководитель отдела", ExperienceYears: &zero}},
		{name: "architect", item: Vacancy{Title: "Архитектор ПО", ExperienceYears: &zero}},
	}
	for _, tt := range entryTests {
		t.Run("entry/"+tt.name, func(t *testing.T) {
			if got := isEntryLevel(tt.item); got != tt.want {
				t.Errorf("isEntryLevel(%+v) = %v, want %v", tt.item, got, tt.want)
			}
		})
	}

	relevanceTests := []struct {
		name  string
		item  Vacancy
		query string
		want  bool
	}{
		{name: "title", item: Vacancy{Title: "Golang developer"}, query: "golang", want: true},
		{name: "requirements only is weak evidence", item: Vacancy{Requirements: "Нужен Python"}, query: "python"},
		{name: "duties only is weak evidence", item: Vacancy{Duties: "Работа с PostgreSQL"}, query: "postgresql"},
		{name: "skill", item: Vacancy{Skills: []Skill{{Name: "Docker"}}}, query: "docker", want: true},
		{name: "one meaningful term", item: Vacancy{Title: "Backend developer"}, query: "golang backend", want: true},
		{name: "generic role word is insufficient", item: Vacancy{Title: "Специалист по продажам"}, query: "специалист по информационным системам", want: false},
		{name: "irrelevant", item: Vacancy{Title: "Бухгалтер"}, query: "golang backend"},
		{name: "only stop words", item: Vacancy{Title: "Любая"}, query: "работа для без опыта junior"},
	}
	for _, tt := range relevanceTests {
		t.Run("relevance/"+tt.name, func(t *testing.T) {
			if got := isRelevant(tt.item, tt.query); got != tt.want {
				t.Errorf("isRelevant(%+v, %q) = %v, want %v", tt.item, tt.query, got, tt.want)
			}
		})
	}
}

func TestServiceFiltersHigherEducationAndReportsSamplingStages(t *testing.T) {
	t.Parallel()

	zero := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestVacancies(t, w,
			testAPIVacancy{ID: "higher", Title: "Программист", Education: "Высшее образование — бакалавриат", Experience: &zero},
			testAPIVacancy{ID: "spo", Title: "Программист", Education: "Среднее профессиональное образование", Experience: &zero},
		)
	}))
	t.Cleanup(server.Close)

	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{DisableCache: true})
	snapshot, stats, err := service.Analyze(context.Background(), SearchRequest{RegionCode: "77", Queries: []string{"программист"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Vacancies) != 1 || snapshot.Vacancies[0].ID != "spo" || stats.VacancyCount != 1 {
		t.Fatalf("education filter result = %#v, stats=%+v", snapshot.Vacancies, stats)
	}
	want := SamplingStages{SourceTotal: 2, Retrieved: 2, EntryLevelPassed: 2, EducationPassed: 1, RelevantPassed: 1, RejectedHigherEducation: 1, Deduplicated: 1, Included: 1}
	if snapshot.Sampling != want {
		t.Errorf("sampling = %+v, want %+v", snapshot.Sampling, want)
	}
	assertWarningContains(t, snapshot.Warnings, "обязательным высшим образованием: 1")
}

func TestEducationClassifierAllowsSPOAlternative(t *testing.T) {
	t.Parallel()

	if got := classifyEducation(Vacancy{Education: "Высшее или среднее профессиональное образование"}); got != EducationHigherOrVocational {
		t.Errorf("mixed education = %q", got)
	}
	if got := classifyEducation(Vacancy{Requirements: "Требуется высшее техническое образование"}); got != EducationHigherOnly {
		t.Errorf("higher-only education = %q", got)
	}
	if got := classifyEducation(Vacancy{Education: "Образование не требуется"}); got != EducationNotRequired {
		t.Errorf("not-required education = %q", got)
	}
}

func TestServiceCacheFreshHitAndStaleFallback(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) > 1 {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		writeTestVacancies(t, w, testAPIVacancy{ID: "one", Title: "Developer"})
	}))
	t.Cleanup(server.Close)

	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{
		CacheTTL:      time.Minute,
		StaleCacheTTL: time.Hour,
		Now:           func() time.Time { return now },
	})
	req := SearchRequest{RegionCode: "77", ProgramCode: "09.02.07", Queries: []string{"developer"}}
	first, _, err := service.Analyze(context.Background(), req)
	if err != nil || first.Mode != ModeTest {
		t.Fatalf("first analyze = mode %q, err %v", first.Mode, err)
	}
	fresh, _, err := service.Analyze(context.Background(), req)
	if err != nil || fresh.Mode != ModeTest || hits.Load() != 1 {
		t.Fatalf("fresh cache = mode %q, hits %d, err %v", fresh.Mode, hits.Load(), err)
	}
	now = now.Add(2 * time.Minute)
	stale, _, err := service.Analyze(context.Background(), req)
	if err != nil || stale.Mode != ModeTest || hits.Load() != 3 { // one request plus one retry
		t.Fatalf("stale fallback = mode %q, hits %d, err %v", stale.Mode, hits.Load(), err)
	}
	assertWarningContains(t, stale.Warnings, "Настраиваемый тестовый источник временно недоступен")
}

func TestServiceCacheHonorsByteBudget(t *testing.T) {
	t.Parallel()

	service := NewService(&Client{}, ServiceOptions{
		CacheMaxEntries: 10,
		CacheMaxBytes:   3 << 10,
		StaleCacheTTL:   time.Hour,
	})
	now := time.Now().UTC()
	small := Snapshot{Source: officialSourceName, Mode: ModeLive, Vacancies: []Vacancy{{ID: "1", Title: "Developer"}}}
	service.storeCache("one", small, Statistics{}, now)
	service.storeCache("two", small, Statistics{}, now.Add(time.Second))

	service.cacheMu.RLock()
	if len(service.cache) != 1 {
		t.Errorf("cache entries = %d, want oldest entry evicted", len(service.cache))
	}
	if _, ok := service.cache["two"]; !ok {
		t.Error("newest entry was not retained")
	}
	if service.cacheBytes > service.opts.CacheMaxBytes {
		t.Errorf("cache bytes = %d, budget = %d", service.cacheBytes, service.opts.CacheMaxBytes)
	}
	service.cacheMu.RUnlock()

	oversized := small
	oversized.Vacancies[0].Requirements = strings.Repeat("x", 8<<10)
	service.storeCache("oversized", oversized, Statistics{}, now.Add(2*time.Second))
	service.cacheMu.RLock()
	_, cached := service.cache["oversized"]
	service.cacheMu.RUnlock()
	if cached {
		t.Error("single entry larger than byte budget was cached")
	}
}

func TestCachedOfficialSnapshotUsesCacheMode(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	service := NewService(&Client{}, ServiceOptions{CacheTTL: time.Minute})
	service.storeCache("official", Snapshot{Source: officialSourceName, Mode: ModeLive}, Statistics{}, now)
	got, _, ok := service.cached("official", now, time.Minute, false)
	if !ok || got.Mode != ModeCache {
		t.Fatalf("official cache mode = %q, ok=%v", got.Mode, ok)
	}
}

func TestSourceTotalSaturatesInsteadOfOverflowing(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	if got := saturatingSourceTotal(maxInt-1, 10); got != maxInt {
		t.Errorf("saturated total = %d, want %d", got, maxInt)
	}
	if got := saturatingSourceTotal(5, -1); got != 5 {
		t.Errorf("negative source total changed count to %d", got)
	}
}

func TestServiceRanksTitleMatchAboveSkillOnlyMatch(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestVacancies(t, w,
			testAPIVacancy{ID: "skill", Title: "Оператор", Skills: []string{"Docker"}, Published: "2026-09-27"},
			testAPIVacancy{ID: "title", Title: "Docker инженер", Published: "2026-09-20"},
		)
	}))
	t.Cleanup(server.Close)

	service := NewService(mustTestClient(t, server, 1<<20), ServiceOptions{DisableCache: true})
	snapshot, _, err := service.Analyze(context.Background(), SearchRequest{RegionCode: "77", Queries: []string{"docker"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Vacancies) != 2 || snapshot.Vacancies[0].ID != "title" || snapshot.Vacancies[0].RelevanceScore <= snapshot.Vacancies[1].RelevanceScore {
		t.Fatalf("ranking = %#v", snapshot.Vacancies)
	}
}

func TestDeduplicateUsesIDURLAndFallbackAndMergesSkills(t *testing.T) {
	t.Parallel()

	items := []Vacancy{
		{ID: "same-id", Title: "First", Skills: []Skill{{Name: "Go", Evidence: "first"}}},
		{ID: "same-id", Title: "Ignored duplicate title", Skills: []Skill{{Name: "go", Evidence: "duplicate"}, {Name: "SQL", Evidence: "second"}}},
		{URL: " HTTPS://EXAMPLE.TEST/V/1 ", Title: "URL first"},
		{URL: "https://example.test/v/1", Title: "URL duplicate"},
		{Title: "No key", Employer: "Employer", Location: "Moscow"},
		{Title: "No key", Employer: "Employer", Location: "Moscow"},
	}
	got := deduplicate(items)
	if len(got) != 3 {
		t.Fatalf("deduplicate() returned %d items: %#v", len(got), got)
	}
	if got[0].Title != "First" {
		t.Errorf("first duplicate was not preserved: %+v", got[0])
	}
	if len(got[0].Skills) != 2 || got[0].Skills[0].Name != "Go" || got[0].Skills[1].Name != "SQL" {
		t.Errorf("merged skills = %#v", got[0].Skills)
	}
}

func TestDeduplicateKeepsSalaryRecordCurrencyConsistent(t *testing.T) {
	t.Parallel()

	rubFrom, rubTo := int64(80_000), int64(120_000)
	usdFrom, usdTo := int64(1_000), int64(2_000)
	got := deduplicate([]Vacancy{
		{ID: "same", SalaryFrom: &rubFrom, SalaryTo: &rubTo, Currency: "RUB"},
		{ID: "same", SalaryFrom: &usdFrom, SalaryTo: &usdTo, Currency: "USD"},
	})
	if len(got) != 1 {
		t.Fatalf("deduplicate() returned %d items", len(got))
	}
	if got[0].Currency != "RUB" || got[0].SalaryFrom == nil || *got[0].SalaryFrom != rubFrom || got[0].SalaryTo == nil || *got[0].SalaryTo != rubTo {
		t.Fatalf("merged salary mixed source records: %+v", got[0])
	}
}

func TestDeduplicateDoesNotInventRangeFromPartialSalaryRecords(t *testing.T) {
	t.Parallel()

	from, to := int64(80_000), int64(120_000)
	got := deduplicate([]Vacancy{
		{ID: "same", SalaryFrom: &from, Currency: "RUB"},
		{ID: "same", SalaryTo: &to, Currency: "RUB"},
	})
	if len(got) != 1 {
		t.Fatalf("deduplicate() returned %d items", len(got))
	}
	if got[0].SalaryFrom == nil || *got[0].SalaryFrom != from || got[0].SalaryTo != nil {
		t.Fatalf("merged salary invented a range: %+v", got[0])
	}
}

func TestDeduplicateKeepsSalaryTextWithSelectedRecord(t *testing.T) {
	t.Parallel()

	from := int64(80_000)
	got := deduplicate([]Vacancy{
		{ID: "same", SalaryText: "по договорённости"},
		{ID: "same", SalaryFrom: &from, Currency: "RUB", SalaryText: "от 80 000 рублей"},
	})
	if len(got) != 1 || got[0].SalaryFrom == nil || got[0].SalaryText != "от 80 000 рублей" || got[0].Currency != "RUB" {
		t.Fatalf("merged salary record = %+v", got)
	}
}

func TestUniqueQueriesTrimsDeduplicatesAndLimits(t *testing.T) {
	t.Parallel()

	got := uniqueQueries([]string{" Go ", "go", "", "SQL", "sql ", "Python", "Docker"}, 3)
	want := []string{"Go", "SQL", "Python"}
	if len(got) != len(want) {
		t.Fatalf("uniqueQueries = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("uniqueQueries[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

type testAPIVacancy struct {
	ID           string
	Title        string
	Employer     string
	Requirements string
	Duties       string
	Education    string
	Experience   *int
	Skills       []string
	Published    string
}

func writeTestVacancies(t *testing.T, w http.ResponseWriter, items ...testAPIVacancy) {
	t.Helper()
	wrapped := make([]map[string]any, 0, len(items))
	for _, item := range items {
		requirement := make(map[string]any)
		if item.Experience != nil {
			requirement["experience"] = *item.Experience
		}
		if item.Education != "" {
			requirement["education"] = item.Education
		}
		wrapped = append(wrapped, map[string]any{
			"vacancy": map[string]any{
				"id":            item.ID,
				"job-name":      item.Title,
				"company":       map[string]any{"name": item.Employer},
				"requirements":  item.Requirements,
				"duty":          item.Duties,
				"requirement":   requirement,
				"skills":        item.Skills,
				"creation-date": item.Published,
			},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"status": "200",
		"meta":   map[string]any{"total": len(items)},
		"results": map[string]any{
			"vacancies": wrapped,
		},
	}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func assertWarningContains(t *testing.T, warnings []string, substring string) {
	t.Helper()
	for _, warning := range warnings {
		if strings.Contains(warning, substring) {
			return
		}
	}
	t.Errorf("warnings %#v do not contain %q", warnings, substring)
}
