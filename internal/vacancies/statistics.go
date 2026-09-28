package vacancies

import (
	"sort"
	"strings"
)

const MinimumSalarySample = 5

func CalculateStatistics(items []Vacancy) Statistics {
	stats := Statistics{
		VacancyCount: len(items),
		SalaryMethod: "Для диапазона используется его середина; для односторонней вилки — опубликованная граница. Валюты не смешиваются; агрегаты показываются минимум по 5 вакансиям. До/после налогов и период выплаты не преобразуются.",
	}
	if len(items) == 0 {
		stats.SmallSample = true
		return stats
	}

	employers := make(map[string]struct{}, len(items))
	currencyCounts := make(map[string]int)
	for _, item := range items {
		employer := strings.TrimSpace(item.EmployerID)
		if employer == "" {
			employer = strings.ToLower(strings.TrimSpace(item.Employer))
		} else {
			employer = "id:" + strings.ToLower(employer)
		}
		if employer != "" {
			employers[employer] = struct{}{}
		}
		if item.ExperienceYears == nil {
			stats.ExperienceUnknownCount++
		} else if *item.ExperienceYears <= 1 {
			stats.EntryLevelCount++
			if *item.ExperienceYears == 0 {
				stats.NoExperienceCount++
			}
		}
		if isRemote(item) {
			stats.RemoteCount++
		}
		if salaryObservation(item) > 0 {
			stats.SalaryCount++
			currency := strings.TrimSpace(item.Currency)
			if currency == "" {
				stats.SalaryUnknownCurrencyCount++
				continue
			}
			currencyCounts[currency]++
		}
	}
	stats.EmployerCount = len(employers)
	stats.EntryLevelPercent = percent(stats.EntryLevelCount, len(items))
	stats.NoExperiencePercent = percent(stats.NoExperienceCount, len(items))
	stats.ExperienceUnknownPercent = percent(stats.ExperienceUnknownCount, len(items))
	stats.RemotePercent = percent(stats.RemoteCount, len(items))
	stats.Currency = dominantCurrency(currencyCounts)

	var observations []int64
	for _, item := range items {
		currency := strings.TrimSpace(item.Currency)
		if stats.Currency == "" || currency == "" || currency != stats.Currency {
			continue
		}
		observation := salaryObservation(item)
		if observation <= 0 {
			continue
		}
		observations = append(observations, observation)
	}
	stats.SalarySampleCount = len(observations)
	stats.SalaryCoveragePercent = percent(stats.SalaryCount, len(items))
	stats.SalaryReliable = len(observations) >= MinimumSalarySample
	if stats.SalaryReliable {
		sort.Slice(observations, func(i, j int) bool { return observations[i] < observations[j] })
		middle := len(observations) / 2
		median := observations[middle]
		if len(observations)%2 == 0 {
			median = observations[middle-1] + (observations[middle]-observations[middle-1])/2
		}
		stats.SalaryMedian = &median
		for _, item := range items {
			if strings.TrimSpace(item.Currency) != stats.Currency || salaryObservation(item) <= 0 {
				continue
			}
			for _, boundary := range []*int64{item.SalaryFrom, item.SalaryTo} {
				if boundary == nil || *boundary <= 0 {
					continue
				}
				if stats.SalaryMin == nil || *boundary < *stats.SalaryMin {
					value := *boundary
					stats.SalaryMin = &value
				}
				if stats.SalaryMax == nil || *boundary > *stats.SalaryMax {
					value := *boundary
					stats.SalaryMax = &value
				}
			}
		}
	}
	stats.TopSkills = topSkillFrequencies(items, 8)
	stats.SmallSample = len(items) < 5 || !stats.SalaryReliable
	return stats
}

func salaryObservation(item Vacancy) int64 {
	switch {
	case item.SalaryFrom != nil && item.SalaryTo != nil && *item.SalaryFrom > 0 && *item.SalaryTo > 0:
		lower, upper := *item.SalaryFrom, *item.SalaryTo
		if lower > upper {
			return 0
		}
		return lower + (upper-lower)/2
	case item.SalaryFrom != nil && *item.SalaryFrom > 0:
		return *item.SalaryFrom
	case item.SalaryTo != nil && *item.SalaryTo > 0:
		return *item.SalaryTo
	default:
		return 0
	}
}

func dominantCurrency(counts map[string]int) string {
	result := ""
	maxCount := 0
	for currency, count := range counts {
		if count > maxCount || count == maxCount && currency < result {
			result, maxCount = currency, count
		}
	}
	return result
}

func isRemote(item Vacancy) bool {
	text := strings.ToLower(strings.Join([]string{item.Employment, item.Schedule, item.Location}, " "))
	return strings.Contains(text, "дистан") || strings.Contains(text, "удален") || strings.Contains(text, "удалён")
}

func percent(value, total int) int {
	if total == 0 {
		return 0
	}
	return (value*100 + total/2) / total
}
