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
	MaxQueries        int
	QueryConcurrency  int
	PerQueryLimit     int
	MaxVacancies      int
	MinRelevanceScore int
	CacheTTL          time.Duration
	StaleCacheTTL     time.Duration
	CacheMaxEntries   int
	CacheMaxBytes     int64
	DisableCache      bool
	Now               func() time.Time
}

type Service struct {
	client  *Client
	opts    ServiceOptions
	cacheMu sync.RWMutex
	cache   map[string]cacheEntry
	// cacheBytes is an upper-bound estimate of retained cache data. It is
	// protected by cacheMu together with cache.
	cacheBytes int64
}

type cacheEntry struct {
	snapshot Snapshot
	stats    Statistics
	storedAt time.Time
	size     int64
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
	if opts.MinRelevanceScore <= 0 {
		opts.MinRelevanceScore = 18
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 5 * time.Minute
	}
	if opts.StaleCacheTTL <= 0 {
		opts.StaleCacheTTL = 24 * time.Hour
	}
	if opts.StaleCacheTTL < opts.CacheTTL {
		opts.StaleCacheTTL = opts.CacheTTL
	}
	if opts.CacheMaxEntries <= 0 {
		opts.CacheMaxEntries = 512
	}
	if opts.CacheMaxBytes <= 0 {
		opts.CacheMaxBytes = 32 << 20
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{client: client, opts: opts, cache: make(map[string]cacheEntry)}
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
	req.Queries = queries
	key := cacheKey(req)
	now := s.opts.Now().UTC()
	if !s.opts.DisableCache {
		if snapshot, stats, ok := s.cached(key, now, s.opts.CacheTTL, false); ok {
			return snapshot, stats, nil
		}
	}

	snapshot, stats, err := s.analyzeLive(ctx, req)
	if err == nil {
		if !s.opts.DisableCache {
			s.storeCache(key, snapshot, stats, s.opts.Now().UTC())
		}
		return snapshot, stats, nil
	}
	if ctx.Err() != nil {
		return snapshot, stats, err
	}
	if !s.opts.DisableCache {
		if cached, cachedStats, ok := s.cached(key, now, s.opts.StaleCacheTTL, true); ok {
			warning := "Официальный источник временно недоступен; показан последний успешный срез."
			if cached.Mode == ModeTest {
				warning = "Настраиваемый тестовый источник временно недоступен; показан последний успешный тестовый срез."
			}
			cached.Warnings = append(cached.Warnings, warning)
			return cached, cachedStats, nil
		}
	}
	return snapshot, stats, err
}

func (s *Service) analyzeLive(ctx context.Context, req SearchRequest) (Snapshot, Statistics, error) {
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
		Source:    s.client.source,
		FetchedAt: s.opts.Now().UTC(),
		Mode:      s.client.mode,
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
			snapshot.Partial = true
			requestErrors = append(requestErrors, fmt.Errorf("query %q: %w", result.query, result.err))
			snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("Запрос «%s» временно не обработан.", result.query))
			continue
		}
		successCount++
		if result.total > 0 {
			snapshot.Sampling.SourceTotal = saturatingSourceTotal(snapshot.Sampling.SourceTotal, result.total)
		}
		snapshot.Sampling.Retrieved += len(result.vacancies)
		if result.truncated {
			snapshot.Partial = true
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
			if !isEntryLevel(vacancy) {
				continue
			}
			snapshot.Sampling.EntryLevelPassed++
			if vacancy.ExperienceYears == nil {
				snapshot.Sampling.ExperienceUnknownPassed++
			}
			vacancy.EducationClass = classifyEducation(vacancy)
			if vacancy.EducationClass == EducationHigherOnly {
				snapshot.Sampling.RejectedHigherEducation++
				continue
			}
			snapshot.Sampling.EducationPassed++
			vacancy.RelevanceScore = relevanceScore(vacancy, result.query, req.Qualification)
			if vacancy.RelevanceScore < s.opts.MinRelevanceScore {
				continue
			}
			snapshot.Sampling.RelevantPassed++
			all = append(all, vacancy)
		}
	}
	if successCount == 0 {
		return snapshot, Statistics{}, fmt.Errorf("all vacancy requests failed: %w", errors.Join(requestErrors...))
	}
	if len(all) == 0 && len(requestErrors) > 0 {
		return snapshot, Statistics{}, fmt.Errorf("vacancy search is incomplete and successful queries returned no matching vacancies: %w", errors.Join(requestErrors...))
	}

	if snapshot.Sampling.RejectedHigherEducation > 0 {
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf(
			"Исключено позиций в выдачах с обязательным высшим образованием: %d; до удаления повторов.",
			snapshot.Sampling.RejectedHigherEducation,
		))
	}
	snapshot.Vacancies = deduplicate(all)
	snapshot.Sampling.Deduplicated = len(snapshot.Vacancies)
	sort.SliceStable(snapshot.Vacancies, func(i, j int) bool {
		left, right := snapshot.Vacancies[i], snapshot.Vacancies[j]
		if left.RelevanceScore != right.RelevanceScore {
			return left.RelevanceScore > right.RelevanceScore
		}
		if !left.PublishedAt.Equal(right.PublishedAt) {
			return left.PublishedAt.After(right.PublishedAt)
		}
		return left.Title < right.Title
	})
	if len(snapshot.Vacancies) > s.opts.MaxVacancies {
		snapshot.Vacancies = snapshot.Vacancies[:s.opts.MaxVacancies]
		snapshot.Warnings = append(snapshot.Warnings, fmt.Sprintf("В анализ включены %d наиболее релевантных вакансий из полученной выборки.", s.opts.MaxVacancies))
	}
	snapshot.Sampling.Included = len(snapshot.Vacancies)
	stats := CalculateStatistics(snapshot.Vacancies)
	return snapshot, stats, nil
}

