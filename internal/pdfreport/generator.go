package pdfreport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"Max-hack/internal/vacancies"

	"github.com/go-pdf/fpdf"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
)

const (
	DefaultMaxVacancies = 15
	DefaultMaxPDFBytes  = 10 << 20

	mediaTypePDF           = "application/pdf"
	fontFamily             = "Go"
	pageWidthMM            = 210.0
	leftMarginMM           = 16.0
	rightMarginMM          = 16.0
	bottomLimitMM          = 276.0
	maxVacancyCardHeightMM = 225.0
)

var (
	ErrNoVacancies  = errors.New("pdf report requires at least one vacancy")
	ErrInvalidInput = errors.New("invalid pdf report input")
	ErrInvalidPDF   = errors.New("generated document is not a valid PDF")
	ErrPDFTooLarge  = errors.New("generated PDF exceeds the configured size limit")

	spacePattern = regexp.MustCompile(`\s+`)
	tagPattern   = regexp.MustCompile(`<[^>]*>`)
)

// Config controls report size. Zero values select safe defaults.
type Config struct {
	MaxVacancies int
	MaxPDFBytes  int
}

// Input contains already-normalized market data. Generator never performs I/O.
type Input struct {
	ProgramCode   string
	ProgramName   string
	Qualification string
	RegionName    string
	Roles         []string
	Snapshot      vacancies.Snapshot
	Statistics    vacancies.Statistics
	GeneratedAt   time.Time
}

// Document is ready to upload to MAX as a file attachment.
type Document struct {
	Filename  string
	MediaType string
	Data      []byte
}

// Generator renders compact A4 market reports.
type Generator struct {
	maxVacancies int
	maxPDFBytes  int
}

// New creates a report generator. Invalid limits are replaced with defaults.
func New(cfg Config) *Generator {
	maxVacancies := cfg.MaxVacancies
	if maxVacancies <= 0 {
		maxVacancies = DefaultMaxVacancies
	}
	maxPDFBytes := cfg.MaxPDFBytes
	if maxPDFBytes <= 0 {
		maxPDFBytes = DefaultMaxPDFBytes
	}
	return &Generator{
		maxVacancies: maxVacancies,
		maxPDFBytes:  maxPDFBytes,
	}
}

// Generate builds an in-memory PDF. It does not create temporary or output files.
func (g *Generator) Generate(ctx context.Context, in Input) (Document, error) {
	if err := contextErr(ctx); err != nil {
		return Document{}, err
	}
	if err := validateInput(in); err != nil {
		return Document{}, err
	}

	generatedAt := in.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = in.Snapshot.FetchedAt
	}
	if generatedAt.IsZero() {
		generatedAt = time.Now()
	}

	maxVacancies := g.maxVacancies
	if maxVacancies <= 0 {
		maxVacancies = DefaultMaxVacancies
	}
	maxPDFBytes := g.maxPDFBytes
	if maxPDFBytes <= 0 {
		maxPDFBytes = DefaultMaxPDFBytes
	}
	limit := min(len(in.Snapshot.Vacancies), maxVacancies)
	pdf := newPDF(in, generatedAt)
	pdf.AddPage()
	renderOverview(pdf, in, generatedAt, limit)

	for i := 0; i < limit; i++ {
		if err := contextErr(ctx); err != nil {
			return Document{}, err
		}
		renderVacancy(pdf, i+1, in.Snapshot.Vacancies[i])
	}

	if err := contextErr(ctx); err != nil {
		return Document{}, err
	}

	var output bytes.Buffer
	if err := pdf.Output(&output); err != nil {
		return Document{}, fmt.Errorf("render PDF: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return Document{}, err
	}

	data := output.Bytes()
	if !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.Contains(data, []byte("%%EOF")) {
		return Document{}, ErrInvalidPDF
	}
	if len(data) > maxPDFBytes {
		return Document{}, fmt.Errorf("%w: got %d bytes, limit %d", ErrPDFTooLarge, len(data), maxPDFBytes)
	}

	return Document{
		Filename:  reportFilename(in.ProgramCode, generatedAt),
		MediaType: mediaTypePDF,
		Data:      append([]byte(nil), data...),
	}, nil
}

func contextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func validateInput(in Input) error {
	if strings.TrimSpace(in.ProgramCode) == "" {
		return fmt.Errorf("%w: program code is required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.ProgramName) == "" {
		return fmt.Errorf("%w: program name is required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.RegionName) == "" {
		return fmt.Errorf("%w: region name is required", ErrInvalidInput)
	}
	if len(in.Snapshot.Vacancies) == 0 {
		return ErrNoVacancies
	}
	return nil
}

func newPDF(in Input, generatedAt time.Time) *fpdf.Fpdf {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(leftMarginMM, 20, rightMarginMM)
	pdf.SetAutoPageBreak(true, 21)
	pdf.SetCompression(true)
	pdf.AddUTF8FontFromBytes(fontFamily, "", goregular.TTF)
	pdf.AddUTF8FontFromBytes(fontFamily, "B", gobold.TTF)
	pdf.SetTitle(cleanText("SkillGap - "+in.ProgramCode+" "+in.ProgramName), true)
	pdf.SetSubject("Анализ текущих стартовых вакансий", true)
	pdf.SetAuthor("SkillGap", true)
	pdf.SetCreator("SkillGap", true)
	pdf.SetCreationDate(generatedAt)
	pdf.AliasNbPages("")

	pdf.SetHeaderFunc(func() {
		pdf.SetFont(fontFamily, "B", 8.5)
		pdf.SetTextColor(28, 47, 68)
		pdf.Text(leftMarginMM, 11, "SKILLGAP")
		pdf.SetDrawColor(210, 220, 231)
		pdf.Line(leftMarginMM, 13, pageWidthMM-rightMarginMM, 13)
		pdf.SetY(20)
	})
	pdf.SetFooterFunc(func() {
		pdf.SetY(-14)
		pdf.SetDrawColor(210, 220, 231)
		pdf.Line(leftMarginMM, pdf.GetY(), pageWidthMM-rightMarginMM, pdf.GetY())
		pdf.SetY(-11)
		pdf.SetFont(fontFamily, "", 7.5)
		pdf.SetTextColor(101, 116, 133)
		pdf.CellFormat(120, 5, "Источник: "+cleanText(sourceName(in.Snapshot.Source)), "", 0, "L", false, 0, "")
		pdf.CellFormat(0, 5, fmt.Sprintf("Страница %d/{nb}", pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	return pdf
}

func renderOverview(pdf *fpdf.Fpdf, in Input, generatedAt time.Time, vacancyLimit int) {
	pdf.SetFillColor(28, 47, 68)
	pdf.Rect(0, 14, pageWidthMM, 50, "F")
	pdf.SetXY(leftMarginMM, 23)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont(fontFamily, "B", 20)
	pdf.MultiCell(pageWidthMM-leftMarginMM-rightMarginMM, 8, "Анализ стартовых вакансий", "", "L", false)
	pdf.SetFont(fontFamily, "", 10)
	pdf.SetTextColor(218, 232, 246)
	pdf.MultiCell(pageWidthMM-leftMarginMM-rightMarginMM, 5.5,
		cleanText(in.ProgramCode+" - "+in.ProgramName), "", "L", false)

	pdf.SetY(70)
	drawSelectionBlock(pdf, in, generatedAt)
	sectionTitle(pdf, "Сводка рынка")
	drawMetrics(pdf, in)
	drawSalarySummary(pdf, in.Statistics)
	drawTopSkills(pdf, in.Statistics.TopSkills)
	drawRoles(pdf, in.Roles)
	drawDataNotes(pdf, in)

	pdf.Ln(2)
	sectionTitle(pdf, fmt.Sprintf("Вакансии в отчёте (%d из %d)", vacancyLimit, len(in.Snapshot.Vacancies)))
	pdf.SetFont(fontFamily, "", 8.5)
	pdf.SetTextColor(71, 85, 105)
	pdf.MultiCell(0, 4.5,
		fmt.Sprintf("В отчёт включено %d наиболее релевантных позиций. Зарплаты без опубликованных значений не оцениваются.", vacancyLimit),
		"", "L", false)
	pdf.Ln(2)
}

func drawSelectionBlock(pdf *fpdf.Fpdf, in Input, generatedAt time.Time) {
	lines := []string{
		"Регион: " + cleanText(in.RegionName),
		"Квалификация: " + valueOrNoData(in.Qualification),
		"Дата формирования: " + generatedAt.Format("02.01.2006 15:04"),
		"Режим данных: " + valueOrNoData(string(in.Snapshot.Mode)),
	}
	text := strings.Join(lines, "\n")
	pdf.SetFillColor(241, 246, 251)
	pdf.SetDrawColor(210, 220, 231)
	y := pdf.GetY()
	innerWidth := pageWidthMM - leftMarginMM - rightMarginMM - 10
	pdf.SetFont(fontFamily, "", 9.5)
	height := linesHeight(pdf, text, innerWidth, 4.4) + 8
	pdf.Rect(leftMarginMM, y, pageWidthMM-leftMarginMM-rightMarginMM, height, "DF")
	pdf.SetXY(leftMarginMM+5, y+4)
	pdf.SetTextColor(28, 47, 68)
	pdf.MultiCell(innerWidth, 4.4, text, "", "L", false)
	pdf.SetY(y + height + 4)
}

type metric struct {
	value string
	label string
}

func drawMetrics(pdf *fpdf.Fpdf, in Input) {
	stats := in.Statistics
	vacancyCount := stats.VacancyCount
	if vacancyCount == 0 {
		vacancyCount = len(in.Snapshot.Vacancies)
	}
	employerCount := stats.EmployerCount
	if employerCount == 0 {
		employerCount = countEmployers(in.Snapshot.Vacancies)
	}
	metrics := []metric{
		{fmt.Sprintf("%d", vacancyCount), "актуальных вакансий"},
		{fmt.Sprintf("%d", employerCount), "работодателей"},
		{fmt.Sprintf("%d", stats.SalaryCount), "зарплатных наблюдений"},
		{fmt.Sprintf("%d%%", stats.SalaryCoveragePercent), "с опубликованной зарплатой"},
		{fmt.Sprintf("%d%%", stats.EntryLevelPercent), "без опыта или до 1 года"},
		{fmt.Sprintf("%d%%", stats.RemotePercent), "с удалённым форматом"},
	}

	const gap = 4.0
	width := (pageWidthMM - leftMarginMM - rightMarginMM - gap) / 2
	height := 19.0
	startY := pdf.GetY()
	for i, item := range metrics {
		row := i / 2
		col := i % 2
		x := leftMarginMM + float64(col)*(width+gap)
		y := startY + float64(row)*(height+gap)
		pdf.SetFillColor(246, 249, 252)
		pdf.SetDrawColor(222, 229, 237)
		pdf.Rect(x, y, width, height, "DF")
		pdf.SetXY(x+4, y+3)
		pdf.SetFont(fontFamily, "B", 13)
		pdf.SetTextColor(13, 148, 136)
		pdf.CellFormat(width-8, 6, item.value, "", 1, "L", false, 0, "")
		pdf.SetX(x + 4)
		pdf.SetFont(fontFamily, "", 8.3)
		pdf.SetTextColor(71, 85, 105)
		pdf.CellFormat(width-8, 5, item.label, "", 1, "L", false, 0, "")
	}
	rows := (len(metrics) + 1) / 2
	pdf.SetY(startY + float64(rows)*height + float64(rows-1)*gap + 4)
}

func drawSalarySummary(pdf *fpdf.Fpdf, stats vacancies.Statistics) {
	ensureSpace(pdf, 22)
	pdf.SetFont(fontFamily, "B", 10)
	pdf.SetTextColor(28, 47, 68)
	pdf.CellFormat(0, 5, "Ориентир по опубликованным зарплатам", "", 1, "L", false, 0, "")
	pdf.SetFont(fontFamily, "", 9)
	pdf.SetTextColor(51, 65, 85)
	text := "Нет данных: работодатели в текущей выборке не указали зарплату."
	if stats.SalaryCount > 0 && stats.SalaryMedian != nil {
		sampleCount := stats.SalarySampleCount
		if sampleCount == 0 {
			sampleCount = stats.SalaryCount
		}
		parts := []string{"медиана " + formatMoney(*stats.SalaryMedian, stats.Currency)}
		if stats.SalaryMin != nil || stats.SalaryMax != nil {
			parts = append(parts, "диапазон "+formatRange(stats.SalaryMin, stats.SalaryMax, stats.Currency))
		}
		text = fmt.Sprintf("%s; расчёт по %d вакансиям в одной валюте.", strings.Join(parts, ", "), sampleCount)
	}
	pdf.MultiCell(0, 4.8, cleanText(text), "", "L", false)
	if stats.SmallSample {
		pdf.SetTextColor(180, 83, 9)
		pdf.MultiCell(0, 4.5, "Выборка мала: значения могут заметно меняться при обновлении вакансий.", "", "L", false)
	}
	pdf.Ln(2)
}

func drawTopSkills(pdf *fpdf.Fpdf, skills []vacancies.SkillFrequency) {
	if len(skills) == 0 {
		return
	}
	ensureSpace(pdf, 18)
	pdf.SetFont(fontFamily, "B", 10)
	pdf.SetTextColor(28, 47, 68)
	pdf.CellFormat(0, 5, "Часто встречающиеся навыки", "", 1, "L", false, 0, "")
	pdf.SetFont(fontFamily, "", 8.8)
	pdf.SetTextColor(51, 65, 85)
	limit := min(len(skills), 8)
	parts := make([]string, 0, limit)
	for _, skill := range skills[:limit] {
		name := cleanText(skill.Name)
		if name == "" {
			continue
		}
		if skill.Percent > 0 {
			parts = append(parts, fmt.Sprintf("%s - %d вакансий (%d%%)", name, skill.Count, skill.Percent))
		} else {
			parts = append(parts, fmt.Sprintf("%s - %d вакансий", name, skill.Count))
		}
	}
	if len(parts) > 0 {
		pdf.MultiCell(0, 4.5, strings.Join(parts, "\n"), "", "L", false)
		pdf.Ln(2)
	}
}

func drawRoles(pdf *fpdf.Fpdf, roles []string) {
	roles = uniqueNonEmpty(roles, 8)
	if len(roles) == 0 {
		return
	}
	ensureSpace(pdf, 15)
	pdf.SetFont(fontFamily, "B", 10)
	pdf.SetTextColor(28, 47, 68)
	pdf.CellFormat(0, 5, "Возможные направления работы", "", 1, "L", false, 0, "")
	pdf.SetFont(fontFamily, "", 8.8)
	pdf.SetTextColor(51, 65, 85)
	pdf.MultiCell(0, 4.5, "- "+strings.Join(roles, "\n- "), "", "L", false)
	pdf.Ln(1)
}

func drawDataNotes(pdf *fpdf.Fpdf, in Input) {
	ensureSpace(pdf, 30)
	text := "Найденные должности - возможные направления работы, а не гарантия трудоустройства. " +
		"Зарплатная оценка построена по текущим опубликованным вакансиям и не является прогнозом дохода после выпуска."
	switch in.Snapshot.Mode {
	case vacancies.ModeTest:
		text = "ВНИМАНИЕ: отчёт построен на тестовых, а не живых данных. " + text
	case vacancies.ModeCache:
		text = "Использован кэшированный срез; дата получения указана ниже. " + text
	}
	pdf.SetFillColor(255, 248, 230)
	pdf.SetDrawColor(245, 194, 66)
	y := pdf.GetY()
	innerWidth := pageWidthMM - leftMarginMM - rightMarginMM - 10
	pdf.SetFont(fontFamily, "", 8.7)
	height := linesHeight(pdf, cleanText(text), innerWidth, 4.4) + 8
	pdf.Rect(leftMarginMM, y, pageWidthMM-leftMarginMM-rightMarginMM, height, "DF")
	pdf.SetXY(leftMarginMM+5, y+4)
	pdf.SetTextColor(111, 78, 15)
	pdf.MultiCell(innerWidth, 4.4, cleanText(text), "", "L", false)
	pdf.SetY(y + height + 3)

	if source := cleanText(in.Snapshot.Source); source != "" {
		pdf.SetFont(fontFamily, "", 8)
		pdf.SetTextColor(71, 85, 105)
		fetched := ""
		if !in.Snapshot.FetchedAt.IsZero() {
			fetched = ", срез от " + in.Snapshot.FetchedAt.Format("02.01.2006 15:04")
		}
		pdf.MultiCell(0, 4.2, "Источник данных: "+source+fetched+".", "", "L", false)
	}
	for _, warning := range uniqueNonEmpty(in.Snapshot.Warnings, 5) {
		pdf.SetTextColor(180, 83, 9)
		pdf.MultiCell(0, 4.2, "Ограничение: "+truncate(cleanText(warning), 260), "", "L", false)
	}
}

func renderVacancy(pdf *fpdf.Fpdf, number int, vacancy vacancies.Vacancy) {
	innerWidth := pageWidthMM - leftMarginMM - rightMarginMM - 10
	card := fitVacancyCard(pdf, vacancyCardData(vacancy), innerWidth)
	height := vacancyCardHeight(pdf, card, innerWidth)
	ensureSpace(pdf, height+5)

	y := pdf.GetY()
	pdf.SetFillColor(249, 251, 253)
	pdf.SetDrawColor(218, 226, 235)
	pdf.Rect(leftMarginMM, y, pageWidthMM-leftMarginMM-rightMarginMM, height, "DF")
	pdf.SetXY(leftMarginMM+5, y+4)

	pdf.SetFont(fontFamily, "B", 11)
	pdf.SetTextColor(28, 47, 68)
	pdf.MultiCell(innerWidth, 5.4, fmt.Sprintf("%d. %s", number, card.title), "", "L", false)
	pdf.SetFont(fontFamily, "", 8.8)
	pdf.SetTextColor(71, 85, 105)
	pdf.MultiCell(innerWidth, 4.4, card.meta, "", "L", false)

	writeField(pdf, "Зарплата", card.salary, innerWidth)
	writeField(pdf, "Навыки", card.skills, innerWidth)
	writeField(pdf, "Требования", card.requirements, innerWidth)
	writeField(pdf, "Обязанности", card.duties, innerWidth)

	if card.url != "" {
		pdf.SetFont(fontFamily, "B", 8.8)
		pdf.SetTextColor(13, 116, 144)
		pdf.CellFormat(innerWidth, 5, "Открыть вакансию на сайте \"Работа России\"", "", 1, "L", false, 0, card.url)
	}
	pdf.SetY(y + height + 5)
}

func fitVacancyCard(pdf *fpdf.Fpdf, card cardData, width float64) cardData {
	pdf.SetFont(fontFamily, "B", 11)
	card.title = fitTextLines(pdf, card.title, width, 4)
	pdf.SetFont(fontFamily, "", 8.8)
	card.meta = fitTextLines(pdf, card.meta, width, 4)
	pdf.SetFont(fontFamily, "", 8.5)
	card.salary = fitTextLines(pdf, card.salary, width, 2)
	card.skills = fitTextLines(pdf, card.skills, width, 5)
	card.requirements = fitTextLines(pdf, card.requirements, width, 12)
	card.duties = fitTextLines(pdf, card.duties, width, 12)
	return card
}

type cardData struct {
	title        string
	meta         string
	salary       string
	skills       string
	requirements string
	duties       string
	url          string
}

func vacancyCardData(v vacancies.Vacancy) cardData {
	metaParts := []string{valueOrNoData(v.Employer)}
	location := firstNonEmpty(v.Location, v.Region)
	if location != "" {
		metaParts = append(metaParts, location)
	}
	if !v.PublishedAt.IsZero() {
		metaParts = append(metaParts, "опубликовано "+v.PublishedAt.Format("02.01.2006"))
	}
	if strings.TrimSpace(v.Employment) != "" {
		metaParts = append(metaParts, v.Employment)
	}
	if strings.TrimSpace(v.Schedule) != "" {
		metaParts = append(metaParts, v.Schedule)
	}

	skills := make([]string, 0, len(v.Skills))
	seenSkills := make(map[string]struct{}, len(v.Skills))
	for _, skill := range v.Skills {
		name := cleanText(skill.Name)
		key := strings.ToLower(name)
		if name == "" {
			continue
		}
		if _, exists := seenSkills[key]; exists {
			continue
		}
		seenSkills[key] = struct{}{}
		skills = append(skills, name)
		if len(skills) == 12 {
			break
		}
	}
	sort.Strings(skills)

	requirements := strings.Join(uniqueNonEmpty([]string{v.Education, v.Requirements}, 2), ". ")
	return cardData{
		title:        truncate(valueOrNoData(v.Title), 180),
		meta:         truncate(cleanText(strings.Join(metaParts, " | ")), 360),
		salary:       formatRange(v.SalaryFrom, v.SalaryTo, v.Currency),
		skills:       truncate(valueOrNoData(strings.Join(skills, ", ")), 420),
		requirements: truncate(valueOrNoData(requirements), 900),
		duties:       truncate(valueOrNoData(v.Duties), 800),
		url:          safeVacancyURL(v.URL),
	}
}

func vacancyCardHeight(pdf *fpdf.Fpdf, card cardData, width float64) float64 {
	height := 8.0
	pdf.SetFont(fontFamily, "B", 11)
	height += linesHeight(pdf, card.title, width, 5.4)
	pdf.SetFont(fontFamily, "", 8.8)
	height += linesHeight(pdf, card.meta, width, 4.4)
	for _, value := range []string{card.salary, card.skills, card.requirements, card.duties} {
		height += 4.5
		height += linesHeight(pdf, value, width, 4.2)
	}
	if card.url != "" {
		height += 6
	}
	return height + 4
}

func writeField(pdf *fpdf.Fpdf, label, value string, width float64) {
	pdf.SetFont(fontFamily, "B", 8.5)
	pdf.SetTextColor(28, 47, 68)
	pdf.CellFormat(width, 4.5, label, "", 1, "L", false, 0, "")
	pdf.SetFont(fontFamily, "", 8.5)
	pdf.SetTextColor(51, 65, 85)
	pdf.MultiCell(width, 4.2, valueOrNoData(value), "", "L", false)
}

func sectionTitle(pdf *fpdf.Fpdf, title string) {
	ensureSpace(pdf, 12)
	pdf.SetFont(fontFamily, "B", 14)
	pdf.SetTextColor(28, 47, 68)
	pdf.CellFormat(0, 8, cleanText(title), "", 1, "L", false, 0, "")
	pdf.SetDrawColor(13, 148, 136)
	y := pdf.GetY()
	pdf.Line(leftMarginMM, y, leftMarginMM+28, y)
	pdf.Ln(4)
}

func ensureSpace(pdf *fpdf.Fpdf, needed float64) {
	if pdf.GetY()+needed > bottomLimitMM {
		pdf.AddPage()
	}
}

func linesHeight(pdf *fpdf.Fpdf, text string, width, lineHeight float64) float64 {
	if strings.TrimSpace(text) == "" {
		text = "Нет данных"
	}
	lineCount := 0
	for _, paragraph := range strings.Split(text, "\n") {
		lines := pdf.SplitText(paragraph, width)
		if len(lines) == 0 {
			lineCount++
		} else {
			lineCount += len(lines)
		}
	}
	return float64(lineCount) * lineHeight
}

func fitTextLines(pdf *fpdf.Fpdf, text string, width float64, maxLines int) string {
	text = valueOrNoData(text)
	if maxLines <= 0 || wrappedLineCount(pdf, text, width) <= maxLines {
		return text
	}

	runes := []rune(text)
	best := "..."
	low, high := 0, len(runes)-1
	for low <= high {
		mid := low + (high-low)/2
		candidate := strings.TrimSpace(string(runes[:mid])) + "..."
		if wrappedLineCount(pdf, candidate, width) <= maxLines {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best
}

func wrappedLineCount(pdf *fpdf.Fpdf, text string, width float64) int {
	count := 0
	for _, paragraph := range strings.Split(valueOrNoData(text), "\n") {
		lines := pdf.SplitText(paragraph, width)
		if len(lines) == 0 {
			count++
		} else {
			count += len(lines)
		}
	}
	return count
}

func countEmployers(items []vacancies.Vacancy) int {
	seen := make(map[string]struct{})
	for _, item := range items {
		name := strings.ToLower(cleanText(item.Employer))
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	return len(seen)
}

func formatRange(from, to *int64, currency string) string {
	switch {
	case from != nil && to != nil:
		if *from == *to {
			return formatMoney(*from, currency)
		}
		return fmt.Sprintf("от %s до %s", formatMoney(*from, currency), formatMoney(*to, currency))
	case from != nil:
		return "от " + formatMoney(*from, currency)
	case to != nil:
		return "до " + formatMoney(*to, currency)
	default:
		return "Нет данных"
	}
}

func formatMoney(value int64, currency string) string {
	negative := value < 0
	if negative {
		value = -value
	}
	digits := fmt.Sprintf("%d", value)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + " " + digits[i:]
	}
	if negative {
		digits = "-" + digits
	}
	return digits + " " + currencyLabel(currency)
}

func currencyLabel(currency string) string {
	normalized := strings.ToLower(cleanText(currency))
	if normalized == "" || strings.Contains(normalized, "руб") || normalized == "rub" || normalized == "rur" {
		return "руб."
	}
	return cleanText(currency)
}

func sourceName(source string) string {
	if cleanText(source) == "" {
		return "Работа России"
	}
	return source
}

func safeVacancyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "trudvsem.ru" && !strings.HasSuffix(host, ".trudvsem.ru") {
		return ""
	}
	return parsed.String()
}

func reportFilename(programCode string, generatedAt time.Time) string {
	var b strings.Builder
	for _, r := range programCode {
		switch {
		case unicode.IsDigit(r), unicode.IsLetter(r) && r <= unicode.MaxASCII:
			b.WriteRune(unicode.ToLower(r))
		case r == '.', r == '-', r == '_':
			b.WriteByte('-')
		}
	}
	code := strings.Trim(b.String(), "-")
	if code == "" {
		code = "program"
	}
	return fmt.Sprintf("skillgap_%s_%s.pdf", code, generatedAt.Format("2006-01-02"))
}

func cleanText(value string) string {
	value = html.UnescapeString(value)
	value = tagPattern.ReplaceAllString(value, " ")
	value = strings.Map(func(r rune) rune {
		// go-pdf/fpdf v0.9.0 indexes UTF-8 font widths by BMP code point.
		// Non-BMP input (commonly emoji in vacancy descriptions) would otherwise
		// panic inside SplitText and terminate the process from a worker goroutine.
		if r > '\uFFFF' {
			return -1
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.NewReplacer(
		"\u00a0", " ",
		"\u2010", "-",
		"\u2011", "-",
		"\u2012", "-",
		"\u2013", "-",
		"\u2014", "-",
		"\u2212", "-",
	).Replace(value)
	return strings.TrimSpace(spacePattern.ReplaceAllString(value, " "))
}

func truncate(value string, maxRunes int) string {
	value = cleanText(value)
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return strings.TrimSpace(string(runes[:maxRunes-3])) + "..."
}

func valueOrNoData(value string) string {
	value = cleanText(value)
	if value == "" {
		return "Нет данных"
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if clean := cleanText(value); clean != "" {
			return clean
		}
	}
	return ""
}

func uniqueNonEmpty(values []string, limit int) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = cleanText(value)
		key := strings.ToLower(value)
		if value == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
