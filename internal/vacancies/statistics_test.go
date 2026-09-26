package vacancies

import "testing"

func TestCalculateStatisticsMedianRangesCoverageAndMarketCounts(t *testing.T) {
	t.Parallel()

	items := []Vacancy{
		{
			Employer:        "Employer A",
			SalaryFrom:      salaryValue(40000),
			SalaryTo:        salaryValue(60000),
			Currency:        "RUB",
			ExperienceYears: experienceValue(0),
			Schedule:        "Дистанционная работа",
			Skills:          []Skill{{Name: "Go"}, {Name: "Go"}, {Name: "SQL"}},
		},
		{
			Employer:        " employer a ",
			SalaryFrom:      salaryValue(50000),
			Currency:        "RUB",
			ExperienceYears: experienceValue(1),
			Skills:          []Skill{{Name: "go"}},
		},
		{
			Employer:        "Employer B",
			SalaryTo:        salaryValue(70000),
			Currency:        "RUB",
			ExperienceYears: experienceValue(2),
			Employment:      "Удалённая занятость",
			Skills:          []Skill{{Name: "SQL"}},
		},
		{
			Employer:   "Employer C",
			SalaryFrom: salaryValue(80000),
			SalaryTo:   salaryValue(100000),
			Skills:     []Skill{{Name: "Go"}},
		},
		{
			Employer:        "Employer D",
			SalaryFrom:      salaryValue(110000),
			Currency:        "RUB",
			ExperienceYears: experienceValue(3),
			Skills:          []Skill{{Name: "Python"}},
		},
		{
			ExperienceYears: experienceValue(0),
			Skills:          []Skill{{Name: "Python"}, {Name: "  "}},
		},
	}

	got := CalculateStatistics(items)
	if got.VacancyCount != 6 {
		t.Errorf("VacancyCount = %d, want 6", got.VacancyCount)
	}
	if got.EmployerCount != 4 {
		t.Errorf("EmployerCount = %d, want 4", got.EmployerCount)
	}
	if got.SalaryCount != 5 || got.SalaryCoveragePercent != 83 {
		t.Errorf("salary count/coverage = %d/%d, want 5/83", got.SalaryCount, got.SalaryCoveragePercent)
	}
	assertInt64Pointer(t, "SalaryMedian", got.SalaryMedian, 70000)
	assertInt64Pointer(t, "SalaryMin", got.SalaryMin, 40000)
	assertInt64Pointer(t, "SalaryMax", got.SalaryMax, 110000)
	if got.Currency != "RUB" {
		t.Errorf("Currency = %q, want RUB", got.Currency)
	}
	if got.EntryLevelCount != 4 || got.EntryLevelPercent != 67 {
		t.Errorf("entry level count/percent = %d/%d, want 4/67", got.EntryLevelCount, got.EntryLevelPercent)
	}
	if got.RemoteCount != 2 || got.RemotePercent != 33 {
		t.Errorf("remote count/percent = %d/%d, want 2/33", got.RemoteCount, got.RemotePercent)
	}
	if got.SmallSample {
		t.Error("SmallSample = true, want false for 6 vacancies and 5 salaries")
	}

	wantSkills := []SkillFrequency{
		{Name: "Go", Count: 3, Percent: 50},
		{Name: "Python", Count: 2, Percent: 33},
		{Name: "SQL", Count: 2, Percent: 33},
	}
	if len(got.TopSkills) != len(wantSkills) {
		t.Fatalf("TopSkills = %#v, want %#v", got.TopSkills, wantSkills)
	}
	for i := range wantSkills {
		if got.TopSkills[i] != wantSkills[i] {
			t.Errorf("TopSkills[%d] = %+v, want %+v", i, got.TopSkills[i], wantSkills[i])
		}
	}
}

func TestCalculateStatisticsMedianOddAndEven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values []int64
		want   int64
	}{
		{name: "odd", values: []int64{100, 20, 10}, want: 20},
		{name: "even", values: []int64{40, 10, 30, 20}, want: 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := make([]Vacancy, 0, len(tt.values))
			for _, value := range tt.values {
				items = append(items, Vacancy{SalaryFrom: salaryValue(value), Currency: "RUB"})
			}
			got := CalculateStatistics(items)
			assertInt64Pointer(t, "SalaryMedian", got.SalaryMedian, tt.want)
		})
	}
}

