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
	Name     string
	Evidence string
}

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
	Employment      string
	Schedule        string
	Currency        string
	URL             string
	FoundByQuery    string
	ExperienceYears *int
	Skills          []Skill
	SalaryFrom      *int64
	SalaryTo        *int64
	PublishedAt     time.Time
}

type Snapshot struct {
	Vacancies []Vacancy
	Source    string
	FetchedAt time.Time
	Mode      Mode
	Warnings  []string
}

type SkillFrequency struct {
	Name    string
	Count   int
	Percent int
}

type Statistics struct {
	VacancyCount          int
	EmployerCount         int
	SalaryCount           int
	SalarySampleCount     int
	SalaryCoveragePercent int
	SalaryMedian          *int64
	SalaryMin             *int64
	SalaryMax             *int64
	Currency              string
	EntryLevelCount       int
	EntryLevelPercent     int
	RemoteCount           int
	RemotePercent         int
	TopSkills             []SkillFrequency
	SmallSample           bool
}

type Analyzer interface {
	Analyze(context.Context, SearchRequest) (Snapshot, Statistics, error)
}
