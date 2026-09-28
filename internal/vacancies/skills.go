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
	newSkillPattern("CRM", []string{"crm", "битрикс24", "amoCRM"}, nil),
	newSkillPattern("Adobe Photoshop", []string{"photoshop", "фотошоп"}, nil),
	newSkillPattern("Figma", []string{"figma"}, nil),
	newSkillPattern("BIM/Revit", []string{"bim", "revit"}, nil),
	newSkillPattern("Электромонтаж", nil, []string{"электромонтаж"}),
	newSkillPattern("Техническое обслуживание оборудования", nil, []string{"техническое обслуживание оборудован"}),
	newSkillPattern("Ремонт оборудования", nil, []string{"ремонт оборудован"}),
	newSkillPattern("Диагностика оборудования", nil, []string{"диагностик оборудован"}),
	newSkillPattern("Измерительные инструменты", nil, []string{"измерительн инструмент", "контрольно измерительн"}),
	newSkillPattern("Складской учет", []string{"складской учет", "складской учёт"}, nil),
	newSkillPattern("Логистика", nil, []string{"логистик"}),
	newSkillPattern("Кассовые операции", nil, []string{"кассовых операц", "кассовые операц"}),
	newSkillPattern("Налоговая отчетность", nil, []string{"налоговой отчетност", "налоговой отчётност"}),
	newSkillPattern("Первая помощь", []string{"первая помощь", "первой помощи"}, nil),
	newSkillPattern("Сестринский уход", nil, []string{"сестринск уход"}),
	newSkillPattern("Санитарные нормы", nil, []string{"санитарн норм", "санпин"}),
	newSkillPattern("Приготовление блюд", nil, []string{"приготовлен блюд", "приготовлен пищ"}),
	newSkillPattern("Контроль качества", []string{"контроль качества"}, nil),
	newSkillPattern("Проектная документация", nil, []string{"проектной документац", "проектную документац"}),
	newSkillPattern("Документооборот", nil, []string{"документооборот"}),
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
	marker = strings.TrimSpace(marker)
	expression := `(?i)(^|[^\p{L}\p{N}])`
	if prefix {
		parts := strings.Fields(marker)
		for index, part := range parts {
			if index > 0 {
				expression += `\s+`
			}
			expression += regexp.QuoteMeta(part) + `[\p{L}\p{N}]*`
		}
	} else {
		expression += regexp.QuoteMeta(marker)
		expression += `($|[^\p{L}\p{N}])`
	}
	return regexp.MustCompile(expression)
}

func collectSkills(structured []string, text string) []Skill {
	return collectSkillsFromFields(structured, "", text, "")
}

type skillTextField struct {
	name  string
	value string
}

func collectSkillsFromFields(structured []string, qualification, requirements, duties string) []Skill {
	capacity := min(len(structured)+4, maxVacancySkills)
	result := make([]Skill, 0, capacity)
	seen := make(map[string]struct{}, capacity)
	for _, value := range structured {
		if len(result) >= maxVacancySkills {
			break
		}
		rawName := cleanTextBounded(value, maxSkillNameBytes)
		name := canonicalSkillName(rawName)
		name = truncateUTF8Bytes(name, maxSkillNameBytes)
		key := normalizeSkillKey(name)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, Skill{
			Name:            name,
			Evidence:        "структурированное поле вакансии",
			EvidenceField:   "skills",
			EvidenceExcerpt: rawName,
		})
	}

	fields := []skillTextField{
		{name: "qualification", value: cleanTextBounded(qualification, maxDescriptionBytes)},
		{name: "requirements", value: cleanTextBounded(requirements, maxDescriptionBytes)},
		{name: "duties", value: cleanTextBounded(duties, maxDescriptionBytes)},
	}
	for _, pattern := range textSkillPatterns {
		if len(result) >= maxVacancySkills {
			break
		}
		var evidenceField, evidenceExcerpt string
		for _, field := range fields {
			for _, marker := range pattern.markers {
				location := marker.FindStringIndex(field.value)
				if location == nil {
					continue
				}
				evidenceField = field.name
				evidenceExcerpt = evidenceWindow(field.value, location[0], location[1])
				break
			}
			if evidenceField != "" {
				break
			}
		}
		if evidenceField == "" {
			continue
		}
		key := normalizeSkillKey(pattern.name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, Skill{
			Name:            pattern.name,
			Evidence:        "из текста требований",
			EvidenceField:   evidenceField,
			EvidenceExcerpt: evidenceExcerpt,
		})
	}
	return result
}

func evidenceWindow(text string, start, end int) string {
	if text == "" {
		return ""
	}
	runes := []rune(text)
	startRune := len([]rune(text[:max(0, start)]))
	endRune := len([]rune(text[:min(len(text), end)]))
	const contextRunes = 45
	from := max(0, startRune-contextRunes)
	to := min(len(runes), endRune+contextRunes)
	excerpt := strings.TrimSpace(string(runes[from:to]))
	if from > 0 {
		excerpt = "…" + excerpt
	}
	if to < len(runes) {
		excerpt += "…"
	}
	return excerpt
}

func canonicalSkillName(value string) string {
	value = cleanText(value)
	key := rawSkillKey(value)
	switch key {
	case "1c", "1с", "1с предприятие", "1c enterprise":
		return "1С"
	case "excel", "эксель", "ms excel", "microsoft excel":
		return "Microsoft Excel"
	case "postgres", "postgresql", "mysql", "ms sql", "mssql", "microsoft sql server", "sql":
		return "SQL"
	case "golang", "go":
		return "Go"
	case "js", "javascript", "node js", "node.js":
		return "JavaScript"
	case "gitlab", "github", "git":
		return "Git"
	case "k8s", "kubernetes":
		return "Kubernetes"
	case "английский", "английский язык", "english":
		return "Английский язык"
	case "работа в команде", "командная работа":
		return "Командная работа"
	case "crm", "amocrm", "битрикс24", "bitrix24":
		return "CRM"
	case "photoshop", "adobe photoshop", "фотошоп":
		return "Adobe Photoshop"
	case "figma":
		return "Figma"
	case "autocad", "автокад":
		return "AutoCAD"
	default:
		return value
	}
}

func mergeSkills(left, right []Skill) []Skill {
	if len(left) > maxVacancySkills {
		left = left[:maxVacancySkills]
	}
	result := append([]Skill(nil), left...)
	positions := make(map[string]int, len(left)+len(right))
	for index, skill := range left {
		positions[normalizeSkillKey(skill.Name)] = index
	}
	for _, skill := range right {
		key := normalizeSkillKey(skill.Name)
		if key == "" {
			continue
		}
		if position, exists := positions[key]; exists {
			if result[position].EvidenceField != "skills" && skill.EvidenceField == "skills" {
				result[position] = skill
			}
			continue
		}
		if len(result) >= maxVacancySkills {
			continue
		}
		positions[key] = len(result)
		result = append(result, skill)
	}
	return result
}

func normalizeSkillKey(value string) string {
	return rawSkillKey(canonicalSkillName(value))
}

func rawSkillKey(value string) string {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	value = strings.NewReplacer("ё", "е", "–", "-", "—", "-").Replace(value)
	return value
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
