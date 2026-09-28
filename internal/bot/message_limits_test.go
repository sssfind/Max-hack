package bot

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"Max-hack/internal/catalog"
	"Max-hack/internal/skillgap"
	"Max-hack/internal/vacancies"
)

func TestWithGuaranteedFooterTruncatesOnlyBody(t *testing.T) {
	body := strings.Repeat("длинный текст ", 100)
	footer := "\nИсточник: Работа России\nСрез: 27.09.2026\nРежим данных: live\n\nЭто ориентир, а не гарантия."

	result := withGuaranteedFooter(body, footer, 240)
	if got := utf8.RuneCountInString(result); got > 240 {
		t.Fatalf("message length = %d, want <= 240", got)
	}
	if !strings.HasSuffix(result, footer) {
		t.Fatalf("mandatory footer was truncated: %q", result)
	}
	if !strings.Contains(result, "…") {
		t.Fatal("long body was not visibly truncated")
	}
}

func TestFormatStatisticsKeepsSkillGapSourceAndCaveatAtMessageLimit(t *testing.T) {
	longValue := strings.Repeat("подробное доказательство из образовательной программы ", 90)
	snapshot := vacancies.Snapshot{
		Source: "Работа России", FetchedAt: time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC), Mode: vacancies.ModeLive,
		Warnings: []string{longValue, longValue},
	}
	stats := vacancies.Statistics{
		VacancyCount: 100,
		TopSkills: []vacancies.SkillFrequency{
			{Name: longValue, Count: 90, Percent: 90},
			{Name: longValue, Count: 80, Percent: 80},
		},
	}
	job := analysisJob{program: &catalog.Program{Code: "09.02.11"}, selection: selection{RegionName: "Москва"}}
	gap := &skillgap.Result{
		Source: skillgap.SourceMetadata{Type: skillgap.DocumentPOP, URL: "https://spolab.firpo.ru/09.02.11/document.pdf", FetchedAt: snapshot.FetchedAt},
		Matches: []skillgap.SkillMatch{{
			Skill: longValue, Status: skillgap.StatusPartial,
			Evidence: []skillgap.Evidence{{Section: longValue, Excerpt: longValue}},
		}},
	}

	result := formatStatistics(job, snapshot, stats, gap, "")
	if got := utf8.RuneCountInString(result); got > 3900 {
		t.Fatalf("statistics length = %d, want <= 3900", got)
	}
	for _, required := range []string{
		"Skill Gap — документ:",
		"https://spolab.firpo.ru/09.02.11/document.pdf",
		"не означает, что конкретный колледж не обучает навыку",
		"Источник: Работа России",
		"не прогноз или гарантия зарплаты после выпуска",
	} {
		if !strings.Contains(result, required) {
			t.Fatalf("mandatory text %q was truncated", required)
		}
	}
}

func TestWithGuaranteedFooterLeavesShortMessageUnchanged(t *testing.T) {
	if got := withGuaranteedFooter("body", " footer", 100); got != "body footer" {
		t.Fatalf("result = %q", got)
	}
}

func TestTruncateHandlesSmallLimits(t *testing.T) {
	if got := truncate("abc", 0); got != "" {
		t.Fatalf("truncate at zero = %q", got)
	}
	if got := truncate("abc", 1); got != "…" {
		t.Fatalf("truncate at one = %q", got)
	}
}

func TestFormatStatisticsKeepsProvenanceAndDisclaimerAtMessageLimit(t *testing.T) {
	longValue := strings.Repeat("очень подробное требование работодателя ", 30)
	salary := int64(95_000)
	snapshot := vacancies.Snapshot{
		Source: "Работа России", FetchedAt: time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC), Mode: vacancies.ModeCache,
		Vacancies: []vacancies.Vacancy{
			{Title: longValue, Employer: longValue, URL: "https://trudvsem.ru/vacancy/1", SalaryFrom: &salary, Currency: "RUB"},
			{Title: longValue, Employer: longValue, URL: "https://trudvsem.ru/vacancy/2", SalaryFrom: &salary, Currency: "RUB"},
			{Title: longValue, Employer: longValue, URL: "https://trudvsem.ru/vacancy/3", SalaryFrom: &salary, Currency: "RUB"},
		},
		Warnings: []string{longValue, longValue},
	}
	topSkills := make([]vacancies.SkillFrequency, 80)
	for index := range topSkills {
		topSkills[index] = vacancies.SkillFrequency{Name: longValue, Count: index + 1, Percent: 50}
	}
	stats := vacancies.Statistics{
		VacancyCount: 80, EmployerCount: 30, SalaryCount: 20, SalaryCoveragePercent: 25,
		SalaryMedian: &salary, SalaryMin: &salary, SalaryMax: &salary, Currency: "RUB", TopSkills: topSkills,
	}
	job := analysisJob{
		program:   &catalog.Program{Code: "09.02.11"},
		selection: selection{RegionName: "Москва"},
	}

	result := formatStatistics(job, snapshot, stats, nil, "")
	if got := utf8.RuneCountInString(result); got > 3900 {
		t.Fatalf("statistics length = %d, want <= 3900", got)
	}
	for _, required := range []string{
		"Источник: Работа России",
		"Срез: 27.09.2026 15:30 МСК",
		"Режим данных: последний сохранённый срез",
		"не прогноз или гарантия зарплаты после выпуска",
	} {
		if !strings.Contains(result, required) {
			t.Fatalf("mandatory text %q was truncated", required)
		}
	}
}
