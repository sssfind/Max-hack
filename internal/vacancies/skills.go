package vacancies

import (
	"regexp"
	"sort"
	"strings"
)

type skillPattern struct {
	name    string
	markers []*regexp.Regexp
}

var textSkillPatterns = []skillPattern{
	newSkillPattern("1С", []string{"1с", "1c"}, nil),
	newSkillPattern("Microsoft Excel", []string{"excel", "эксель"}, nil),
	newSkillPattern("SQL", []string{"sql", "postgresql", "mysql"}, nil),
	newSkillPattern("Python", []string{"python"}, nil),
	newSkillPattern("Java", []string{"java"}, nil),
	newSkillPattern("JavaScript", []string{"javascript", "react", "vue.js", "node.js"}, nil),
	newSkillPattern("C++", []string{"c++"}, nil),
	newSkillPattern("C#", []string{"c#", ".net"}, nil),
	newSkillPattern("Go", []string{"go", "golang"}, nil),
	newSkillPattern("Git", []string{"git"}, nil),
	newSkillPattern("Linux", []string{"linux"}, nil),
	newSkillPattern("Docker", []string{"docker"}, nil),
	newSkillPattern("Kubernetes", []string{"kubernetes", "k8s"}, nil),
	newSkillPattern("HTML/CSS", []string{"html", "css"}, nil),
	newSkillPattern("AutoCAD", []string{"autocad", "автокад"}, nil),
	newSkillPattern("КОМПАС-3D", []string{"компас-3d", "компас 3d"}, nil),
	newSkillPattern("САПР", []string{"сапр", "cad"}, nil),
	newSkillPattern("Чтение чертежей", nil, []string{"чтение чертеж", "читать чертеж"}),
	newSkillPattern("Работа на станках с ЧПУ", []string{"чпу", "cnc"}, nil),
	newSkillPattern("Сварочные работы", nil, []string{"сварк", "свароч"}),
	newSkillPattern("Электробезопасность", nil, []string{"электробезопас"}),
	newSkillPattern("Охрана труда", []string{"охрана труда", "охраны труда"}, nil),
	newSkillPattern("Бухгалтерский учет", []string{"бухучет", "бухучёт"}, []string{"бухгалтерск"}),
	newSkillPattern("Делопроизводство", nil, []string{"делопроизвод"}),
	newSkillPattern("Продажи", nil, []string{"продаж"}),
	newSkillPattern("Работа с клиентами", []string{"клиентский сервис"}, []string{"работа с клиент"}),
	newSkillPattern("Английский язык", []string{"english"}, []string{"английск"}),
	newSkillPattern("Командная работа", []string{"командная работа", "работа в команде"}, nil),
	newSkillPattern("Коммуникация", nil, []string{"коммуникабель", "коммуникатив"}),
	newSkillPattern("Медицинская документация", nil, []string{"медицинской документац", "медицинскую документац"}),
	newSkillPattern("ГОСТ", []string{"гост"}, nil),
}

func newSkillPattern(name string, exact, prefixes []string) skillPattern {
	markers := make([]*regexp.Regexp, 0, len(exact)+len(prefixes))
	for _, marker := range exact {
		markers = append(markers, compileSkillMarker(marker, false))
	}
	for _, marker := range prefixes {
		markers = append(markers, compileSkillMarker(marker, true))
	}
	return skillPattern{name: name, markers: markers}
}

func compileSkillMarker(marker string, prefix bool) *regexp.Regexp {
	expression := `(?i)(^|[^\p{L}\p{N}])` + regexp.QuoteMeta(strings.TrimSpace(marker))
	if prefix {
		expression += `[\p{L}\p{N}]*`
	} else {
		expression += `($|[^\p{L}\p{N}])`
	}
	return regexp.MustCompile(expression)
}

func collectSkills(structured []string, text string) []Skill {
	result := make([]Skill, 0, len(structured)+4)
	seen := make(map[string]struct{}, len(structured)+4)
	for _, value := range structured {
		name := cleanText(value)
		key := normalizeSkillKey(name)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, Skill{Name: name, Evidence: "структурированное поле вакансии"})
	}

	normalizedText := cleanText(text)
	for _, pattern := range textSkillPatterns {
		matched := false
		for _, marker := range pattern.markers {
			if marker.MatchString(normalizedText) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		key := normalizeSkillKey(pattern.name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, Skill{Name: pattern.name, Evidence: "из текста требований"})
	}
	return result
}

func mergeSkills(left, right []Skill) []Skill {
	result := append([]Skill(nil), left...)
	seen := make(map[string]struct{}, len(left)+len(right))
	for _, skill := range left {
		seen[normalizeSkillKey(skill.Name)] = struct{}{}
	}
	for _, skill := range right {
		key := normalizeSkillKey(skill.Name)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, skill)
	}
	return result
}

func normalizeSkillKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func topSkillFrequencies(items []Vacancy, limit int) []SkillFrequency {
	type counter struct {
		name  string
		count int
	}
	counts := make(map[string]*counter)
	for _, item := range items {
		seenInVacancy := make(map[string]struct{}, len(item.Skills))
		for _, skill := range item.Skills {
			key := normalizeSkillKey(skill.Name)
			if key == "" {
				continue
			}
			if _, seen := seenInVacancy[key]; seen {
				continue
			}
			seenInVacancy[key] = struct{}{}
			if counts[key] == nil {
				counts[key] = &counter{name: skill.Name}
			}
			counts[key].count++
		}
	}
	ordered := make([]counter, 0, len(counts))
	for _, item := range counts {
		ordered = append(ordered, *item)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].name < ordered[j].name
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	result := make([]SkillFrequency, 0, len(ordered))
	for _, item := range ordered {
		result = append(result, SkillFrequency{
			Name:    item.name,
			Count:   item.count,
			Percent: percent(item.count, len(items)),
		})
	}
	return result
}
