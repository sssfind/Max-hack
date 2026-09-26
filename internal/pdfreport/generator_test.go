package pdfreport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"Max-hack/internal/vacancies"
)

func TestGenerateProducesStructuredUnicodePDF(t *testing.T) {
	t.Parallel()

	in := testInput(3)
	doc, err := New(Config{MaxVacancies: 2}).Generate(context.Background(), in)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if doc.Filename != "skillgap_09-02-07_2026-09-26.pdf" {
		t.Errorf("Filename = %q", doc.Filename)
	}
	if doc.MediaType != "application/pdf" {
		t.Errorf("MediaType = %q", doc.MediaType)
	}
	if !bytes.HasPrefix(doc.Data, []byte("%PDF-")) {
		t.Fatal("document has no PDF header")
	}
	if !bytes.Contains(doc.Data, []byte("%%EOF")) {
		t.Fatal("document has no PDF trailer")
	}
	if !bytes.Contains(doc.Data, []byte("/ToUnicode")) && !bytes.Contains(doc.Data, []byte("/Identity-H")) {
		t.Fatal("document has no Unicode font mapping for Cyrillic text")
	}
	if pages := countPDFPages(doc.Data); pages < 2 {
		t.Fatalf("page count = %d, want at least 2", pages)
	}

	for i := 1; i <= 2; i++ {
		url := fmt.Sprintf("https://trudvsem.ru/vacancy/card/company/vacancy-%d", i)
		if !bytes.Contains(doc.Data, []byte(url)) {
			t.Errorf("PDF does not contain clickable URL %q", url)
		}
	}
	if bytes.Contains(doc.Data, []byte("https://trudvsem.ru/vacancy/card/company/vacancy-3")) {
		t.Error("PDF contains a vacancy beyond configured MaxVacancies")
	}
	if count := bytes.Count(doc.Data, []byte("/URI")); count < 2 {
		t.Errorf("URI structure marker count = %d, want at least 2", count)
	}
	if previewPath := os.Getenv("SKILLGAP_PDF_PREVIEW"); previewPath != "" {
		if err := os.WriteFile(previewPath, doc.Data, 0o600); err != nil {
			t.Fatalf("write preview PDF: %v", err)
		}
	}
}

func TestGenerateSupportsCyrillicAndSanitizesHTML(t *testing.T) {
	t.Parallel()

	input := "<b>Разработчик</b> информационных систем — Москва"
	got := cleanText(input)
	if got != "Разработчик информационных систем - Москва" {
		t.Fatalf("cleanText() = %q", got)
	}
	if !strings.Contains(got, "Разработчик") {
		t.Fatal("Cyrillic text was lost")
	}

	doc, err := New(Config{}).Generate(context.Background(), testInput(1))
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(doc.Data) < 1_000 {
		t.Fatalf("PDF size = %d, want a non-empty embedded-font document", len(doc.Data))
	}

	withEmoji := testInput(1)
	withEmoji.Snapshot.Vacancies[0].Title += " 🔥"
	withEmoji.Snapshot.Vacancies[0].Requirements += " Работа с клиентами 😀"
	if _, err := New(Config{}).Generate(context.Background(), withEmoji); err != nil {
		t.Fatalf("Generate() with non-BMP text error = %v", err)
	}
}

