package catalog

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

var (
	programCodePattern   = regexp.MustCompile(`^\d{2}\.\d{2}\.\d{2}$`)
	schemaVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
)

// WorkRole — стартовая должность и поисковые запросы к вакансиям.
type WorkRole struct {
	CanonicalTitle  string   `json:"canonical_title"`
	SourceBasis     string   `json:"source_basis"`
	VacancyQueries  []string `json:"vacancy_queries"`
	QueryConfidence string   `json:"query_confidence"`
	ReviewStatus    string   `json:"review_status"`
}

// ActiveStandard — актуальный ФГОС/ПОП программы.
type ActiveStandard struct {
	FGOSRequisites       string  `json:"fgos_requisites"`
	FGOSYear             int     `json:"fgos_year"`
	FGOSStatus           string  `json:"fgos_status"`
	FGOSURL              string  `json:"fgos_url"`
	POPStatus            *string `json:"pop_status"`
	POPURL               *string `json:"pop_url"`
	AdmissionEnds        string  `json:"admission_ends"`
	AdmissionEndYear     *int    `json:"admission_end_year"`
	DurationAfterGrade9  string  `json:"duration_after_grade_9"`
	DurationAfterGrade11 string  `json:"duration_after_grade_11"`
	GIAForm              string  `json:"gia_form"`
	QualificationRaw     string  `json:"qualification_raw"`
	Comment              *string `json:"comment"`
}

// Program — запись справочника СПО.
type Program struct {
	Code                 string           `json:"-"`
	ProgramName          string           `json:"program_name"`
	ProgramType          string           `json:"program_type"`
	UGPS                 string           `json:"ugps"`
	IsCurrent            bool             `json:"is_current"`
	Qualifications       []string         `json:"qualifications"`
	WorkRoles            []WorkRole       `json:"work_roles"`
	VacancyQueries       []string         `json:"vacancy_queries"`
	RequiresManualReview bool             `json:"requires_manual_review"`
	ActiveStandard       ActiveStandard   `json:"active_standard"`
	AllKnownStandards    []ActiveStandard `json:"all_known_standards"`
}

type fileRoot struct {
	Metadata CatalogMetadata    `json:"metadata"`
	Programs map[string]Program `json:"programs"`
}

type CatalogMetadata struct {
	SchemaVersion                string `json:"schema_version"`
	GeneratedOn                  string `json:"generated_on"`
	SourceName                   string `json:"source_name"`
	SourceURL                    string `json:"source_url"`
	SourceRows                   int    `json:"source_rows"`
	UniqueProgramCodes           int    `json:"unique_program_codes"`
	CurrentProgramCodes          int    `json:"current_program_codes"`
	ProgramsRequiringQueryReview int    `json:"programs_requiring_query_review"`
	Coverage                     string `json:"coverage"`
}

type ValidationSeverity string

const (
	SeverityError   ValidationSeverity = "error"
	SeverityWarning ValidationSeverity = "warning"
)

type ValidationIssue struct {
	Severity ValidationSeverity
	Code     string
	Field    string
	Message  string
}

type ValidationReport struct {
	Issues []ValidationIssue
}

func (r ValidationReport) ErrorCount() int {
	count := 0
	for _, issue := range r.Issues {
		if issue.Severity == SeverityError {
			count++
		}
	}
	return count
}

func (r ValidationReport) WarningCount() int {
	return len(r.Issues) - r.ErrorCount()
}

type ReviewPolicy string

const (
	ReviewPolicyAll     ReviewPolicy = "all"
	ReviewPolicyCurrent ReviewPolicy = "current"
	ReviewPolicyPilot   ReviewPolicy = "pilot"
)

type SourceDocument struct {
	Kind   string
	Status string
	URL    string
}

// Catalog — загруженный справочник программ СПО.
type Catalog struct {
	programs   map[string]*Program
	ordered    []*Program
	metadata   CatalogMetadata
	validation ValidationReport
}

