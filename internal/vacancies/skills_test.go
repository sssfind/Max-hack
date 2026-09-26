package vacancies

import "testing"

func TestCollectSkillsDeduplicatesStructuredAndExtractedEvidence(t *testing.T) {
	t.Parallel()

	got := collectSkills(
		[]string{" SQL ", "sql", "<b>Командная</b> работа", "", "  "},
		"Нужны SQL, Python, Docker, Linux, Git, английский язык и работа в команде.",
	)
	byName := make(map[string]Skill, len(got))
	for _, skill := range got {
		if _, exists := byName[skill.Name]; exists {
			t.Errorf("duplicate exact skill %q in %#v", skill.Name, got)
		}
		byName[skill.Name] = skill
	}
	for _, name := range []string{"SQL", "Командная работа", "Python", "Docker", "Linux", "Git", "Английский язык"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing skill %q in %#v", name, got)
		}
	}
	if len(got) != 7 {
		t.Fatalf("collectSkills returned %d skills, want 7: %#v", len(got), got)
	}
	if byName["SQL"].Evidence != "структурированное поле вакансии" || byName["Командная работа"].Evidence != "структурированное поле вакансии" {
		t.Errorf("structured evidence lost: %#v", got)
	}
	if byName["Python"].Evidence != "из текста требований" {
		t.Errorf("extracted evidence = %q", byName["Python"].Evidence)
	}
}

func TestCollectSkillsRecognizesDomainPatterns(t *testing.T) {
	t.Parallel()

	text := "1С, Excel, PostgreSQL, JavaScript, C++, C#, golang, Kubernetes, HTML/CSS, AutoCAD, КОМПАС-3D, САПР, " +
		"чтение чертежей, ЧПУ, сварочные работы, электробезопасность, охрана труда, " +
		"бухучет, делопроизводство, продажи, работа с клиентами, коммуникабельность, ГОСТ"
	got := collectSkills(nil, text)
	names := make(map[string]bool, len(got))
	for _, skill := range got {
		names[skill.Name] = true
	}
	want := []string{
		"1С", "Microsoft Excel", "SQL", "JavaScript", "C++", "C#", "Go", "Kubernetes", "HTML/CSS",
		"AutoCAD", "КОМПАС-3D", "САПР", "Чтение чертежей", "Работа на станках с ЧПУ", "Сварочные работы",
		"Электробезопасность", "Охрана труда", "Бухгалтерский учет", "Делопроизводство", "Продажи", "Работа с клиентами", "Коммуникация", "ГОСТ",
	}
	for _, name := range want {
		if !names[name] {
			t.Errorf("pattern did not produce %q; got %#v", name, got)
		}
	}
}

func TestCollectSkillsUsesWordBoundaries(t *testing.T) {
	t.Parallel()

	got := collectSkills(nil, "excellent academic гостиница gopher javascript")
	for _, skill := range got {
		switch skill.Name {
		case "Microsoft Excel", "САПР", "ГОСТ", "Go", "Java":
			t.Errorf("false positive skill %q extracted from unrelated words: %#v", skill.Name, got)
		}
	}
	if len(got) != 1 || got[0].Name != "JavaScript" {
		t.Errorf("boundary result = %#v, want only JavaScript", got)
	}
}

func TestMergeSkillsPreservesFirstEvidenceAndOrder(t *testing.T) {
	t.Parallel()

	left := []Skill{{Name: "Go", Evidence: "structured"}, {Name: "SQL", Evidence: "left"}}
	right := []Skill{{Name: " go ", Evidence: "right duplicate"}, {Name: "Python", Evidence: "right"}, {Name: "", Evidence: "empty"}}
	got := mergeSkills(left, right)
	want := []Skill{{Name: "Go", Evidence: "structured"}, {Name: "SQL", Evidence: "left"}, {Name: "Python", Evidence: "right"}}
	if len(got) != len(want) {
		t.Fatalf("mergeSkills = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mergeSkills[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if left[0].Evidence != "structured" || len(left) != 2 {
		t.Errorf("mergeSkills mutated left input: %#v", left)
	}
}

func TestTopSkillFrequenciesCountsOncePerVacancyAndLimits(t *testing.T) {
	t.Parallel()

	items := []Vacancy{
		{Skills: []Skill{{Name: "Go"}, {Name: "go"}, {Name: "SQL"}}},
		{Skills: []Skill{{Name: " Go "}, {Name: "Python"}}},
		{Skills: []Skill{{Name: "SQL"}, {Name: "Python"}}},
		{Skills: []Skill{{Name: ""}}},
	}
	got := topSkillFrequencies(items, 2)
	want := []SkillFrequency{
		{Name: "Go", Count: 2, Percent: 50},
		{Name: "Python", Count: 2, Percent: 50},
	}
	if len(got) != len(want) {
		t.Fatalf("topSkillFrequencies = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("topSkillFrequencies[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got := topSkillFrequencies(nil, 8); len(got) != 0 {
		t.Errorf("topSkillFrequencies(nil) = %#v", got)
	}
}

func TestNormalizeSkillKey(t *testing.T) {
	t.Parallel()

	if got := normalizeSkillKey("  Microsoft   SQL\tServer "); got != "microsoft sql server" {
		t.Errorf("normalizeSkillKey = %q", got)
	}
	if got := normalizeSkillKey("   "); got != "" {
		t.Errorf("normalizeSkillKey(blank) = %q", got)
	}
}
