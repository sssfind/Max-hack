package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"Max-hack/internal/maxapi"
	"Max-hack/internal/pdfreport"
	"Max-hack/internal/skillgap"
	"Max-hack/internal/vacancies"
)

type immediateMarketAnalyzer struct {
	snapshot vacancies.Snapshot
	stats    vacancies.Statistics
}

func (a immediateMarketAnalyzer) Analyze(context.Context, vacancies.SearchRequest) (vacancies.Snapshot, vacancies.Statistics, error) {
	return a.snapshot, a.stats, nil
}

type fakeProgramAnalyzer struct {
	requests chan skillgap.Request
	result   skillgap.Result
}

func (a *fakeProgramAnalyzer) Analyze(_ context.Context, request skillgap.Request) (skillgap.Result, error) {
	a.requests <- request
	return a.result, nil
}

type capturingReporter struct {
	inputs chan pdfreport.Input
}

func (r *capturingReporter) Generate(_ context.Context, input pdfreport.Input) (pdfreport.Document, error) {
	r.inputs <- input
	return pdfreport.Document{Filename: "skillgap.pdf", MediaType: "application/pdf", Data: []byte("%PDF-test")}, nil
}

func TestAnalysisIncludesDocumentBackedSkillGap(t *testing.T) {
	cat := loadTestCatalog(t)
	program, ok := cat.Get("09.02.11")
	if !ok || len(program.Qualifications) == 0 {
		t.Fatal("pilot program is unavailable")
	}
	messenger := newFakeMessenger()
	salary := int64(90_000)
	market := immediateMarketAnalyzer{
		snapshot: vacancies.Snapshot{
			Vacancies: []vacancies.Vacancy{{ID: "v1", Title: "Программист", Employer: "Компания", SalaryFrom: &salary, Currency: "RUB", URL: "https://trudvsem.ru/vacancy/card/company/v1"}},
			Source:    "Работа России", FetchedAt: time.Now(), Mode: vacancies.ModeLive,
		},
		stats: vacancies.Statistics{
			VacancyCount: 1, EmployerCount: 1,
			TopSkills: []vacancies.SkillFrequency{{Name: "SQL", Count: 1, Percent: 100}},
		},
	}
	programAnalyzer := &fakeProgramAnalyzer{
		requests: make(chan skillgap.Request, 1),
		result: skillgap.Result{
			Source: skillgap.SourceMetadata{URL: program.ActiveStandard.FGOSURL, Type: skillgap.DocumentFGOS, FetchedAt: time.Now(), SHA256: strings.Repeat("a", 64)},
			Matches: []skillgap.SkillMatch{{
				Skill: "SQL", Status: skillgap.StatusFound,
				Evidence: []skillgap.Evidence{{Excerpt: "Проектирование баз данных", Page: 12}},
			}},
		},
	}
	reporter := &capturingReporter{inputs: make(chan pdfreport.Input, 1)}
	h := NewHandler(messenger, cat, market, reporter, AnalysisConfig{Workers: 1, Queue: 2, Timeout: 5 * time.Second, SkillGap: programAnalyzer})
	h.sessions.set(101, session{
		Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва",
		ProgramCode: program.Code, Qualification: program.Qualifications[0],
	})

	ctx, cancel := context.WithCancel(context.Background())
	h.StartAnalysisWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		h.WaitAnalysisWorkers()
	})
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCallback,
		ChatID:     101,
		Callback: &maxapi.Callback{
			CallbackID: "skill-gap",
			Payload:    payloadAnalyze + "0:" + program.Code,
			User:       maxapi.User{UserID: 11},
		},
	}
	h.onCallback(t.Context(), update)
	receive(t, messenger.answers)

	request := receive(t, programAnalyzer.requests)
	if request.Primary.URL == "" || request.Fallback == nil || len(request.Skills) != 1 || request.Skills[0].Name != "SQL" {
		t.Fatalf("unexpected document analysis request: %+v", request)
	}
	message := receive(t, messenger.messages)
	if message.body.Text == nil || !strings.Contains(*message.body.Text, "Skill Gap по федеральному документу") || !strings.Contains(*message.body.Text, "SQL — Найдено") {
		t.Fatalf("statistics do not contain Skill Gap: %#v", message.body.Text)
	}
	reportInput := receive(t, reporter.inputs)
	if reportInput.SkillGap == nil || len(reportInput.SkillGap.Matches) != 1 {
		t.Fatalf("PDF input does not contain Skill Gap: %+v", reportInput.SkillGap)
	}
	receive(t, messenger.files)
}