func saturatingSourceTotal(current int, additional int64) int {
	if additional <= 0 {
		return current
	}
	maxInt := int(^uint(0) >> 1)
	if current >= maxInt || additional > int64(maxInt-current) {
		return maxInt
	}
	return current + int(additional)
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

func cacheKey(req SearchRequest) string {
	parts := make([]string, 0, len(req.Queries)+3)
	parts = append(parts,
		strings.ToLower(strings.TrimSpace(req.RegionCode)),
		strings.ToLower(strings.TrimSpace(req.ProgramCode)),
		strings.ToLower(strings.TrimSpace(req.Qualification)),
		fmt.Sprintf("review=%t", req.QueryModelNeedsReview),
	)
	for _, query := range req.Queries {
		parts = append(parts, strings.ToLower(strings.Join(strings.Fields(query), " ")))
	}
	return strings.Join(parts, "\x00")
}

func (s *Service) cached(key string, now time.Time, ttl time.Duration, stale bool) (Snapshot, Statistics, bool) {
	s.cacheMu.RLock()
	entry, ok := s.cache[key]
	s.cacheMu.RUnlock()
	if !ok || ttl <= 0 || now.Sub(entry.storedAt) > ttl {
		return Snapshot{}, Statistics{}, false
	}
	snapshot := cloneSnapshot(entry.snapshot)
	if snapshot.Mode != ModeTest {
		snapshot.Mode = ModeCache
	}
	if stale {
		snapshot.Warnings = append(snapshot.Warnings, "Кэш старше обычного срока обновления.")
	} else {
		snapshot.Warnings = append(snapshot.Warnings, "Использован недавний успешный срез, чтобы не повторять запросы к источнику.")
	}
	return snapshot, cloneStatistics(entry.stats), true
}

func (s *Service) storeCache(key string, snapshot Snapshot, stats Statistics, now time.Time) {
	size := estimateCacheEntryBytes(key, snapshot, stats)
	if size > s.opts.CacheMaxBytes {
		return
	}
	storedSnapshot := cloneSnapshot(snapshot)
	storedStats := cloneStatistics(stats)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if previous, exists := s.cache[key]; snapshot.Partial && exists && !previous.snapshot.Partial && now.Sub(previous.storedAt) <= s.opts.StaleCacheTTL {
		return
	}
	for candidate, entry := range s.cache {
		if now.Sub(entry.storedAt) > s.opts.StaleCacheTTL {
			s.cacheBytes -= entry.size
			delete(s.cache, candidate)
		}
	}
	if previous, exists := s.cache[key]; exists {
		s.cacheBytes -= previous.size
		delete(s.cache, key)
	}
	for len(s.cache) >= s.opts.CacheMaxEntries || s.cacheBytes+size > s.opts.CacheMaxBytes {
		oldestKey := ""
		var oldest time.Time
		for candidate, entry := range s.cache {
			if oldestKey == "" || entry.storedAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.storedAt
			}
		}
		if oldestKey == "" {
			break
		}
		s.cacheBytes -= s.cache[oldestKey].size
		delete(s.cache, oldestKey)
	}
	s.cache[key] = cacheEntry{snapshot: storedSnapshot, stats: storedStats, storedAt: now, size: size}
	s.cacheBytes += size
}

