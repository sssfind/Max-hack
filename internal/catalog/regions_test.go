package catalog

import "testing"

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
