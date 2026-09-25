package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
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
	FGOSRequisites string  `json:"fgos_requisites"`
	FGOSYear       int     `json:"fgos_year"`
	FGOSStatus     string  `json:"fgos_status"`
	FGOSURL        string  `json:"fgos_url"`
	POPStatus      *string `json:"pop_status"`
	POPURL         *string `json:"pop_url"`
	AdmissionEnds  string  `json:"admission_ends"`
}

// Program — запись справочника СПО.
type Program struct {
	Code                 string         `json:"-"`
	ProgramName          string         `json:"program_name"`
	ProgramType          string         `json:"program_type"`
	UGPS                 string         `json:"ugps"`
	IsCurrent            bool           `json:"is_current"`
	Qualifications       []string       `json:"qualifications"`
	WorkRoles            []WorkRole     `json:"work_roles"`
	VacancyQueries       []string       `json:"vacancy_queries"`
	RequiresManualReview bool           `json:"requires_manual_review"`
	ActiveStandard       ActiveStandard `json:"active_standard"`
}

type fileRoot struct {
	Programs map[string]Program `json:"programs"`
}

// Catalog — загруженный справочник программ СПО.
type Catalog struct {
	programs map[string]*Program
	ordered  []*Program
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
	}
	for code, prog := range root.Programs {
		p := prog
		p.Code = code
		c.programs[code] = &p
		c.ordered = append(c.ordered, &p)
	}
	sort.Slice(c.ordered, func(i, j int) bool {
		return c.ordered[i].Code < c.ordered[j].Code
	})
	return c, nil
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
	q := strings.TrimSpace(query)
	if q == "" || limit <= 0 {
		return nil
	}

	if p, ok := c.Get(q); ok {
		return []*Program{p}
	}

	norm := normalizeSearch(q)
	var (
		current []*Program
		other   []*Program
	)
	for _, p := range c.ordered {
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
	if p.ActiveStandard.POPURL != nil && *p.ActiveStandard.POPURL != "" {
		return *p.ActiveStandard.POPURL
	}
	return p.ActiveStandard.FGOSURL
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