func estimateCacheEntryBytes(key string, snapshot Snapshot, stats Statistics) int64 {
	// The fixed allowances intentionally overestimate slice/map/struct overhead;
	// variable-sized strings are then counted exactly in bytes.
	size := int64(1024 + len(key) + len(snapshot.Source))
	for _, warning := range snapshot.Warnings {
		size += int64(32 + len(warning))
	}
	for _, vacancy := range snapshot.Vacancies {
		size += int64(512 + len(vacancy.ID) + len(vacancy.Title) + len(vacancy.Employer) + len(vacancy.EmployerID) +
			len(vacancy.Region) + len(vacancy.Location) + len(vacancy.Duties) + len(vacancy.Requirements) +
			len(vacancy.Education) + len(vacancy.Employment) + len(vacancy.Schedule) + len(vacancy.Currency) +
			len(vacancy.SalaryText) + len(vacancy.URL) + len(vacancy.FoundByQuery))
		for _, skill := range vacancy.Skills {
			size += int64(192 + len(skill.Name) + len(skill.Evidence) + len(skill.EvidenceField) + len(skill.EvidenceExcerpt))
		}
	}
	size += int64(512 + len(stats.Currency) + len(stats.SalaryMethod))
	for _, skill := range stats.TopSkills {
		size += int64(96 + len(skill.Name))
	}
	return size
}

func cloneSnapshot(source Snapshot) Snapshot {
	result := source
	result.Warnings = append([]string(nil), source.Warnings...)
	result.Vacancies = make([]Vacancy, len(source.Vacancies))
	for i, vacancy := range source.Vacancies {
		result.Vacancies[i] = vacancy
		result.Vacancies[i].Skills = append([]Skill(nil), vacancy.Skills...)
		result.Vacancies[i].ExperienceYears = cloneInt(vacancy.ExperienceYears)
		result.Vacancies[i].SalaryFrom = cloneInt64(vacancy.SalaryFrom)
		result.Vacancies[i].SalaryTo = cloneInt64(vacancy.SalaryTo)
	}
	return result
}

func cloneStatistics(source Statistics) Statistics {
	result := source
	result.TopSkills = append([]SkillFrequency(nil), source.TopSkills...)
	result.SalaryMedian = cloneInt64(source.SalaryMedian)
	result.SalaryMin = cloneInt64(source.SalaryMin)
	result.SalaryMax = cloneInt64(source.SalaryMax)
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
	return relevanceScore(v, query, "") >= 18
}

func relevanceScore(v Vacancy, query, role string) int {
	if hasHardNegative(v) {
		return 0
	}
	queryTerms := searchTerms(query)
	roleTerms := searchTerms(role)
	if len(queryTerms) == 0 && len(roleTerms) == 0 {
		return 0
	}

	title := normalizeMatchText(v.Title)
	requirements := normalizeMatchText(v.Requirements)
	duties := normalizeMatchText(v.Duties)
	queryPhrase := normalizeMatchText(query)
	rolePhrase := normalizeMatchText(role)
	skillText := ""
	for _, skill := range v.Skills {
		skillText += " " + normalizeMatchText(skill.Name)
	}

	score := 0
	if queryPhrase != "" && containsPhrase(title, queryPhrase) {
		score += 80
	}
	if rolePhrase != "" && containsPhrase(title, rolePhrase) {
		score += 35
	}
	for _, term := range uniqueTerms(append(queryTerms, roleTerms...)) {
		switch {
		case containsTerm(title, term):
			score += titleTermWeight(term)
		case containsTerm(skillText, term):
			score += 20
		case containsTerm(requirements, term):
			score += 7
		case containsTerm(duties, term):
			score += 4
		}
	}
	uniqueQueryTerms := uniqueTerms(queryTerms)
	allQueryTermsInTitle := len(uniqueQueryTerms) > 1
	for _, term := range uniqueQueryTerms {
		if !containsTerm(title, term) {
			allQueryTermsInTitle = false
			break
		}
	}
	if allQueryTermsInTitle {
		score += 25
	}
	if queryPhrase != "" && containsPhrase(requirements, queryPhrase) {
		score += 5
	}
	if queryPhrase != "" && containsPhrase(duties, queryPhrase) {
		score += 3
	}
	return score
}

func titleTermWeight(term string) int {
	switch term {
	case "специалист", "техник", "оператор", "инженер", "мастер", "менеджер", "работник":
		return 5
	default:
		return 18
	}
}

func hasHardNegative(v Vacancy) bool {
	return !isEntryLevel(v) || classifyEducation(v) == EducationHigherOnly
}

func normalizeMatchText(value string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+' || r == '#' {
			b.WriteRune(r)
			lastSpace = false
		} else if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func containsPhrase(text, phrase string) bool {
	if phrase == "" {
		return false
	}
	return strings.Contains(" "+text+" ", " "+phrase+" ")
}

