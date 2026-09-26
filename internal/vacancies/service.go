package vacancies

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type ServiceOptions struct {
	MaxQueries       int
	QueryConcurrency int
	PerQueryLimit    int
	MaxVacancies     int
	Now              func() time.Time
}

type Service struct {
	client *Client
	opts   ServiceOptions
}

func NewService(client *Client, opts ServiceOptions) *Service {
	if opts.MaxQueries <= 0 {
		opts.MaxQueries = 6
	}
	if opts.QueryConcurrency <= 0 {
		opts.QueryConcurrency = 3
	}
	if opts.PerQueryLimit <= 0 || opts.PerQueryLimit > 100 {
		opts.PerQueryLimit = 100
	}
	if opts.MaxVacancies <= 0 {
		opts.MaxVacancies = 50
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{client: client, opts: opts}
}

type queryResult struct {
	index     int
	query     string
	vacancies []Vacancy
	total     int64
	truncated bool
	err       error
}

func (s *Service) Analyze(ctx context.Context, req SearchRequest) (Snapshot, Statistics, error) {
	if s == nil || s.client == nil {
		return Snapshot{}, Statistics{}, errors.New("vacancies service is not configured")
	}
	if strings.TrimSpace(req.RegionCode) == "" {
		return Snapshot{}, Statistics{}, errors.New("region code is required")
	}
	queries := uniqueQueries(req.Queries, s.opts.MaxQueries)
	if len(queries) == 0 {
		return Snapshot{}, Statistics{}, errors.New("vacancy queries are empty")
	}

	results := make(chan queryResult, len(queries))
	sem := make(chan struct{}, s.opts.QueryConcurrency)
	var wg sync.WaitGroup
	for index, query := range queries {
		index := index
		query := query
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- queryResult{index: index, query: query, err: ctx.Err()}
				return
			}
			searchResult, err := s.client.searchWithMeta(ctx, req.RegionCode, query, s.opts.PerQueryLimit)
			results <- queryResult{
				index: index, query: query, vacancies: searchResult.vacancies,
				total: searchResult.total, truncated: searchResult.truncated, err: err,
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	snapshot := Snapshot{
		Source:    "Работа России (opendata.trudvsem.ru)",
		FetchedAt: s.opts.Now().UTC(),
		Mode:      ModeLive,
	}
	if req.QueryModelNeedsReview {
		snapshot.Warnings = append(snapshot.Warnings, "Поисковая модель программы требует ручной проверки; результаты являются ориентиром.")
	}

	orderedResults := make([]queryResult, len(queries))
	for result := range results {
		orderedResults[result.index] = result
	}

	var (
		all           []Vacancy
		successCount  int
		requestErrors []error
	)
	for _, result := range orderedResults {
		if result.err != nil {
			requestErrors = append(requestErrors, fmt.Errorf("query %q: %w", result.query, result.err))
			snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Запрос «%s» временно не обработан.", result.query))
			continue
		}
		successCount++
		if result.truncated {
			if result.total > 0 {
				snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf(
					"Запрос «%s»: обработано %d из %d вакансий; статистика отражает ограниченную выборку.",
					result.query, len(result.vacancies), result.total,
				))
			} else {
				snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf(
					"Запрос «%s»: источник не сообщил полный размер выдачи; статистика отражает ограниченную выборку.",
					result.query,
				))
			}
		}
		for _, vacancy := range result.vacancies {
			if isEntryLevel(vacancy) && isRelevant(vacancy, result.query) {
				all = append(all, vacancy)
			}
		}
	}
	if successCount == 0 {
		return snapshot, Statistics{}, fmt.Errorf("all vacancy requests failed: %w", errors.Join(requestErrors...))
	}
	if len(all) == 0 && len(requestErrors) > 0 {
		return snapshot, Statistics{}, fmt.Errorf("vacancy search is incomplete and successful queries returned no matching vacancies: %w", errors.Join(requestErrors...))
	}

	snapshot.Vacancies = deduplicate(all)
	sort.SliceStable(snapshot.Vacancies, func(i, j int) bool {
		left, right := snapshot.Vacancies[i], snapshot.Vacancies[j]
		if !left.PublishedAt.Equal(right.PublishedAt) {
			return left.PublishedAt.After(right.PublishedAt)
		}
		return left.Title < right.Title
	})
	if len(snapshot.Vacancies) > s.opts.MaxVacancies {
		snapshot.Vacancies = snapshot.Vacancies[:s.opts.MaxVacancies]
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("В анализ включены %d наиболее свежих вакансий из полученной выборки.", s.opts.MaxVacancies))
	}
	stats := CalculateStatistics(snapshot.Vacancies)
	return snapshot, stats, nil
}

func uniqueQueries(queries []string, limit int) []string {
	result := make([]string, 0, min(len(queries), limit))
	seen := make(map[string]struct{}, len(queries))
	for _, query := range queries {
		query = strings.TrimSpace(query)
		key := strings.ToLower(query)
		if query == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, query)
		if len(result) == limit {
			break
		}
	}
	return result
}

