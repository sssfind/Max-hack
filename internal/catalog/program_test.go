package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func catalogPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "spo_program_vacancy_map.json")
}

func TestCatalogValidationAndPilotPolicy(t *testing.T) {
	t.Parallel()

	c, err := Load(catalogPath(t))
	if err != nil {
		t.Fatal(err)
	}
	report := c.ValidationReport()
	if report.ErrorCount() != 0 {
		t.Fatalf("validation errors = %#v", report.Issues)
	}
	if report.WarningCount() == 0 {
		t.Fatal("expected validation warnings for catalog entries awaiting curation")
	}
	if got, want := c.Metadata().UniqueProgramCodes, c.Len(); got != want {
		t.Errorf("metadata program count = %d, want %d", got, want)
	}
	if c.Metadata().CurrentProgramCodes == 0 || c.Metadata().ProgramsRequiringQueryReview == 0 {
		t.Errorf("catalog metadata counters were not decoded: %+v", c.Metadata())
	}
	pilot := c.PilotPrograms()
	if len(pilot) == 0 {
		t.Fatal("expected at least one reviewed current pilot program")
	}
	for _, program := range pilot {
		if !program.ProductionReady() {
			t.Errorf("PilotPrograms returned unready program %s", program.Code)
		}
	}
	if got := c.SearchWithPolicy("09.02.07", 5, ReviewPolicyPilot); len(got) != 0 {
		t.Errorf("historical program escaped pilot gate: %#v", got)
	}
	if got := c.SearchWithPolicy("09.02.11", 5, ReviewPolicyPilot); len(got) != 1 || got[0].Code != "09.02.11" {
		t.Errorf("reviewed pilot program missing: %#v", got)
	}
	if got := c.SearchWithPolicy("09.02.07", 5, ReviewPolicy("typo")); len(got) != 0 {
		t.Errorf("invalid policy failed open: %#v", got)
	}
	if got := c.SearchWithPolicy("09.02.07", 5, ReviewPolicyAll); len(got) != 1 {
		t.Errorf("explicit all policy unexpectedly hid program: %#v", got)
	}
}

func TestSourceDocumentUsesOnlyApprovedPOP(t *testing.T) {
	t.Parallel()

	approved := "утвержден"
	project := "проект"
	program := Program{ActiveStandard: ActiveStandard{
		FGOSStatus: "принят",
		FGOSURL:    "https://spolab.firpo.ru/fgos.pdf",
		POPStatus:  &project,
		POPURL:     stringPointer("https://spolab.firpo.ru/pop-draft.pdf"),
	}}
	if got := program.SourceDocument(); got.Kind != "ФГОС" || got.URL != program.ActiveStandard.FGOSURL {
		t.Errorf("draft POP source = %+v", got)
	}
	program.ActiveStandard.POPStatus = &approved
	if got := program.SourceDocument(); got.Kind != "ПОП" || got.URL != "https://spolab.firpo.ru/pop-draft.pdf" {
		t.Errorf("approved POP source = %+v", got)
	}
	unsafe := "http://example.test/pop.pdf"
	program.ActiveStandard.POPURL = &unsafe
	if got := program.SourceDocument(); got.Kind != "ФГОС" {
		t.Errorf("unsafe POP URL selected: %+v", got)
	}
	notApproved := "не утвержден"
	program.ActiveStandard.POPURL = stringPointer("https://spolab.firpo.ru/pop.pdf")
	program.ActiveStandard.POPStatus = &notApproved
	if got := program.SourceDocument(); got.Kind != "ФГОС" {
		t.Errorf("unapproved POP selected: %+v", got)
	}
	program.ActiveStandard.FGOSStatus = "!!! проект в разработке"
	if got := program.SourceDocument(); got.URL != "" {
		t.Errorf("draft FGOS selected: %+v", got)
	}
	if got := program.FGOSDocument(); got.URL != "" {
		t.Errorf("draft FGOS selected as fallback: %+v", got)
	}
}

