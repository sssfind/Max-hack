package catalog

import (
	"path/filepath"
	"runtime"
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