// Load читает spo_program_vacancy_map.json с диска.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	var root fileRoot
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	if len(root.Programs) == 0 {
		return nil, fmt.Errorf("catalog has no programs")
	}

	c := &Catalog{
		programs: make(map[string]*Program, len(root.Programs)),
		ordered:  make([]*Program, 0, len(root.Programs)),
		metadata: root.Metadata,
	}
	for code, prog := range root.Programs {
		p := prog
		p.Code = code
		c.programs[code] = &p
		c.ordered = append(c.ordered, &p)
	}
	c.validation = c.validate()
	if c.validation.ErrorCount() > 0 {
		first := ValidationIssue{}
		for _, issue := range c.validation.Issues {
			if issue.Severity == SeverityError {
				first = issue
				break
			}
		}
		return nil, fmt.Errorf("catalog validation failed (%d errors): %s %s: %s", c.validation.ErrorCount(), first.Code, first.Field, first.Message)
	}
	sort.Slice(c.ordered, func(i, j int) bool {
		return c.ordered[i].Code < c.ordered[j].Code
	})
	return c, nil
}

func (c *Catalog) Metadata() CatalogMetadata {
	return c.metadata
}

func (c *Catalog) ValidationReport() ValidationReport {
	return ValidationReport{Issues: append([]ValidationIssue(nil), c.validation.Issues...)}
}

// Get возвращает программу по коду.
func (c *Catalog) Get(code string) (*Program, bool) {
	p, ok := c.programs[normalizeCode(code)]
	return p, ok
}

// Len — число программ в справочнике.
func (c *Catalog) Len() int {
	return len(c.programs)
}

// Search ищет программы по коду или подстроке названия.
// Точное совпадение кода имеет приоритет; иначе — подстрока в названии (без учёта регистра).
func (c *Catalog) Search(query string, limit int) []*Program {
	return c.SearchWithPolicy(query, limit, ReviewPolicyAll)
}