func isEntryLevel(v Vacancy) bool {
	title := strings.ToLower(v.Title)
	words := strings.FieldsFunc(title, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, word := range words {
		if word == "senior" || word == "lead" || word == "teamlead" {
			return false
		}
	}
	for _, marker := range []string{
		"ведущий", "главный", "руководител", "начальник", "директор", "архитектор",
	} {
		if strings.Contains(title, marker) {
			return false
		}
	}
	return v.ExperienceYears == nil || *v.ExperienceYears <= 1
}

func isRelevant(v Vacancy, query string) bool {
	terms := searchTerms(query)
	if len(terms) == 0 {
		return true
	}
	var skills strings.Builder
	for _, skill := range v.Skills {
		skills.WriteByte(' ')
		skills.WriteString(skill.Name)
	}
	haystack := strings.ToLower(strings.Join([]string{v.Title, v.Requirements, v.Duties, skills.String()}, " "))
	for _, term := range terms {
		if strings.Contains(haystack, term) {
			return true
		}
	}
	return false
}

func searchTerms(query string) []string {
	stop := map[string]struct{}{
		"для": {}, "или": {}, "без": {}, "при": {}, "работа": {}, "специалист": {}, "техник": {}, "оператор": {},
	}
	var result []string
	for _, term := range strings.Fields(strings.ToLower(query)) {
		term = strings.Trim(term, " ,.;:()[]{}\"'")
		if len([]rune(term)) < 3 {
			continue
		}
		if _, ignored := stop[term]; ignored || strings.HasPrefix(term, "техник") || strings.HasPrefix(term, "специалист") || strings.HasPrefix(term, "работ") {
			continue
		}
		result = append(result, term)
	}
	return result
}

func deduplicate(items []Vacancy) []Vacancy {
	result := make([]Vacancy, 0, len(items))
	positions := make(map[string]int, len(items))
	for _, item := range items {
		key := strings.TrimSpace(item.ID)
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(item.URL))
		}
		if key == "" {
			employerKey := item.EmployerID
			if employerKey == "" {
				employerKey = item.Employer
			}
			key = strings.ToLower(item.Title + "\x00" + employerKey + "\x00" + item.Location)
		}
		if pos, exists := positions[key]; exists {
			result[pos] = mergeVacancies(result[pos], item)
			continue
		}
		positions[key] = len(result)
		result = append(result, item)
	}
	return result
}

func mergeVacancies(left, right Vacancy) Vacancy {
	salaryFrom, salaryTo, salaryCurrency := mergeSalary(left, right)
	merged := Vacancy{
		ID:           preferText(left.ID, right.ID),
		Title:        preferText(left.Title, right.Title),
		Employer:     preferText(left.Employer, right.Employer),
		EmployerID:   preferText(left.EmployerID, right.EmployerID),
		Region:       preferText(left.Region, right.Region),
		Location:     preferText(left.Location, right.Location),
		Duties:       preferText(left.Duties, right.Duties),
		Requirements: preferText(left.Requirements, right.Requirements),
		Education:    preferText(left.Education, right.Education),
		Employment:   preferText(left.Employment, right.Employment),
		Schedule:     preferText(left.Schedule, right.Schedule),
		Currency:     salaryCurrency,
		URL:          preferText(left.URL, right.URL),
		FoundByQuery: mergeQueryNames(left.FoundByQuery, right.FoundByQuery),
		SalaryFrom:   salaryFrom,
		SalaryTo:     salaryTo,
		Skills:       mergeSkills(left.Skills, right.Skills),
	}
	if left.ExperienceYears == nil {
		merged.ExperienceYears = cloneInt(right.ExperienceYears)
	} else if right.ExperienceYears == nil || *left.ExperienceYears <= *right.ExperienceYears {
		merged.ExperienceYears = cloneInt(left.ExperienceYears)
	} else {
		merged.ExperienceYears = cloneInt(right.ExperienceYears)
	}
	merged.PublishedAt = left.PublishedAt
	if right.PublishedAt.After(merged.PublishedAt) {
		merged.PublishedAt = right.PublishedAt
	}
	sort.Slice(merged.Skills, func(i, j int) bool {
		return normalizeSkillKey(merged.Skills[i].Name) < normalizeSkillKey(merged.Skills[j].Name)
	})
	return merged
}

func preferText(left, right string) string {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" {
		return right
	}
	return left
}

func mergeQueryNames(left, right string) string {
	parts := strings.Split(left+";"+right, ";")
	seen := make(map[string]string, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			seen[strings.ToLower(part)] = part
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, seen[key])
	}
	return strings.Join(result, "; ")
}

func mergeSalary(left, right Vacancy) (*int64, *int64, string) {
	leftCount := salaryBoundCount(left)
	rightCount := salaryBoundCount(right)
	if rightCount > leftCount {
		return cloneInt64(right.SalaryFrom), cloneInt64(right.SalaryTo), strings.TrimSpace(right.Currency)
	}
	return cloneInt64(left.SalaryFrom), cloneInt64(left.SalaryTo), strings.TrimSpace(left.Currency)
}

func salaryBoundCount(v Vacancy) int {
	count := 0
	if v.SalaryFrom != nil {
		count++
	}
	if v.SalaryTo != nil {
		count++
	}
	return count
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