func containsTerm(text, term string) bool {
	if term == "" {
		return false
	}
	for _, word := range strings.Fields(text) {
		if word == term || len([]rune(term)) >= 5 && strings.HasPrefix(word, term) {
			return true
		}
	}
	return false
}

func uniqueTerms(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	result := make([]string, 0, len(terms))
	for _, term := range terms {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		result = append(result, term)
	}
	return result
}

func classifyEducation(v Vacancy) EducationRequirement {
	text := normalizeMatchText(strings.Join([]string{v.Education, v.Requirements}, " "))
	if text == "" {
		return EducationUnknown
	}
	if containsAny(text,
		"образование не требуется", "без требований к образованию", "не имеет значения",
		"высшее образование не обязательно", "высшее не обязательно", "можно без высшего",
		"желательно высшее", "высшее приветствуется",
	) {
		return EducationNotRequired
	}
	hasHigher := containsAny(text, "высшее", "бакалавриат", "магистратура", "специалитет")
	hasVocational := containsAny(text, "среднее профессиональное", "среднее специальное", "начальное профессиональное", "спо")
	if hasHigher && hasVocational {
		return EducationHigherOrVocational
	}
	if hasVocational {
		return EducationSecondaryVocational
	}
	if hasHigher {
		return EducationHigherOnly
	}
	if containsAny(text, "среднее общее", "основное общее", "общее образование") {
		return EducationSecondaryGeneral
	}
	return EducationUnknown
}

func containsAny(text string, phrases ...string) bool {
	for _, phrase := range phrases {
		if containsPhrase(text, normalizeMatchText(phrase)) {
			return true
		}
	}
	return false
}

func searchTerms(query string) []string {
	stop := map[string]struct{}{
		"для": {}, "или": {}, "без": {}, "при": {}, "работа": {}, "по": {}, "на": {}, "под": {}, "над": {},
		"опыт": {}, "опыта": {}, "стажер": {}, "стажёр": {}, "стажировка": {}, "младший": {}, "начинающий": {},
		"junior": {}, "intern": {}, "trainee": {},
	}
	var result []string
	for _, term := range strings.Fields(strings.ToLower(query)) {
		term = strings.Trim(term, " ,.;:()[]{}\"'")
		if len([]rune(term)) < 3 {
			continue
		}
		if _, ignored := stop[term]; ignored || strings.HasPrefix(term, "работ") {
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
	salaryFrom, salaryTo, salaryCurrency, salaryText := mergeSalary(left, right)
	merged := Vacancy{
		ID:             preferText(left.ID, right.ID),
		Title:          preferText(left.Title, right.Title),
		Employer:       preferText(left.Employer, right.Employer),
		EmployerID:     preferText(left.EmployerID, right.EmployerID),
		Region:         preferText(left.Region, right.Region),
		Location:       preferText(left.Location, right.Location),
		Duties:         preferText(left.Duties, right.Duties),
		Requirements:   preferText(left.Requirements, right.Requirements),
		Education:      preferText(left.Education, right.Education),
		EducationClass: preferEducation(left.EducationClass, right.EducationClass),
		Employment:     preferText(left.Employment, right.Employment),
		Schedule:       preferText(left.Schedule, right.Schedule),
		Currency:       salaryCurrency,
		SalaryText:     salaryText,
		URL:            preferText(left.URL, right.URL),
		FoundByQuery:   mergeQueryNames(left.FoundByQuery, right.FoundByQuery),
		RelevanceScore: max(left.RelevanceScore, right.RelevanceScore),
		SalaryFrom:     salaryFrom,
		SalaryTo:       salaryTo,
		Skills:         mergeSkills(left.Skills, right.Skills),
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

func preferEducation(left, right EducationRequirement) EducationRequirement {
	if left == "" || left == EducationUnknown {
		return right
	}
	return left
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

func mergeSalary(left, right Vacancy) (*int64, *int64, string, string) {
	leftScore := salaryRecordScore(left)
	rightScore := salaryRecordScore(right)
	if rightScore > leftScore {
		return cloneInt64(right.SalaryFrom), cloneInt64(right.SalaryTo), strings.TrimSpace(right.Currency), strings.TrimSpace(right.SalaryText)
	}
	return cloneInt64(left.SalaryFrom), cloneInt64(left.SalaryTo), strings.TrimSpace(left.Currency), strings.TrimSpace(left.SalaryText)
}

func salaryRecordScore(v Vacancy) int {
	count := 0
	if v.SalaryFrom != nil {
		count += 2
	}
	if v.SalaryTo != nil {
		count += 2
	}
	if strings.TrimSpace(v.SalaryText) != "" {
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