// SearchWithPolicy allows production to expose only current or human-reviewed
// pilot entries without maintaining a second catalogue.
func (c *Catalog) SearchWithPolicy(query string, limit int, policy ReviewPolicy) []*Program {
	q := strings.TrimSpace(query)
	if q == "" || limit <= 0 {
		return nil
	}

	if p, ok := c.Get(q); ok && ProgramAllowed(p, policy) {
		return []*Program{p}
	}

	norm := normalizeSearch(q)
	var (
		current []*Program
		other   []*Program
	)
	for _, p := range c.ordered {
		if !ProgramAllowed(p, policy) {
			continue
		}
		nameNorm := normalizeSearch(p.ProgramName)
		codeNorm := normalizeSearch(p.Code)
		if !strings.Contains(nameNorm, norm) && !strings.Contains(codeNorm, norm) {
			continue
		}
		if p.IsCurrent {
			current = append(current, p)
		} else {
			other = append(other, p)
		}
	}

	out := make([]*Program, 0, limit)
	out = append(out, current...)
	out = append(out, other...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (c *Catalog) PilotPrograms() []*Program {
	result := make([]*Program, 0)
	for _, program := range c.ordered {
		if ProgramAllowed(program, ReviewPolicyPilot) {
			result = append(result, program)
		}
	}
	return result
}

// ProgramAllowed reports whether a program may be exposed under the selected
// review policy. Unknown policies fail closed to the reviewed pilot subset.
func ProgramAllowed(program *Program, policy ReviewPolicy) bool {
	if program == nil {
		return false
	}
	switch policy {
	case ReviewPolicyAll:
		return true
	case ReviewPolicyPilot:
		return program.ProductionReady()
	case ReviewPolicyCurrent:
		return program.IsCurrent
	default:
		return program.ProductionReady()
	}
}

func (p *Program) ProductionReady() bool {
	if p == nil || !p.IsCurrent || p.RequiresManualReview || p.SourceURL() == "" || len(p.Qualifications) == 0 {
		return false
	}
	if len(p.WorkRoles) == 0 {
		return false
	}
	for _, role := range p.WorkRoles {
		if role.ReviewStatus != "reviewed_for_pilot" || role.QueryConfidence != "curated" || len(role.VacancyQueries) == 0 {
			return false
		}
	}
	return true
}

// RolesForQualification возвращает роли для выбранной квалификации.
// Если совпадений по названию нет — все роли программы.
func (p *Program) RolesForQualification(qualification string) []WorkRole {
	if qualification == "" || len(p.WorkRoles) == 0 {
		return p.WorkRoles
	}
	var matched []WorkRole
	qNorm := normalizeSearch(qualification)
	for _, role := range p.WorkRoles {
		if normalizeSearch(role.CanonicalTitle) == qNorm {
			matched = append(matched, role)
		}
	}
	if len(matched) == 0 {
		return p.WorkRoles
	}
	return matched
}

// SourceURL — ссылка на федеральный источник (ПОП, иначе ФГОС).
func (p *Program) SourceURL() string {
	return p.SourceDocument().URL
}

func (p *Program) SourceDocument() SourceDocument {
	if p == nil {
		return SourceDocument{}
	}
	if p.ActiveStandard.POPURL != nil && validFederalDocumentURL(*p.ActiveStandard.POPURL) && isApprovedStatus(valueOrEmpty(p.ActiveStandard.POPStatus)) {
		return SourceDocument{Kind: "ПОП", Status: valueOrEmpty(p.ActiveStandard.POPStatus), URL: strings.TrimSpace(*p.ActiveStandard.POPURL)}
	}
	return p.FGOSDocument()
}

// FGOSDocument returns only an approved/active FGOS. The same gate is used
// for a primary source and for fallback after an unavailable POP.
func (p *Program) FGOSDocument() SourceDocument {
	if p == nil || !validFederalDocumentURL(p.ActiveStandard.FGOSURL) || !isApprovedStatus(p.ActiveStandard.FGOSStatus) {
		return SourceDocument{}
	}
	return SourceDocument{Kind: "ФГОС", Status: p.ActiveStandard.FGOSStatus, URL: strings.TrimSpace(p.ActiveStandard.FGOSURL)}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func isApprovedStatus(status string) bool {
	status = strings.NewReplacer("ё", "е", "Ё", "Е").Replace(strings.ToLower(strings.TrimSpace(status)))
	for _, denied := range []string{
		"не утвержден", "не принят", "не действует", "недейств", "проект",
		"утратил", "отменен", "аннулирован", "прекратил действие",
	} {
		if strings.Contains(status, denied) {
			return false
		}
	}
	if status == "" {
		return false
	}
	return strings.Contains(status, "утвержден") || strings.Contains(status, "принят") || strings.Contains(status, "действующий")
}

func validHTTPSURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && (parsed.Port() == "" || parsed.Port() == "443")
}

func validFederalDocumentURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) == 0 || len(trimmed) > 2048 {
		return false
	}
	parsed, err := url.Parse(trimmed)
	return err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Hostname(), "spolab.firpo.ru") &&
		parsed.User == nil && (parsed.Port() == "" || parsed.Port() == "443")
}