func TestSalaryObservationHandlesRangesAndMissingValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from *int64
		to   *int64
		want int64
	}{
		{name: "range midpoint", from: salaryValue(40000), to: salaryValue(60000), want: 50000},
		{name: "from only", from: salaryValue(45000), want: 45000},
		{name: "to only", to: salaryValue(70000), want: 70000},
		{name: "no salary"},
		{name: "zero values", from: salaryValue(0), to: salaryValue(0)},
		{name: "negative values", from: salaryValue(-1), to: salaryValue(-2)},
		{name: "valid from invalid to", from: salaryValue(50000), to: salaryValue(0), want: 50000},
		{name: "invalid from valid to", from: salaryValue(0), to: salaryValue(90000), want: 90000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := salaryObservation(Vacancy{SalaryFrom: tt.from, SalaryTo: tt.to}); got != tt.want {
				t.Errorf("salaryObservation() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCalculateStatisticsWithNoSalaryData(t *testing.T) {
	t.Parallel()

	got := CalculateStatistics([]Vacancy{
		{Employer: "A", Skills: []Skill{{Name: "Go"}}},
		{Employer: "B", SalaryFrom: salaryValue(0)},
	})
	if got.SalaryCount != 0 || got.SalaryCoveragePercent != 0 {
		t.Errorf("salary count/coverage = %d/%d", got.SalaryCount, got.SalaryCoveragePercent)
	}
	if got.SalaryMedian != nil || got.SalaryMin != nil || got.SalaryMax != nil {
		t.Errorf("salary values must be nil: %+v", got)
	}
	if got.Currency != "RUB" {
		t.Errorf("Currency = %q, want fallback RUB", got.Currency)
	}
	if !got.SmallSample {
		t.Error("SmallSample = false, want true")
	}

	empty := CalculateStatistics(nil)
	if empty.VacancyCount != 0 || !empty.SmallSample {
		t.Errorf("empty statistics = %+v", empty)
	}
	if empty.TopSkills != nil || empty.SalaryMedian != nil {
		t.Errorf("empty statistics should not manufacture data: %+v", empty)
	}
}

func TestCalculateStatisticsDoesNotMixCurrencies(t *testing.T) {
	t.Parallel()

	items := []Vacancy{
		{SalaryFrom: salaryValue(100000), Currency: "RUB"},
		{SalaryFrom: salaryValue(200000), Currency: "RUB"},
		{SalaryFrom: salaryValue(1000), Currency: "USD"},
		{SalaryFrom: salaryValue(3000), Currency: "USD"},
		{SalaryFrom: salaryValue(5000), Currency: "USD"},
	}
	got := CalculateStatistics(items)
	if got.Currency != "USD" {
		t.Fatalf("Currency = %q, want dominant USD", got.Currency)
	}
	if got.SalaryCount != 5 || got.SalaryCoveragePercent != 100 {
		t.Errorf("salary count/coverage = %d/%d, want 5/100", got.SalaryCount, got.SalaryCoveragePercent)
	}
	if got.SalarySampleCount != 3 {
		t.Errorf("SalarySampleCount = %d, want 3", got.SalarySampleCount)
	}
	assertInt64Pointer(t, "SalaryMedian", got.SalaryMedian, 3000)
	assertInt64Pointer(t, "SalaryMin", got.SalaryMin, 1000)
	assertInt64Pointer(t, "SalaryMax", got.SalaryMax, 5000)
}

func TestDominantCurrencyHasDeterministicTieBreak(t *testing.T) {
	t.Parallel()

	for i := 0; i < 50; i++ {
		got := dominantCurrency(map[string]int{"USD": 2, "EUR": 2, "RUB": 1})
		if got != "EUR" {
			t.Fatalf("dominantCurrency tie = %q, want EUR", got)
		}
	}
	if got := dominantCurrency(nil); got != "RUB" {
		t.Errorf("dominantCurrency(nil) = %q, want RUB", got)
	}
}

func TestEmployerCountPrefersStableCompanyCode(t *testing.T) {
	t.Parallel()

	got := CalculateStatistics([]Vacancy{
		{EmployerID: "company-1", Employer: "Одинаковое имя"},
		{EmployerID: "company-2", Employer: "Одинаковое имя"},
		{EmployerID: "company-1", Employer: "Другое написание"},
		{Employer: "Без идентификатора"},
	})
	if got.EmployerCount != 3 {
		t.Errorf("EmployerCount = %d, want 3", got.EmployerCount)
	}
}

func TestRemoteAndPercentHelpers(t *testing.T) {
	t.Parallel()

	for _, item := range []Vacancy{
		{Schedule: "Дистанционный режим"},
		{Employment: "Удаленная работа"},
		{Location: "Удалённо"},
	} {
		if !isRemote(item) {
			t.Errorf("isRemote(%+v) = false", item)
		}
	}
	if isRemote(Vacancy{Schedule: "Полный день", Location: "Москва"}) {
		t.Error("office vacancy detected as remote")
	}
	for _, tt := range []struct {
		value int
		total int
		want  int
	}{
		{value: 0, total: 0, want: 0},
		{value: 1, total: 3, want: 33},
		{value: 2, total: 3, want: 67},
		{value: 1, total: 6, want: 17},
	} {
		if got := percent(tt.value, tt.total); got != tt.want {
			t.Errorf("percent(%d, %d) = %d, want %d", tt.value, tt.total, got, tt.want)
		}
	}
}

func salaryValue(value int64) *int64 {
	return &value
}

func experienceValue(value int) *int {
	return &value
}

func assertInt64Pointer(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %d", name, got, want)
	}
}