func TestFederalDocumentSourceRejectsUntrustedURLsAndStatuses(t *testing.T) {
	t.Parallel()

	approved := "утвержден"
	for _, rawURL := range []string{
		"https://example.test/document.pdf",
		"https://user@spolab.firpo.ru/document.pdf",
		"https://spolab.firpo.ru:8443/document.pdf",
		"https://spolab.firpo.ru/" + strings.Repeat("x", 2048),
	} {
		program := Program{ActiveStandard: ActiveStandard{POPStatus: &approved, POPURL: stringPointer(rawURL)}}
		if got := program.SourceDocument(); got.URL != "" {
			t.Errorf("untrusted URL %q selected as federal source: %+v", rawURL, got)
		}
	}

	for _, status := range []string{"недействующий", "не действует", "утратил силу", "проект"} {
		program := Program{ActiveStandard: ActiveStandard{
			FGOSStatus: status,
			FGOSURL:    "https://spolab.firpo.ru/document.pdf",
		}}
		if got := program.SourceDocument(); got.URL != "" {
			t.Errorf("status %q selected as approved source: %+v", status, got)
		}
	}
}

func TestPilotPolicyRequiresEveryExposedRoleToBeCurated(t *testing.T) {
	t.Parallel()

	program := &Program{
		IsCurrent:      true,
		Qualifications: []string{"Квалификация"},
		ActiveStandard: ActiveStandard{FGOSStatus: "принят", FGOSURL: "https://spolab.firpo.ru/document.pdf"},
		WorkRoles: []WorkRole{
			{CanonicalTitle: "Проверенная роль", ReviewStatus: "reviewed_for_pilot", QueryConfidence: "curated", VacancyQueries: []string{"роль"}},
			{CanonicalTitle: "Непроверенная роль", ReviewStatus: "requires_manual_review", QueryConfidence: "medium", VacancyQueries: []string{"другая"}},
		},
	}
	if program.ProductionReady() {
		t.Fatal("mixed curated/unreviewed program passed the pilot gate")
	}
}

func TestValidationRequiresManualReviewForNonCuratedConfidence(t *testing.T) {
	t.Parallel()

	program := &Program{
		Code:                 "09.02.99",
		ProgramName:          "Тестовая программа",
		ProgramType:          "специальность",
		Qualifications:       []string{"Специалист"},
		VacancyQueries:       []string{"developer"},
		RequiresManualReview: false,
		WorkRoles: []WorkRole{{
			CanonicalTitle:  "Developer",
			SourceBasis:     "official_qualification",
			VacancyQueries:  []string{"developer"},
			ReviewStatus:    "reviewed_for_pilot",
			QueryConfidence: "medium",
		}},
	}
	catalog := &Catalog{
		programs: map[string]*Program{program.Code: program},
		ordered:  []*Program{program},
		metadata: CatalogMetadata{
			SchemaVersion:      "1.0.0",
			GeneratedOn:        "2026-09-28",
			SourceName:         "test",
			SourceRows:         1,
			UniqueProgramCodes: 1,
		},
	}
	report := catalog.validate()
	for _, issue := range report.Issues {
		if issue.Code == program.Code && issue.Field == "requires_manual_review" && issue.Severity == SeverityError {
			return
		}
	}
	t.Fatalf("missing requires_manual_review validation error: %#v", report.Issues)
}

func TestCatalogSchemaIsValidJSON(t *testing.T) {
	t.Parallel()

	schemaPath := filepath.Join(filepath.Dir(catalogPath(t)), "schemas", "spo_program_vacancy_map.schema.json")
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse JSON schema: %v", err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("unexpected schema dialect: %v", schema["$schema"])
	}
	if _, ok := schema["$defs"]; !ok {
		t.Error("schema has no reusable definitions")
	}
}

func stringPointer(value string) *string {
	return &value
}

func TestLoadAndSearch(t *testing.T) {
	c, err := Load(catalogPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() < 100 {
		t.Fatalf("expected many programs, got %d", c.Len())
	}

	byCode := c.Search("09.02.07", 5)
	if len(byCode) != 1 || byCode[0].ProgramName == "" {
		t.Fatalf("exact code search failed: %#v", byCode)
	}
	if len(byCode[0].Qualifications) < 2 {
		t.Fatalf("expected multi-qual program, got %#v", byCode[0].Qualifications)
	}

	byName := c.Search("информацион", 10)
	if len(byName) == 0 {
		t.Fatal("name search returned nothing")
	}

	roles := byCode[0].RolesForQualification(byCode[0].Qualifications[0])
	if len(roles) == 0 {
		t.Fatal("expected roles for qualification")
	}
}