func (c *Catalog) validate() ValidationReport {
	var report ValidationReport
	add := func(severity ValidationSeverity, code, field, message string) {
		report.Issues = append(report.Issues, ValidationIssue{Severity: severity, Code: code, Field: field, Message: message})
	}
	if strings.TrimSpace(c.metadata.SchemaVersion) == "" {
		add(SeverityWarning, "_catalog", "metadata.schema_version", "schema version is missing")
	} else if !schemaVersionPattern.MatchString(c.metadata.SchemaVersion) {
		add(SeverityError, "_catalog", "metadata.schema_version", "must use semantic X.Y.Z form")
	}
	if _, err := time.Parse("2006-01-02", c.metadata.GeneratedOn); err != nil {
		add(SeverityError, "_catalog", "metadata.generated_on", "must be an ISO date")
	}
	if strings.TrimSpace(c.metadata.SourceName) == "" {
		add(SeverityError, "_catalog", "metadata.source_name", "is required")
	}
	if strings.TrimSpace(c.metadata.SourceURL) != "" && !validHTTPSURL(c.metadata.SourceURL) {
		add(SeverityError, "_catalog", "metadata.source_url", "must be an absolute HTTPS URL")
	}
	if c.metadata.SourceRows < len(c.programs) {
		add(SeverityError, "_catalog", "metadata.source_rows", "must be at least the programs count")
	}
	if c.metadata.UniqueProgramCodes > 0 && c.metadata.UniqueProgramCodes != len(c.programs) {
		add(SeverityError, "_catalog", "metadata.unique_program_codes", "does not match programs count")
	}
	currentPrograms := 0
	programsRequiringReview := 0
	for _, p := range c.ordered {
		if p.IsCurrent {
			currentPrograms++
		}
		if p.RequiresManualReview {
			programsRequiringReview++
		}
	}
	if c.metadata.CurrentProgramCodes != currentPrograms {
		add(SeverityError, "_catalog", "metadata.current_program_codes", "does not match current programs count")
	}
	if c.metadata.ProgramsRequiringQueryReview != programsRequiringReview {
		add(SeverityError, "_catalog", "metadata.programs_requiring_query_review", "does not match programs requiring review")
	}
	if strings.TrimSpace(c.metadata.Coverage) == "" {
		add(SeverityWarning, "_catalog", "metadata.coverage", "coverage description is missing")
	}
	for _, p := range c.ordered {
		if !programCodePattern.MatchString(p.Code) {
			add(SeverityError, p.Code, "code", "must match XX.XX.XX")
		}
		if strings.TrimSpace(p.ProgramName) == "" {
			add(SeverityError, p.Code, "program_name", "is required")
		}
		if p.ProgramType != "профессия" && p.ProgramType != "специальность" {
			add(SeverityError, p.Code, "program_type", "unsupported value")
		}
		if len(p.Qualifications) == 0 {
			add(SeverityWarning, p.Code, "qualifications", "no confirmed qualification")
		}
		qualifications := make(map[string]struct{}, len(p.Qualifications))
		for index, qualification := range p.Qualifications {
			key := normalizeSearch(qualification)
			if key == "" {
				add(SeverityError, p.Code, fmt.Sprintf("qualifications[%d]", index), "qualification must not be blank")
				continue
			}
			if _, duplicate := qualifications[key]; duplicate {
				add(SeverityError, p.Code, fmt.Sprintf("qualifications[%d]", index), "duplicate qualification")
			}
			qualifications[key] = struct{}{}
		}
		if len(p.WorkRoles) == 0 || len(p.VacancyQueries) == 0 {
			add(SeverityError, p.Code, "work_roles", "roles and vacancy queries are required")
		}
		programQueries := make(map[string]struct{}, len(p.VacancyQueries))
		for index, query := range p.VacancyQueries {
			key := normalizeSearch(query)
			if key == "" {
				add(SeverityError, p.Code, fmt.Sprintf("vacancy_queries[%d]", index), "query must not be blank")
				continue
			}
			if _, duplicate := programQueries[key]; duplicate {
				add(SeverityError, p.Code, fmt.Sprintf("vacancy_queries[%d]", index), "duplicate query")
			}
			programQueries[key] = struct{}{}
		}
		needsReview := false
		for index, role := range p.WorkRoles {
			field := fmt.Sprintf("work_roles[%d]", index)
			if strings.TrimSpace(role.CanonicalTitle) == "" || len(role.VacancyQueries) == 0 {
				add(SeverityError, p.Code, field, "canonical title and at least one query are required")
			}
			if role.SourceBasis != "official_qualification" && role.SourceBasis != "program_name_fallback" {
				add(SeverityError, p.Code, field+".source_basis", "unsupported value")
			}
			if role.ReviewStatus != "requires_manual_review" && role.ReviewStatus != "reviewed_for_pilot" {
				add(SeverityError, p.Code, field+".review_status", "unsupported value")
			}
			if role.ReviewStatus != "reviewed_for_pilot" || role.QueryConfidence != "curated" {
				needsReview = true
			}
			if role.QueryConfidence != "low" && role.QueryConfidence != "medium" && role.QueryConfidence != "curated" {
				add(SeverityError, p.Code, field+".query_confidence", "unsupported value")
			}
			roleQueries := make(map[string]struct{}, len(role.VacancyQueries))
			for queryIndex, query := range role.VacancyQueries {
				key := normalizeSearch(query)
				if key == "" {
					add(SeverityError, p.Code, fmt.Sprintf("%s.vacancy_queries[%d]", field, queryIndex), "query must not be blank")
					continue
				}
				if _, duplicate := roleQueries[key]; duplicate {
					add(SeverityError, p.Code, fmt.Sprintf("%s.vacancy_queries[%d]", field, queryIndex), "duplicate query")
				}
				roleQueries[key] = struct{}{}
				if _, exists := programQueries[key]; !exists {
					add(SeverityError, p.Code, fmt.Sprintf("%s.vacancy_queries[%d]", field, queryIndex), "query is missing from program-level vacancy_queries")
				}
			}
		}
		if needsReview != p.RequiresManualReview {
			add(SeverityError, p.Code, "requires_manual_review", "does not match role review status and confidence")
		}
		if p.RequiresManualReview {
			add(SeverityWarning, p.Code, "work_roles", "vacancy query model is not approved for production")
		}
		if p.ActiveStandard.POPURL != nil && valueOrEmpty(p.ActiveStandard.POPStatus) == "" {
			add(SeverityWarning, p.Code, "active_standard.pop_status", "POP URL has no status")
		}
		if p.ActiveStandard.POPURL != nil && !validHTTPSURL(*p.ActiveStandard.POPURL) {
			add(SeverityError, p.Code, "active_standard.pop_url", "must be an absolute HTTPS URL")
		}
		if strings.TrimSpace(p.ActiveStandard.FGOSURL) != "" && !validHTTPSURL(p.ActiveStandard.FGOSURL) {
			add(SeverityError, p.Code, "active_standard.fgos_url", "must be an absolute HTTPS URL")
		}
		if p.SourceURL() == "" {
			add(SeverityWarning, p.Code, "active_standard", "no usable federal source")
		}
		if p.IsCurrent && !isApprovedStatus(p.ActiveStandard.FGOSStatus) {
			add(SeverityWarning, p.Code, "active_standard.fgos_status", "current program has no approved active standard")
		}
		if p.ActiveStandard.FGOSYear != 0 && (p.ActiveStandard.FGOSYear < 1900 || p.ActiveStandard.FGOSYear > 2200) {
			add(SeverityError, p.Code, "active_standard.fgos_year", "year is outside the supported range")
		}
		if len(p.AllKnownStandards) == 0 {
			add(SeverityError, p.Code, "all_known_standards", "at least one source record is required")
		}
		for index, standard := range p.AllKnownStandards {
			field := fmt.Sprintf("all_known_standards[%d]", index)
			if standard.POPURL != nil && !validHTTPSURL(*standard.POPURL) {
				add(SeverityError, p.Code, field+".pop_url", "must be an absolute HTTPS URL")
			}
			if strings.TrimSpace(standard.FGOSURL) != "" && !validHTTPSURL(standard.FGOSURL) {
				add(SeverityError, p.Code, field+".fgos_url", "must be an absolute HTTPS URL")
			}
			if standard.POPURL != nil && valueOrEmpty(standard.POPStatus) == "" {
				add(SeverityWarning, p.Code, field+".pop_status", "POP URL has no status")
			}
			if standard.FGOSYear != 0 && (standard.FGOSYear < 1900 || standard.FGOSYear > 2200) {
				add(SeverityError, p.Code, field+".fgos_year", "year is outside the supported range")
			}
		}
	}
	return report
}

func normalizeCode(code string) string {
	return strings.TrimSpace(code)
}

func normalizeSearch(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) || r == '-' || r == '—' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
