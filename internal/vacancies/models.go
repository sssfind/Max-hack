package vacancies

import (
	"context"
	"time"
)

const DefaultBaseURL = "https://opendata.trudvsem.ru/api/v1/vacancies"

type Mode string

const (
	ModeLive  Mode = "live"
	ModeCache Mode = "cache"
	ModeTest  Mode = "test"
)

// SearchRequest describes one market snapshot for a selected SPO program.
type SearchRequest struct {
	RegionCode            string
	RegionName            string
	ProgramCode           string
	Qualification         string
	Queries               []string
	QueryModelNeedsReview bool
}

type Skill struct {
	Name            string
	Evidence        string
	EvidenceField   string
	EvidenceExcerpt string
}

type EducationRequirement string

const (
	EducationUnknown             EducationRequirement = "unknown"
	EducationNotRequired         EducationRequirement = "not_required"
	EducationSecondaryGeneral    EducationRequirement = "secondary_general"
	EducationSecondaryVocational EducationRequirement = "secondary_vocational"
	EducationHigherOrVocational  EducationRequirement = "higher_or_vocational"
	EducationHigherOnly          EducationRequirement = "higher_only"
)

type Vacancy struct {
	ID              string
	Title           string
	Employer        string
	EmployerID      string
	Region          string
	Location        string
	Duties          string
	Requirements    string
	Education       string
	EducationClass  EducationRequirement
	Employment      string
	Schedule        string
	Currency        string
	SalaryText      string
	URL             string
	FoundByQuery    string
	RelevanceScore  int
	ExperienceYears *int
	Skills          []Skill
	SalaryFrom      *int64
	SalaryTo        *int64
	PublishedAt     time.Time
}

// SamplingStages makes the transformation from source results to the final
// report auditable. SourceTotal can include the same vacancy in several query
// result sets; Deduplicated and Included are unique vacancy counts.
type SamplingStages struct {
	SourceTotal             int
	Retrieved               int
	EntryLevelPassed        int
	ExperienceUnknownPassed int
	EducationPassed         int
	RelevantPassed          int
	RejectedHigherEducation int
	Deduplicated            int
	Included                int
}

type Snapshot struct {
	Vacancies []Vacancy
	Source    string
	FetchedAt time.Time
	Mode      Mode
	Partial   bool
	Warnings  []string
	Sampling  SamplingStages
}

type SkillFrequency struct {
	Name    string
	Count   int
	Percent int
}

type Statistics struct {
	VacancyCount               int
	EmployerCount              int
	SalaryCount                int
	SalarySampleCount          int
	SalaryCoveragePercent      int
	SalaryMedian               *int64
	SalaryMin                  *int64
	SalaryMax                  *int64
	Currency                   string
	EntryLevelCount            int
	EntryLevelPercent          int
	NoExperienceCount          int
	NoExperiencePercent        int
	ExperienceUnknownCount     int
	ExperienceUnknownPercent   int
	RemoteCount                int
	RemotePercent              int
	TopSkills                  []SkillFrequency
	SmallSample                bool
	SalaryReliable             bool
	SalaryUnknownCurrencyCount int
	SalaryMethod               string
}

type Analyzer interface {
	Analyze(context.Context, SearchRequest) (Snapshot, Statistics, error)
}