func TestGenerateHandlesLongQualificationWithoutFixedHeightOverlap(t *testing.T) {
	t.Parallel()

	in := testInput(1)
	in.Qualification = strings.Repeat("Технолог сложного производственного процесса; ", 14)
	doc, err := New(Config{}).Generate(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if pages := countPDFPages(doc.Data); pages < 2 {
		t.Fatalf("page count = %d, want a valid multi-page report for long qualification", pages)
	}
	if previewPath := os.Getenv("SKILLGAP_PDF_LONG_PREVIEW"); previewPath != "" {
		if err := os.WriteFile(previewPath, doc.Data, 0o600); err != nil {
			t.Fatalf("write long preview PDF: %v", err)
		}
	}
}

func TestVacancyCardIsFittedToOnePage(t *testing.T) {
	t.Parallel()

	in := testInput(1)
	vacancy := in.Snapshot.Vacancies[0]
	vacancy.Title = strings.Repeat("Очень длинное название должности ", 100)
	vacancy.Employer = strings.Repeat("Работодатель с длинным наименованием ", 100)
	vacancy.Requirements = strings.Repeat("Подробное требование к кандидату и его компетенциям. ", 400)
	vacancy.Duties = strings.Repeat("Подробное описание трудовых обязанностей и рабочих задач. ", 400)
	for i := 0; i < 100; i++ {
		vacancy.Skills = append(vacancy.Skills, vacancies.Skill{Name: fmt.Sprintf("Очень длинный навык номер %d", i)})
	}

	pdf := newPDF(in, in.GeneratedAt)
	pdf.AddPage()
	pdf.SetY(20)
	innerWidth := pageWidthMM - leftMarginMM - rightMarginMM - 10
	card := fitVacancyCard(pdf, vacancyCardData(vacancy), innerWidth)
	height := vacancyCardHeight(pdf, card, innerWidth)
	if height > maxVacancyCardHeightMM {
		t.Fatalf("fitted card height = %.1f mm, want <= %.1f mm", height, maxVacancyCardHeightMM)
	}
	if !strings.HasSuffix(card.requirements, "...") || !strings.HasSuffix(card.duties, "...") {
		t.Fatalf("long fields were not visibly truncated: requirements=%q duties=%q", card.requirements, card.duties)
	}

	renderVacancy(pdf, 1, vacancy)
	if page := pdf.PageNo(); page != 1 {
		t.Fatalf("single fitted card spilled to page %d", page)
	}
}

func TestGenerateHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New(Config{}).Generate(ctx, testInput(1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate() error = %v, want context.Canceled", err)
	}
}

func TestGenerateValidatesInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(*Input)
		want error
	}{
		{
			name: "program code",
			edit: func(in *Input) { in.ProgramCode = "" },
			want: ErrInvalidInput,
		},
		{
			name: "program name",
			edit: func(in *Input) { in.ProgramName = "" },
			want: ErrInvalidInput,
		},
		{
			name: "region",
			edit: func(in *Input) { in.RegionName = "" },
			want: ErrInvalidInput,
		},
		{
			name: "vacancies",
			edit: func(in *Input) { in.Snapshot.Vacancies = nil },
			want: ErrNoVacancies,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := testInput(1)
			tt.edit(&in)
			_, err := New(Config{}).Generate(context.Background(), in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Generate() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestGenerateRejectsOversizedDocument(t *testing.T) {
	t.Parallel()

	_, err := New(Config{MaxPDFBytes: 100}).Generate(context.Background(), testInput(1))
	if !errors.Is(err, ErrPDFTooLarge) {
		t.Fatalf("Generate() error = %v, want ErrPDFTooLarge", err)
	}
}

func TestSalaryFormatting(t *testing.T) {
	t.Parallel()

	from := int64(60_000)
	to := int64(90_000)
	tests := []struct {
		name string
		from *int64
		to   *int64
		want string
	}{
		{name: "range", from: &from, to: &to, want: "от 60 000 руб. до 90 000 руб."},
		{name: "from", from: &from, want: "от 60 000 руб."},
		{name: "to", to: &to, want: "до 90 000 руб."},
		{name: "none", want: "Нет данных"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatRange(tt.from, tt.to, "«руб.»"); got != tt.want {
				t.Errorf("formatRange() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSafeVacancyURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "portal link",
			url:  "https://trudvsem.ru/vacancy/card/company/id",
			want: "https://trudvsem.ru/vacancy/card/company/id",
		},
		{
			name: "portal subdomain",
			url:  "https://www.trudvsem.ru/vacancy/card/company/id",
			want: "https://www.trudvsem.ru/vacancy/card/company/id",
		},
		{name: "foreign host", url: "https://example.com/file.pdf"},
		{name: "insecure scheme", url: "http://trudvsem.ru/vacancy/card/company/id"},
		{name: "lookalike host", url: "https://trudvsem.ru.example.com/vacancy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := safeVacancyURL(tt.url); got != tt.want {
				t.Errorf("safeVacancyURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func testInput(vacancyCount int) Input {
	generatedAt := time.Date(2026, time.September, 26, 18, 30, 0, 0, time.FixedZone("MSK", 3*60*60))
	from := int64(70_000)
	to := int64(110_000)
	experience := 1
	items := make([]vacancies.Vacancy, 0, vacancyCount)
	for i := 1; i <= vacancyCount; i++ {
		items = append(items, vacancies.Vacancy{
			ID:              fmt.Sprintf("vacancy-%d", i),
			Title:           fmt.Sprintf("Младший разработчик %d", i),
			Employer:        "ООО Технологии будущего",
			Region:          "Москва",
			Location:        "г. Москва",
			Duties:          strings.Repeat("Разрабатывать и тестировать информационные системы. ", 18),
			Requirements:    strings.Repeat("Знание SQL, Git и основ программирования. ", 18),
			Education:       "Среднее профессиональное образование",
			ExperienceYears: &experience,
			Skills: []vacancies.Skill{
				{Name: "SQL", Evidence: "structured"},
				{Name: "Git", Evidence: "structured"},
				{Name: "Командная работа", Evidence: "extracted_from_text"},
			},
			Employment:  "Полная занятость",
			Schedule:    "Полный рабочий день",
			SalaryFrom:  &from,
			SalaryTo:    &to,
			Currency:    "«руб.»",
			PublishedAt: generatedAt.AddDate(0, 0, -i),
			URL:         fmt.Sprintf("https://trudvsem.ru/vacancy/card/company/vacancy-%d", i),
		})
	}

	return Input{
		ProgramCode:   "09.02.07",
		ProgramName:   "Информационные системы и программирование",
		Qualification: "Программист",
		RegionName:    "Москва",
		Roles:         []string{"Программист", "Младший разработчик"},
		Snapshot: vacancies.Snapshot{
			Vacancies: items,
			Source:    "Работа России",
			FetchedAt: generatedAt.Add(-time.Hour),
			Mode:      vacancies.ModeLive,
			Warnings:  []string{"Часть запросов была ограничена по времени"},
		},
		Statistics: vacancies.Statistics{
			VacancyCount:          vacancyCount,
			EmployerCount:         1,
			SalaryCount:           vacancyCount,
			SalaryCoveragePercent: 100,
			SalaryMedian:          int64Ptr(90_000),
			SalaryMin:             &from,
			SalaryMax:             &to,
			Currency:              "«руб.»",
			EntryLevelCount:       vacancyCount,
			EntryLevelPercent:     100,
			TopSkills: []vacancies.SkillFrequency{
				{Name: "SQL", Count: vacancyCount, Percent: 100},
				{Name: "Git", Count: vacancyCount, Percent: 100},
			},
			SmallSample: vacancyCount < 5,
		},
		GeneratedAt: generatedAt,
	}
}

func countPDFPages(data []byte) int {
	return len(regexp.MustCompile(`/Type\s*/Page\b`).FindAll(data, -1))
}

func int64Ptr(value int64) *int64 {
	return &value
}
