package catalog

import (
	"regexp"
	"testing"
)

func TestNormalizeAndSearchHelpers(t *testing.T) {
	if got := normalizeSearch("Информационные  системы"); got != "информационныесистемы" {
		t.Fatalf("normalizeSearch: %q", got)
	}
}

func TestSearchRegions(t *testing.T) {
	got := SearchRegions("москва", 5)
	if len(got) == 0 || got[0].Short != "77" {
		t.Fatalf("expected Moscow, got %#v", got)
	}
	byCode := SearchRegions("50", 1)
	if len(byCode) != 1 || byCode[0].Name != "Московская область" {
		t.Fatalf("expected Moscow oblast, got %#v", byCode)
	}
}

func TestRegionsMatchSupportedTerritoryCountAndHaveUniqueCodes(t *testing.T) {
	t.Parallel()

	if len(Regions) != 91 {
		t.Fatalf("regions = %d, want 91 supported by the source catalog", len(Regions))
	}
	codePattern := regexp.MustCompile(`^[0-9]{13}$`)
	shortPattern := regexp.MustCompile(`^[0-9]{2}$`)
	codes := make(map[string]struct{}, len(Regions))
	shorts := make(map[string]struct{}, len(Regions))
	for _, region := range Regions {
		if !codePattern.MatchString(region.Code) || !shortPattern.MatchString(region.Short) || region.Name == "" {
			t.Errorf("invalid region entry: %+v", region)
		}
		if _, exists := codes[region.Code]; exists {
			t.Errorf("duplicate region code %q", region.Code)
		}
		if _, exists := shorts[region.Short]; exists {
			t.Errorf("duplicate callback short code %q", region.Short)
		}
		codes[region.Code] = struct{}{}
		shorts[region.Short] = struct{}{}
	}
	for _, code := range []string{
		"9000000000000", "9300000000000", "9400000000000",
		"9500000000000", "9800000000000", "9900000000000",
	} {
		if _, exists := codes[code]; !exists {
			t.Errorf("supported source territory %q is missing", code)
		}
	}
}
